package postgres_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
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
		source := postgres.CorePolicyImportSource{BundlePath: corePolicyCopy(t, organization, now), OrganizationID: organization, Objects: newHistoryObjects()}
		for range 2 {
			result, err := repository.ImportCorePolicies(t.Context(), source)
			if err != nil {
				t.Fatalf("organization %s import: %v", organization, err)
			}
			if result != (postgres.CorePolicyImportResult{PolicyRevisions: 4, CurrentPolicies: 2, ReviewPolicies: 2, MonthlyPolicies: 4}) {
				t.Fatalf("organization %s import counts = %+v", organization, result)
			}
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		repository, _ = repositoryFor(t, db, smallPool())
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			current, err := reader.ProjectPolicy(t.Context(), "prj_2")
			if err != nil {
				return err
			}
			if current.PolicyID != policyID || current.ApprovedBy != "operator" {
				t.Fatalf("second project current policy=%+v", current)
			}
			review, err := reader.ReviewPolicy(t.Context(), "prj_2")
			if err != nil {
				return err
			}
			reviewValue, err := eventstore.NewContent(source.Objects).ReviewPolicy(t.Context(), review)
			if err != nil {
				return err
			}
			if review.PolicyRevisionID != policyID || reviewValue.PolicyID != policyID || !reviewValue.RequireReview {
				t.Fatalf("second project review policy=%+v", review)
			}
			monthly, err := reader.BillingMonthlyPolicy(t.Context(), "prj_2")
			if err != nil {
				return err
			}
			if monthly.Policy.RunnerMicros == nil || *monthly.Policy.RunnerMicros != 300 {
				t.Fatalf("second project monthly policy=%+v", monthly)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		reported := importedReportedPolicyDescriptor()
		reportedRaw, err := json.Marshal(reported)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := eventstore.NewContent(source.Objects).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: organization}, reportedRaw)
		if err != nil {
			t.Fatal(err)
		}
		observation := policystore.Observation{
			OrganizationID: organization,
			ProjectID:      "prj_1",
			RunnerID:       "runner_example",
			PolicyID:       reported.ID,
			ObservedAt:     now.Add(time.Minute),
			Revision: policystore.Revision{
				OrganizationID:      organization,
				ProjectID:           "prj_1",
				PolicyID:            reported.ID,
				GitHubPullRequest:   new(reported.Gates.GitHubPullRequest),
				PayloadObjectKey:    receipt.PayloadObjectKey,
				PayloadSHA256:       receipt.PayloadSHA256,
				PayloadBytes:        receipt.PayloadBytes,
				PayloadContentBytes: receipt.PayloadContentBytes,
			},
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			_, err := writer.RecordProjectPolicyObservation(t.Context(), observation)
			return err
		}); err != nil {
			t.Fatalf("organization %s report imported policy observation: %v", organization, err)
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
		if budgetRows != 4 {
			t.Fatalf("organization %s monthly budget rows = %d, want 4", organization, budgetRows)
		}
		claimLease(t, repository, organization, leaseClaim(1))
		claimLease(t, repository, organization, leaseClaim(2))
		for range 2 {
			facts, err := repository.ImportCoreMonthlyBudgetFacts(t.Context(), source)
			if err != nil || facts != (postgres.CoreMonthlyBudgetFactsResult{Admissions: 2, Leases: 2}) {
				t.Fatalf("organization %s budget facts = %+v, error=%v", organization, facts, err)
			}
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		repository, _ = repositoryFor(t, db, smallPool())
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			second, err := reader.BillingMonthlyAdmission(t.Context(), 2)
			if err != nil {
				return err
			}
			if !second.AdmittedAt.Equal(now.Add(-time.Minute)) {
				t.Fatalf("second admission=%+v", second)
			}
			secondLease, err := reader.BillingMonthlyLease(t.Context(), "lease_2")
			if err != nil {
				return err
			}
			if !secondLease.LunaAPI || secondLease.RunnerAPI || !secondLease.SpriteInfrastructure {
				t.Fatalf("second lease=%+v", secondLease)
			}
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
	tables := []postgres.NeutralImportTable{
		neutralFixtureTable(t, "policy_revisions", []postgres.NeutralImportKey{{Column: "scope", Kind: "text"}, {Column: "policy_id", Kind: "text"}}),
		neutralFixtureTable(t, "project_policies", []postgres.NeutralImportKey{{Column: "scope", Kind: "text"}}),
		neutralFixtureTable(t, "monthly_budget_policies", []postgres.NeutralImportKey{{Column: "organization_id", Kind: "text"}, {Column: "project_id", Kind: "text"}, {Column: "revision", Kind: "integer"}}),
		neutralFixtureTable(t, "issues", []postgres.NeutralImportKey{{Column: "id", Kind: "integer"}}),
		neutralFixtureTable(t, "leases", []postgres.NeutralImportKey{{Column: "lease_id", Kind: "text"}}),
		neutralFixtureTable(t, "monthly_budget_admissions", []postgres.NeutralImportKey{{Column: "issue_id", Kind: "integer"}}),
		neutralFixtureTable(t, "monthly_budget_leases", []postgres.NeutralImportKey{{Column: "lease_id", Kind: "text"}}),
		neutralFixtureTable(t, "change_review_policies", []postgres.NeutralImportKey{{Column: "organization_id", Kind: "text"}, {Column: "project_id", Kind: "text"}}),
	}
	definitions := map[string]postgres.NeutralImportTable{}
	for _, table := range tables {
		definitions[table.Name] = table
	}
	records := map[string][]postgres.NeutralImportRow{}
	add := func(name string, values map[string]any) {
		records[name] = append(records[name], neutralFixtureRow(t, definitions[name], values))
	}
	old, current := importedReportedPolicyDescriptor(), importedCurrentPolicyDescriptor()
	for id := int64(1); id <= 2; id++ {
		project := fmt.Sprintf("prj_%d", id)
		scope := string(organization) + "/" + project
		for _, row := range []struct {
			descriptor policy.Descriptor
			actor      string
		}{{old, "runner-report"}, {current, "operator"}} {
			encoded, err := json.Marshal(row.descriptor)
			if err != nil {
				t.Fatal(err)
			}
			add("policy_revisions", map[string]any{"scope": scope, "policy_id": row.descriptor.ID, "metadata_json": string(encoded), "approved_by": row.actor, "approved_at": now.Format(time.RFC3339Nano), "gate_github_pull_request": row.descriptor.Gates.GitHubPullRequest})
		}
		add("project_policies", map[string]any{"scope": scope, "policy_id": current.ID})
		add("change_review_policies", map[string]any{"organization_id": organization, "project_id": project, "policy_json": legacyReviewPolicyJSON(current.ID)})
		projectPolicy, err := json.Marshal(budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, RunnerMicros: int64ptr(300)})
		if err != nil {
			t.Fatal(err)
		}
		add("monthly_budget_policies", map[string]any{"organization_id": organization, "project_id": project, "revision": 1, "policy_json": string(projectPolicy), "actor_id": "admin", "updated_at": now.Add(time.Second).Format(time.RFC3339Nano)})
		add("issues", map[string]any{"id": id, "organization_id": organization})
		lease := fmt.Sprintf("lease_%d", id)
		add("leases", map[string]any{"lease_id": lease, "issue_id": id})
		exposure, err := json.Marshal(budget.CostExposure{LunaAPI: true, SpriteInfrastructure: true})
		if err != nil {
			t.Fatal(err)
		}
		add("monthly_budget_admissions", map[string]any{"issue_id": id, "admitted_at": now.Add(-time.Minute).Format(time.RFC3339Nano)})
		add("monthly_budget_leases", map[string]any{"lease_id": lease, "exposure_json": string(exposure)})
	}
	for _, row := range []struct {
		revision int64
		total    int64
	}{{1, 0}, {2, 1000}} {
		encoded, err := json.Marshal(budget.MonthlyPolicy{Enabled: true, Mode: budget.DrainIssue, TotalMicros: int64ptr(row.total)})
		if err != nil {
			t.Fatal(err)
		}
		add("monthly_budget_policies", map[string]any{"organization_id": organization, "project_id": "", "revision": row.revision, "policy_json": string(encoded), "actor_id": "admin", "updated_at": now.Add(time.Duration(row.revision) * time.Second).Format(time.RFC3339Nano)})
	}
	directory, _ := writeNeutralFixture(t, organization, tables, records)
	return directory
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
