package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/changerequest"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func neutralCorePolicyPage(ctx context.Context, source CorePolicyImportSource, table string, after []string) ([]NeutralImportRow, error) {
	var expected []NeutralImportKey
	switch table {
	case "policy_revisions":
		expected = []NeutralImportKey{{Column: "scope", Kind: "text"}, {Column: "policy_id", Kind: "text"}}
	case "project_policies":
		expected = []NeutralImportKey{{Column: "scope", Kind: "text"}}
	case "change_review_policies":
		expected = []NeutralImportKey{{Column: "organization_id", Kind: "text"}, {Column: "project_id", Kind: "text"}}
	case "monthly_budget_policies":
		expected = []NeutralImportKey{{Column: "organization_id", Kind: "text"}, {Column: "project_id", Kind: "text"}, {Column: "revision", Kind: "integer"}}
	case "monthly_budget_admissions":
		expected = []NeutralImportKey{{Column: "issue_id", Kind: "integer"}}
	case "monthly_budget_leases":
		expected = []NeutralImportKey{{Column: "lease_id", Kind: "text"}}
	default:
		return nil, ErrNeutralImport
	}
	actual, err := source.neutral.TableKey(table)
	if err != nil || !slices.Equal(actual, expected) {
		return nil, ErrNeutralImport
	}
	return source.neutral.Page(ctx, table, after, 100)
}
func neutralCoreReviewPolicies(ctx context.Context, source CorePolicyImportSource, after tracker.ProjectID) ([]coreReviewPolicyImportRow, error) {
	rows, err := neutralCorePolicyPage(ctx, source, "change_review_policies", []string{string(source.OrganizationID), string(after)})
	if err != nil {
		return nil, err
	}
	result := make([]coreReviewPolicyImportRow, 0, len(rows))
	for _, raw := range rows {
		var row coreReviewPolicyImportRow
		if err := raw.Decode(map[string]any{"project_id": &row.project, "policy_json": &row.raw}); err != nil {
			return nil, err
		}
		if row.project == "" || json.Unmarshal([]byte(row.raw), &row.rules) != nil || row.rules.PolicyID == "" {
			return nil, eventstore.ErrInvalidRecord
		}
		expected := changerequest.PolicyID(row.rules)
		if expected == "" || row.rules.ID != "" && row.rules.ID != expected {
			return nil, mutation.ErrConflict
		}
		row.rules.ID = expected
		result = append(result, row)
	}
	return result, nil
}
func neutralCorePolicyRevisions(ctx context.Context, source CorePolicyImportSource, afterScope, afterID string) ([]projectPolicyImportRow, error) {
	var after []string
	if afterScope != "" {
		after = []string{afterScope, afterID}
	}
	rows, err := neutralCorePolicyPage(ctx, source, "policy_revisions", after)
	if err != nil {
		return nil, err
	}
	result := make([]projectPolicyImportRow, 0, len(rows))
	for _, raw := range rows {
		var row projectPolicyImportRow
		var encoded, approvedAt string
		var sourceGate sql.NullBool
		if err := raw.Decode(map[string]any{"scope": &row.Scope, "policy_id": &row.PolicyID, "metadata_json": &encoded, "approved_by": &row.ApprovedBy, "approved_at": &approvedAt, "gate_github_pull_request": &sourceGate}); err != nil {
			return nil, err
		}
		row.ProjectID, err = projectPolicyScope(source.OrganizationID, row.Scope)
		if err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(encoded), &row.Descriptor) != nil {
			return nil, eventstore.ErrInvalidRecord
		}
		if row.Descriptor.ID != row.PolicyID || sourceGate.Valid && sourceGate.Bool != row.Descriptor.Gates.GitHubPullRequest {
			return nil, mutation.ErrConflict
		}
		if _, err := workflowconfig.ResolveNativePolicy(row.Descriptor); err != nil {
			return nil, err
		}
		row.ApprovedAt, err = time.Parse(time.RFC3339Nano, approvedAt)
		if err != nil || row.ApprovedBy == "" || row.ApprovedAt.IsZero() {
			return nil, eventstore.ErrInvalidRecord
		}
		result = append(result, row)
	}
	return result, nil
}
func neutralCoreCurrentPolicy(ctx context.Context, source CorePolicyImportSource, scope string) (string, error) {
	keys, err := source.neutral.TableKey("project_policies")
	if err != nil || !slices.Equal(keys, []NeutralImportKey{{Column: "scope", Kind: "text"}}) {
		return "", ErrNeutralImport
	}
	row, err := source.neutral.Lookup(ctx, "project_policies", []string{scope})
	if err != nil {
		return "", err
	}
	var policy string
	if err := row.Decode(map[string]any{"policy_id": &policy}); err != nil {
		return "", err
	}
	return policy, nil
}
func neutralCorePolicyPointers(ctx context.Context, source CorePolicyImportSource, afterScope string) ([][2]string, error) {
	var after []string
	if afterScope != "" {
		after = []string{afterScope}
	}
	rows, err := neutralCorePolicyPage(ctx, source, "project_policies", after)
	if err != nil {
		return nil, err
	}
	result := make([][2]string, 0, len(rows))
	for _, raw := range rows {
		var scope, policy string
		if err := raw.Decode(map[string]any{"scope": &scope, "policy_id": &policy}); err != nil {
			return nil, err
		}
		project, err := projectPolicyScope(source.OrganizationID, scope)
		if err != nil {
			return nil, err
		}
		result = append(result, [2]string{string(project), policy})
	}
	return result, nil
}
func neutralCoreMonthlyPolicies(ctx context.Context, source CorePolicyImportSource, afterProject string, afterRevision int64) ([]billingstore.MonthlyPolicy, error) {
	rows, err := neutralCorePolicyPage(ctx, source, "monthly_budget_policies", []string{string(source.OrganizationID), afterProject, strconv.FormatInt(afterRevision, 10)})
	if err != nil {
		return nil, err
	}
	result := make([]billingstore.MonthlyPolicy, 0, len(rows))
	for _, raw := range rows {
		row := billingstore.MonthlyPolicy{OrganizationID: source.OrganizationID}
		var encoded, updatedAt string
		if err := raw.Decode(map[string]any{"project_id": &row.ProjectID, "revision": &row.Revision, "policy_json": &encoded, "actor_id": &row.Actor, "updated_at": &updatedAt}); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(encoded), &row.Policy) != nil {
			return nil, eventstore.ErrInvalidRecord
		}
		row.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil || row.Revision < 1 {
			return nil, eventstore.ErrInvalidRecord
		}
		if err := row.Policy.Validate(); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}
