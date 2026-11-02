package postgres_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCorePolicyImportPreservesApprovalsPointersAndBudgetHistory(t *testing.T) {
	db := template.Database(t)
	repository, _ := repositoryFor(t, db, smallPool())
	organizations := []tracker.OrganizationID{"org_policy_import_first", "org_policy_import_second"}
	for _, organization := range organizations {
		seedTenantAccess(t, repository, organization)
		seedLeaseFamily(t, repository, organization)
	}
	now := time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)
	for _, organization := range organizations {
		policyID := importedCurrentPolicyDescriptor().ID
		source := postgres.CorePolicyImportSource{CopyPath: corePolicyCopy(t, organization, now), OrganizationID: organization, Objects: newHistoryObjects()}
		for range 2 {
			result, err := repository.ImportCorePolicies(t.Context(), source)
			if err != nil {
				t.Fatalf("organization %s import: %v", organization, err)
			}
			if result != (postgres.CorePolicyImportResult{PolicyRevisions: 2, CurrentPolicies: 1, ReviewPolicies: 1, MonthlyPolicies: 3}) {
				t.Fatalf("organization %s import counts = %+v", organization, result)
			}
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			current, err := reader.ProjectPolicy(t.Context(), "prj_1")
			if err != nil {
				return err
			}
			if current.PolicyID != policyID || current.ApprovedBy != "operator" || current.GitHubPullRequest == nil || !*current.GitHubPullRequest {
				t.Errorf("organization %s current policy = %+v", organization, current)
			}
			old, err := reader.ProjectPolicyRevision(t.Context(), "prj_1", importedReportedPolicyDescriptor().ID)
			if err != nil {
				return err
			}
			if old.ApprovedBy != "runner-report" {
				t.Errorf("organization %s historical policy approval = %+v", organization, old)
			}
			review, err := reader.ReviewPolicy(t.Context(), "prj_1")
			if err != nil {
				return err
			}
			if review.PolicyRevisionID != policyID || review.UpdatedBy != "production-import" || !review.UpdatedAt.Equal(time.Unix(0, 0).UTC()) {
				t.Errorf("organization %s review policy provenance = %+v", organization, review)
			}
			reviewValue, err := eventstore.NewContent(source.Objects).ReviewPolicy(t.Context(), review)
			if err != nil {
				return err
			}
			if reviewValue.ID != review.ReviewPolicyID || reviewValue.PolicyID != policyID || !reviewValue.RequireReview {
				t.Errorf("organization %s review policy payload = %+v", organization, reviewValue)
			}
			receipt, err := reader.Receipt(t.Context(), "production-import", "core-review-policy-source", "prj_1")
			if err != nil {
				return err
			}
			receiptValue, err := eventstore.NewContent(source.Objects).Receipt(t.Context(), receipt)
			if err != nil {
				return err
			}
			var sourceValue struct {
				PolicyJSON string `json:"policy_json"`
			}
			if err := json.Unmarshal(receiptValue, &sourceValue); err != nil {
				return err
			}
			var sourceMetadata map[string]json.RawMessage
			if err := json.Unmarshal([]byte(sourceValue.PolicyJSON), &sourceMetadata); err != nil {
				return err
			}
			if sourceValue.PolicyJSON != legacyReviewPolicyJSON(policyID) || sourceMetadata["updated_at"] != nil || sourceMetadata["updated_by"] != nil {
				t.Errorf("organization %s source review policy receipt = %s", organization, receiptValue)
			}
			budgetPolicy, err := reader.BillingMonthlyPolicy(t.Context(), "prj_1")
			if err != nil {
				return err
			}
			if budgetPolicy.Revision != 1 || !budgetPolicy.Policy.Enabled || budgetPolicy.Policy.EffectiveMode() != budget.HardStop || budgetPolicy.Policy.RunnerMicros == nil || *budgetPolicy.Policy.RunnerMicros != 300 {
				t.Errorf("organization %s current monthly policy = %+v", organization, budgetPolicy)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var unapproved, approved int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM policy_revision_approvals WHERE organization_id=$1 AND project_id='prj_1' AND policy_id=$2`, organization, importedReportedPolicyDescriptor().ID).Scan(&unapproved); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM policy_revision_approvals WHERE organization_id=$1 AND project_id='prj_1' AND policy_id=$2`, organization, policyID).Scan(&approved); err != nil {
			t.Fatal(err)
		}
		if unapproved != 0 || approved != 1 {
			t.Fatalf("organization %s approval rows reported=%d current=%d", organization, unapproved, approved)
		}
		var budgetRows int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM monthly_budget_policies WHERE organization_id=$1`, organization).Scan(&budgetRows); err != nil {
			t.Fatal(err)
		}
		if budgetRows != 3 {
			t.Fatalf("organization %s monthly budget rows = %d, want 3", organization, budgetRows)
		}
		claimLease(t, repository, organization, leaseClaim(1))
		for range 2 {
			facts, err := repository.ImportCoreMonthlyBudgetFacts(t.Context(), source)
			if err != nil || facts != (postgres.CoreMonthlyBudgetFactsResult{Admissions: 1, Leases: 1}) {
				t.Fatalf("organization %s budget facts = %+v, error=%v", organization, facts, err)
			}
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			admission, err := reader.BillingMonthlyAdmission(t.Context(), 1)
			if err != nil {
				return err
			}
			if !admission.AdmittedAt.Equal(now.Add(-time.Minute)) {
				t.Errorf("organization %s admission = %+v", organization, admission)
			}
			budgetLease, err := reader.BillingMonthlyLease(t.Context(), "lease_1")
			if err != nil {
				return err
			}
			if !budgetLease.LunaAPI || budgetLease.RunnerAPI || !budgetLease.SpriteInfrastructure {
				t.Errorf("organization %s budget lease = %+v", organization, budgetLease)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE monthly_budget_admissions SET admitted_at=$1 WHERE organization_id=$2 AND issue_id=1`, now.Add(-2*time.Minute), organization); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ImportCoreMonthlyBudgetFacts(t.Context(), source); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("changed imported admission timestamp error = %v, want revision conflict", err)
		}
	}
}

