package policyfacts

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RecordPolicy(ctx context.Context, tx pgcore.Transaction, row policystore.Revision) error {
	facts := row.Compatibility
	if facts == nil {
		return nil
	}
	if row.GitHubPullRequest != nil && *row.GitHubPullRequest != facts.GitHubPullRequest {
		return eventstore.ErrInvalidRecord
	}
	if facts.RequiredChecks < 0 || facts.RequiredChecks > 256 {
		return eventstore.ErrInvalidRecord
	}
	inserted, err := tx.Queries.PolicyInsertRevisionCompatibility(ctx, pgquery.PolicyInsertRevisionCompatibilityParams{
		OrganizationID:           row.OrganizationID,
		ProjectID:                row.ProjectID,
		PolicyID:                 row.PolicyID,
		PayloadSHA256:            row.PayloadSHA256,
		GateGithubPullRequest:    facts.GitHubPullRequest,
		GateLandingMode:          facts.LandingMode,
		GateKind:                 facts.Kind,
		GateLandingCommandDigest: facts.LandingCommandDigest,
		GateGithubCiDigest:       facts.GitHubCIDigest,
		GatePlanReview:           facts.PlanReview,
		GateAutomatedReview:      facts.AutomatedReview,
		GateRequiredChecks:       int32(facts.RequiredChecks),
		GateValidator:            facts.Validator,
		GateSecurityAudit:        facts.SecurityAudit,
		GateHumanReview:          facts.HumanReview,
		GatePlanEnabled:          facts.PlanEnabled,
	})
	if err != nil {
		return pgcore.MapError(ctx, err)
	}
	if inserted == 1 {
		return nil
	}
	existing, err := Policies(ctx, tx, row.ProjectID, []string{row.PolicyID})
	if err != nil {
		return err
	}
	current, found := existing[row.PolicyID]
	if !found || current.PayloadSHA256 != row.PayloadSHA256 || current.Gates != *facts {
		return corestore.ErrUniqueViolation
	}
	return nil
}

func Policies(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID, ids []string) (map[string]policystore.Compatibility, error) {
	result := make(map[string]policystore.Compatibility, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := tx.Queries.PolicyGetRevisionCompatibility(ctx, pgquery.PolicyGetRevisionCompatibilityParams{OrganizationID: tx.Organization, ProjectID: project, PolicyIds: ids})
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	for _, row := range rows {
		result[row.PolicyID] = policystore.Compatibility{PayloadSHA256: row.PayloadSHA256, Gates: changerequest.CompatibilityGates{
			GitHubPullRequest: row.GateGithubPullRequest, LandingMode: row.GateLandingMode, Kind: row.GateKind,
			LandingCommandDigest: row.GateLandingCommandDigest, GitHubCIDigest: row.GateGithubCiDigest, PlanReview: row.GatePlanReview,
			AutomatedReview: row.GateAutomatedReview, RequiredChecks: int(row.GateRequiredChecks), Validator: row.GateValidator,
			SecurityAudit: row.GateSecurityAudit, HumanReview: row.GateHumanReview, PlanEnabled: row.GatePlanEnabled,
		}}
	}
	return result, nil
}

func RecordReview(ctx context.Context, tx pgcore.Transaction, row eventstore.ReviewPolicy) error {
	facts := row.Compatibility
	if facts == nil {
		return nil
	}
	if changerequest.ValidateChecks(facts.RequiredChecks) != nil {
		return eventstore.ErrInvalidRecord
	}
	inserted, err := tx.Queries.PolicyInsertReviewCompatibility(ctx, pgquery.PolicyInsertReviewCompatibilityParams{
		OrganizationID:     row.OrganizationID,
		ProjectID:          row.ProjectID,
		ReviewPolicyID:     row.ReviewPolicyID,
		PolicyID:           row.PolicyRevisionID,
		PayloadSHA256:      row.PayloadSHA256,
		RequireReview:      facts.RequireReview,
		RequiredCheckCount: int32(len(facts.RequiredChecks)),
	})
	if err != nil {
		return pgcore.MapError(ctx, err)
	}
	if inserted == 1 {
		if len(facts.RequiredChecks) == 0 {
			return nil
		}
		params := pgquery.PolicyInsertReviewChecksParams{OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID), ReviewPolicyID: row.ReviewPolicyID}
		for index, check := range facts.RequiredChecks {
			params.Ordinals = append(params.Ordinals, int32(index))
			params.Names = append(params.Names, check.Name)
			params.PrincipalIds = append(params.PrincipalIds, check.PrincipalID)
			params.WorkflowIds = append(params.WorkflowIds, check.WorkflowID)
			params.WorkflowSha256s = append(params.WorkflowSha256s, check.WorkflowSHA256)
			params.Sources = append(params.Sources, check.Source)
			params.MaxAges = append(params.MaxAges, check.MaxAgeSeconds)
		}
		count, err := tx.Queries.PolicyInsertReviewChecks(ctx, params)
		if err != nil {
			return pgcore.MapError(ctx, err)
		}
		if count != int64(len(facts.RequiredChecks)) {
			return corestore.ErrUniqueViolation
		}
		return nil
	}
	existing, err := Reviews(ctx, tx, row.ProjectID, []string{row.ReviewPolicyID})
	if err != nil {
		return err
	}
	current, found := existing[row.ReviewPolicyID]
	if !found || current.PolicyID != row.PolicyRevisionID || current.PayloadSHA256 != row.PayloadSHA256 || current.RequireReview != facts.RequireReview || !slices.Equal(current.RequiredChecks, facts.RequiredChecks) {
		return corestore.ErrUniqueViolation
	}
	return nil
}