func neutralCoreMonthlyAdmissions(ctx context.Context, source CorePolicyImportSource, after int64) ([]billingstore.MonthlyAdmission, error) {
	rows, err := neutralCorePolicyPage(ctx, source, "monthly_budget_admissions", []string{strconv.FormatInt(after, 10)})
	if err != nil {
		return nil, err
	}
	result := make([]billingstore.MonthlyAdmission, 0, len(rows))
	for _, raw := range rows {
		row := billingstore.MonthlyAdmission{OrganizationID: source.OrganizationID}
		var admittedAt string
		if err := raw.Decode(map[string]any{"issue_id": &row.IssueID, "admitted_at": &admittedAt}); err != nil {
			return nil, err
		}
		if err := neutralCoreBudgetIssue(ctx, source, int64(row.IssueID)); err != nil {
			return nil, err
		}
		row.AdmittedAt, err = time.Parse(time.RFC3339Nano, admittedAt)
		if err != nil || row.IssueID < 1 || row.AdmittedAt.IsZero() {
			return nil, eventstore.ErrInvalidRecord
		}
		result = append(result, row)
	}
	return result, nil
}
func neutralCoreBudgetIssue(ctx context.Context, source CorePolicyImportSource, id int64) error {
	keys, err := source.neutral.TableKey("issues")
	if err != nil || !slices.Equal(keys, []NeutralImportKey{{Column: "id", Kind: "integer"}}) {
		return ErrNeutralImport
	}
	row, err := source.neutral.Lookup(ctx, "issues", []string{strconv.FormatInt(id, 10)})
	if err != nil {
		return err
	}
	var organization tracker.OrganizationID
	if err := row.Decode(map[string]any{"organization_id": &organization}); err != nil {
		return err
	}
	if organization != source.OrganizationID {
		return corestore.ErrTenantMismatch
	}
	return nil
}
func neutralCoreMonthlyLeases(ctx context.Context, source CorePolicyImportSource, afterLease string) ([]billingstore.MonthlyLease, error) {
	var after []string
	if afterLease != "" {
		after = []string{afterLease}
	}
	rows, err := neutralCorePolicyPage(ctx, source, "monthly_budget_leases", after)
	if err != nil {
		return nil, err
	}
	result := make([]billingstore.MonthlyLease, 0, len(rows))
	keys, err := source.neutral.TableKey("leases")
	if err != nil || !slices.Equal(keys, []NeutralImportKey{{Column: "lease_id", Kind: "text"}}) {
		return nil, ErrNeutralImport
	}
	for _, raw := range rows {
		row := billingstore.MonthlyLease{OrganizationID: source.OrganizationID}
		var encoded string
		if err := raw.Decode(map[string]any{"lease_id": &row.LeaseID, "exposure_json": &encoded}); err != nil {
			return nil, err
		}
		lease, err := source.neutral.Lookup(ctx, "leases", []string{string(row.LeaseID)})
		if err != nil {
			return nil, err
		}
		var issue int64
		if err := lease.Decode(map[string]any{"issue_id": &issue}); err != nil {
			return nil, err
		}
		if err := neutralCoreBudgetIssue(ctx, source, issue); err != nil {
			return nil, err
		}
		var exposure budget.CostExposure
		if json.Unmarshal([]byte(encoded), &exposure) != nil || row.LeaseID == "" {
			return nil, eventstore.ErrInvalidRecord
		}
		row.LunaAPI, row.RunnerAPI, row.SpriteInfrastructure = exposure.LunaAPI, exposure.RunnerAPI, exposure.SpriteInfrastructure
		result = append(result, row)
	}
	return result, nil
}