func corePolicyCopy(t *testing.T, organization tracker.OrganizationID, now time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policies.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE policy_revisions(scope TEXT,policy_id TEXT,metadata_json TEXT,approved_by TEXT,approved_at TEXT,gate_github_pull_request INTEGER,PRIMARY KEY(scope,policy_id))`,
		`CREATE TABLE project_policies(scope TEXT PRIMARY KEY,policy_id TEXT)`,
		`CREATE TABLE monthly_budget_policies(organization_id TEXT,project_id TEXT,revision INTEGER,policy_json TEXT,actor_id TEXT,updated_at TEXT,PRIMARY KEY(organization_id,project_id,revision))`,
		`CREATE TABLE issues(id INTEGER PRIMARY KEY,organization_id TEXT NOT NULL)`,
		`CREATE TABLE leases(lease_id TEXT PRIMARY KEY,issue_id INTEGER NOT NULL REFERENCES issues(id))`,
		`CREATE TABLE monthly_budget_admissions(issue_id INTEGER PRIMARY KEY,admitted_at TEXT)`,
		`CREATE TABLE monthly_budget_leases(lease_id TEXT PRIMARY KEY,exposure_json TEXT)`,
		`CREATE TABLE change_review_policies(organization_id TEXT,project_id TEXT,policy_json TEXT,PRIMARY KEY(organization_id,project_id))`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	old := importedReportedPolicyDescriptor()
	current := importedCurrentPolicyDescriptor()
	for _, row := range []struct {
		descriptor policy.Descriptor
		actor      string
	}{
		{descriptor: old, actor: "runner-report"},
		{descriptor: current, actor: "operator"},
	} {
		encoded, err := json.Marshal(row.descriptor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO policy_revisions(scope,policy_id,metadata_json,approved_by,approved_at,gate_github_pull_request) VALUES(?,?,?,?,?,?)`, string(organization)+"/prj_1", row.descriptor.ID, string(encoded), row.actor, now.Format(time.RFC3339Nano), row.descriptor.Gates.GitHubPullRequest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO project_policies(scope,policy_id) VALUES(?,?)`, string(organization)+"/prj_1", importedCurrentPolicyDescriptor().ID); err != nil {
		t.Fatal(err)
	}
	reviewJSON := legacyReviewPolicyJSON(importedCurrentPolicyDescriptor().ID)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO change_review_policies(organization_id,project_id,policy_json) VALUES(?,?,?)`, organization, "prj_1", reviewJSON); err != nil {
		t.Fatal(err)
	}
	orgPolicy, _ := json.Marshal(budget.MonthlyPolicy{Enabled: true, Mode: budget.DrainIssue, TotalMicros: new(int64)})
	orgPolicyTwo, _ := json.Marshal(budget.MonthlyPolicy{Enabled: true, Mode: budget.DrainIssue, TotalMicros: int64ptr(1000)})
	projectPolicy, _ := json.Marshal(budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, RunnerMicros: int64ptr(300)})
	for _, row := range []struct {
		project  string
		revision int64
		policy   []byte
	}{
		{project: "", revision: 1, policy: orgPolicy},
		{project: "", revision: 2, policy: orgPolicyTwo},
		{project: "prj_1", revision: 1, policy: projectPolicy},
	} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO monthly_budget_policies(organization_id,project_id,revision,policy_json,actor_id,updated_at) VALUES(?,?,?,?,?,?)`, organization, row.project, row.revision, string(row.policy), "admin", now.Add(time.Duration(row.revision)*time.Second).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO issues(id,organization_id) VALUES(1,?),(2,?)`, organization, organization); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id) VALUES('lease_1',1)`); err != nil {
		t.Fatal(err)
	}
	exposure, _ := json.Marshal(budget.CostExposure{LunaAPI: true, SpriteInfrastructure: true})
	if _, err := db.ExecContext(t.Context(), `INSERT INTO monthly_budget_admissions(issue_id,admitted_at) VALUES(1,?)`, now.Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO monthly_budget_leases(lease_id,exposure_json) VALUES('lease_1',?)`, string(exposure)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	return path
}

func legacyReviewPolicyJSON(policyID string) string {
	rules := tracker.ChangeReviewPolicy{PolicyID: policyID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
	rules.ID = changerequest.PolicyID(rules)
	encoded, _ := json.Marshal(rules)
	return string(encoded)
}

func importedCurrentPolicyDescriptor() policy.Descriptor {
	return policy.Descriptor{SourceRevision: "abcdef0123456789abcdef0123456789abcdef01", SourceDigest: policy.Digest([]byte("source-current")), ConfigDigest: policy.Digest([]byte("config-current")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("review-current")), AutomatedReview: "optional", MergeMethod: "squash", GitHubPullRequest: true}}.WithID()
}

func importedReportedPolicyDescriptor() policy.Descriptor {
	return policy.Descriptor{SourceRevision: "0123456789abcdef0123456789abcdef01234567", SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("review")), AutomatedReview: "optional", MergeMethod: "squash"}}.WithID()
}

func int64ptr(value int64) *int64 { return &value }