func Reviews(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID, ids []string) (map[string]policystore.ReviewCompatibility, error) {
	result := make(map[string]policystore.ReviewCompatibility, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := tx.Queries.PolicyGetReviewCompatibility(ctx, pgquery.PolicyGetReviewCompatibilityParams{OrganizationID: tx.Organization, ProjectID: project, ReviewPolicyIds: ids})
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	checks, err := tx.Queries.PolicyGetReviewChecks(ctx, pgquery.PolicyGetReviewChecksParams{OrganizationID: tx.Organization, ProjectID: project, ReviewPolicyIds: ids})
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	byReview := make(map[string][]tracker.ChangeCheckSpec, len(rows))
	for _, check := range checks {
		if int(check.Ordinal) != len(byReview[check.ReviewPolicyID]) {
			return nil, eventstore.ErrInvalidRecord
		}
		byReview[check.ReviewPolicyID] = append(byReview[check.ReviewPolicyID], tracker.ChangeCheckSpec{Name: check.Name, PrincipalID: check.PrincipalID, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSha256, Source: check.Source, MaxAgeSeconds: check.MaxAgeSeconds})
	}
	for _, row := range rows {
		listed := byReview[row.ReviewPolicyID]
		if len(listed) != int(row.RequiredCheckCount) {
			return nil, eventstore.ErrInvalidRecord
		}
		result[row.ReviewPolicyID] = policystore.ReviewCompatibility{PolicyID: row.PolicyID, PayloadSHA256: row.PayloadSHA256, ReviewFacts: policystore.ReviewFacts{RequireReview: row.RequireReview, RequiredChecks: listed}}
	}
	return result, nil
}

func BackfillPolicy(ctx context.Context, tx pgcore.Transaction, row policystore.Revision) error {
	stored, err := tx.Queries.PolicyGetProjectRevision(ctx, pgquery.PolicyGetProjectRevisionParams{OrganizationID: tx.Organization, ProjectID: row.ProjectID, PolicyID: row.PolicyID})
	if err != nil {
		return pgcore.MapError(ctx, err)
	}
	if row.OrganizationID != tx.Organization || stored.PayloadSHA256 != row.PayloadSHA256 || row.Compatibility == nil {
		return eventstore.ErrInvalidRecord
	}
	row.GitHubPullRequest = nil
	if stored.GateGithubPullRequest.Valid {
		row.GitHubPullRequest = new(stored.GateGithubPullRequest.Bool)
	}
	return RecordPolicy(ctx, tx, row)
}

func ReviewRevision(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID, id string) (eventstore.ReviewPolicy, error) {
	row, err := tx.Queries.PolicyGetReviewRevision(ctx, pgquery.PolicyGetReviewRevisionParams{OrganizationID: tx.Organization, ProjectID: project, ReviewPolicyID: id})
	if err != nil {
		return eventstore.ReviewPolicy{}, pgcore.MapError(ctx, err)
	}
	return eventstore.ReviewPolicy{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, PolicyRevisionID: row.PolicyID, ReviewPolicyID: row.ReviewPolicyID, Revision: row.Revision, UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}, nil
}

func BackfillReview(ctx context.Context, tx pgcore.Transaction, row eventstore.ReviewPolicy) error {
	stored, err := ReviewRevision(ctx, tx, row.ProjectID, row.ReviewPolicyID)
	if err != nil {
		return err
	}
	if row.OrganizationID != tx.Organization || stored.PolicyRevisionID != row.PolicyRevisionID || stored.PayloadSHA256 != row.PayloadSHA256 || row.Compatibility == nil {
		return eventstore.ErrInvalidRecord
	}
	return RecordReview(ctx, tx, row)
}

func CurrentReview(ctx context.Context, tx pgcore.Transaction, project tracker.ProjectID) (eventstore.ReviewPolicy, error) {
	row, err := tx.Queries.PolicyGetReviewCurrentFacts(ctx, pgquery.PolicyGetReviewCurrentFactsParams{OrganizationID: tx.Organization, ProjectID: project})
	if err != nil {
		return eventstore.ReviewPolicy{}, pgcore.MapError(ctx, err)
	}
	result := eventstore.ReviewPolicy{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, PolicyRevisionID: row.PolicyID, ReviewPolicyID: row.ReviewPolicyID, Revision: row.Revision, UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}
	if !row.HasFacts {
		return result, nil
	}
	count := len(row.Ordinals)
	if count != int(row.RequiredCheckCount) || len(row.Names) != count || len(row.PrincipalIds) != count || len(row.WorkflowIds) != count || len(row.WorkflowSha256s) != count || len(row.Sources) != count || len(row.MaxAges) != count {
		return eventstore.ReviewPolicy{}, eventstore.ErrInvalidRecord
	}
	facts := policystore.ReviewFacts{RequireReview: row.RequireReview}
	for index, ordinal := range row.Ordinals {
		if int(ordinal) != index {
			return eventstore.ReviewPolicy{}, eventstore.ErrInvalidRecord
		}
		facts.RequiredChecks = append(facts.RequiredChecks, tracker.ChangeCheckSpec{Name: row.Names[index], PrincipalID: row.PrincipalIds[index], WorkflowID: row.WorkflowIds[index], WorkflowSHA256: row.WorkflowSha256s[index], Source: row.Sources[index], MaxAgeSeconds: row.MaxAges[index]})
	}
	result.Compatibility = &facts
	return result, nil
}
