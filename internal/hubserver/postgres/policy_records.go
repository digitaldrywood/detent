package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectPolicyRecord(row pgquery.PolicyRevision) policystore.Revision {
	record := policystore.Revision{
		OrganizationID:      row.OrganizationID,
		ProjectID:           row.ProjectID,
		PolicyID:            row.PolicyID,
		ApprovedBy:          row.ApprovedBy,
		ApprovedAt:          row.ApprovedAt,
		PayloadObjectKey:    row.PayloadObjectKey,
		PayloadSHA256:       row.PayloadSHA256,
		PayloadBytes:        row.PayloadBytes,
		PayloadContentBytes: row.PayloadContentBytes,
	}
	if row.GateGithubPullRequest.Valid {
		record.GitHubPullRequest = new(row.GateGithubPullRequest.Bool)
	}
	return record
}

func (s *session) ProjectPolicy(ctx context.Context, project tracker.ProjectID) (policystore.Revision, error) {
	row, err := s.queries.PolicyGetProjectCurrent(ctx, pgquery.PolicyGetProjectCurrentParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil {
		return policystore.Revision{}, mapError(ctx, err)
	}
	return projectPolicyRecord(row), nil
}

func (s *session) ProjectPolicyRevision(ctx context.Context, project tracker.ProjectID, policyID string) (policystore.Revision, error) {
	row, err := s.queries.PolicyGetProjectRevision(ctx, pgquery.PolicyGetProjectRevisionParams{OrganizationID: s.organization, ProjectID: project, PolicyID: policyID})
	if err != nil {
		return policystore.Revision{}, mapError(ctx, err)
	}
	return projectPolicyRecord(row), nil
}

func (s *session) LeasePolicy(ctx context.Context, lease tracker.LeaseID) (policystore.LeasePolicy, error) {
	var row policystore.LeasePolicy
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,project_id,lease_id,policy_id,bound_at
 FROM lease_policy_bindings WHERE organization_id=$1 AND lease_id=$2`, s.organization, lease).
		Scan(&row.OrganizationID, &row.ProjectID, &row.LeaseID, &row.PolicyID, &row.BoundAt)
	if err != nil {
		return policystore.LeasePolicy{}, mapError(ctx, err)
	}
	return row, nil
}

func (s *session) BindLeasePolicy(ctx context.Context, row policystore.LeasePolicy) error {
	if row.OrganizationID != s.organization {
		return corestore.ErrTenantMismatch
	}
	if row.ProjectID == "" || row.LeaseID == "" || row.PolicyID == "" || row.BoundAt.IsZero() {
		return eventstore.ErrInvalidRecord
	}
	row.BoundAt = recordTime(row.BoundAt)
	_, err := s.db.ExecContext(ctx, `INSERT INTO lease_policy_bindings (organization_id,project_id,lease_id,policy_id,bound_at)
 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (organization_id,lease_id) DO NOTHING`,
		row.OrganizationID, row.ProjectID, row.LeaseID, row.PolicyID, row.BoundAt)
	if err != nil {
		return mapError(ctx, err)
	}
	current, err := s.LeasePolicy(ctx, row.LeaseID)
	if err != nil {
		return err
	}
	if current.OrganizationID != row.OrganizationID || current.ProjectID != row.ProjectID || current.LeaseID != row.LeaseID || current.PolicyID != row.PolicyID || !current.BoundAt.Equal(row.BoundAt) {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) ApproveProjectPolicy(ctx context.Context, row policystore.Revision, expected string) error {
	if row.OrganizationID != s.organization || row.ProjectID == "" || row.PolicyID == "" || row.GitHubPullRequest == nil || row.ApprovedBy == "" || row.ApprovedAt.IsZero() {
		return eventstore.ErrInvalidRecord
	}
	if err := s.LockWorkItemProject(ctx, row.ProjectID); err != nil {
		return err
	}
	if err := validRecordPayload(row.OrganizationID, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes); err != nil {
		return err
	}
	row.ApprovedAt = recordTime(row.ApprovedAt)
	inserted, err := s.queries.PolicyInsertProjectRevision(ctx, pgquery.PolicyInsertProjectRevisionParams{
		OrganizationID:        row.OrganizationID,
		ProjectID:             row.ProjectID,
		PolicyID:              row.PolicyID,
		GateGithubPullRequest: sql.NullBool{Bool: *row.GitHubPullRequest, Valid: true},
		ApprovedBy:            row.ApprovedBy,
		ApprovedAt:            recordTime(row.ApprovedAt),
		PayloadObjectKey:      row.PayloadObjectKey,
		PayloadSHA256:         row.PayloadSHA256,
		PayloadBytes:          row.PayloadBytes,
		PayloadContentBytes:   row.PayloadContentBytes,
	})
	if errors.Is(err, sql.ErrNoRows) {
		current, readErr := s.queries.PolicyGetProjectRevision(ctx, pgquery.PolicyGetProjectRevisionParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, PolicyID: row.PolicyID})
		if readErr != nil {
			return mapError(ctx, readErr)
		}
		if !sameProjectPolicy(projectPolicyRecord(current), row) {
			return corestore.ErrUniqueViolation
		}
	} else if err != nil {
		return mapError(ctx, err)
	} else if !sameProjectPolicy(projectPolicyRecord(inserted), row) {
		return corestore.ErrTenantMismatch
	}
	var expectedID *string
	if expected != "" {
		expectedID = &expected
	}
	count, err := s.queries.PolicySetProjectCurrent(ctx, pgquery.PolicySetProjectCurrentParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, PolicyID: row.PolicyID, ExpectedPolicyID: expectedID})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func reviewPolicyRecord(row pgquery.ChangeReviewPolicyRevision) eventstore.ReviewPolicy {
	return eventstore.ReviewPolicy{
		OrganizationID:      row.OrganizationID,
		ProjectID:           row.ProjectID,
		PolicyRevisionID:    row.PolicyID,
		ReviewPolicyID:      row.ReviewPolicyID,
		Revision:            row.Revision,
		UpdatedBy:           row.UpdatedBy,
		UpdatedAt:           row.UpdatedAt,
		PayloadObjectKey:    row.PayloadObjectKey,
		PayloadSHA256:       row.PayloadSHA256,
		PayloadBytes:        row.PayloadBytes,
		PayloadContentBytes: row.PayloadContentBytes,
	}
}

func (s *session) ReviewPolicy(ctx context.Context, project tracker.ProjectID) (eventstore.ReviewPolicy, error) {
	row, err := s.queries.PolicyGetReviewCurrent(ctx, pgquery.PolicyGetReviewCurrentParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil {
		return eventstore.ReviewPolicy{}, mapError(ctx, err)
	}
	return reviewPolicyRecord(row), nil
}

func (s *session) SetReviewPolicy(ctx context.Context, row eventstore.ReviewPolicy, expected string) error {
	if row.OrganizationID != s.organization || row.ProjectID == "" || row.PolicyRevisionID == "" || row.ReviewPolicyID == "" || row.Revision < 1 || row.UpdatedBy == "" || row.UpdatedAt.IsZero() {
		return eventstore.ErrInvalidRecord
	}
	if err := s.LockWorkItemProject(ctx, row.ProjectID); err != nil {
		return err
	}
	if err := validRecordPayload(row.OrganizationID, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes); err != nil {
		return err
	}
	row.UpdatedAt = recordTime(row.UpdatedAt)
	inserted, err := s.queries.PolicyInsertReviewRevision(ctx, pgquery.PolicyInsertReviewRevisionParams{
		OrganizationID:      row.OrganizationID,
		ProjectID:           row.ProjectID,
		ReviewPolicyID:      row.ReviewPolicyID,
		PolicyID:            row.PolicyRevisionID,
		Revision:            row.Revision,
		UpdatedBy:           row.UpdatedBy,
		UpdatedAt:           recordTime(row.UpdatedAt),
		PayloadObjectKey:    row.PayloadObjectKey,
		PayloadSHA256:       row.PayloadSHA256,
		PayloadBytes:        row.PayloadBytes,
		PayloadContentBytes: row.PayloadContentBytes,
	})
	if err == nil && !sameReviewPolicy(reviewPolicyRecord(inserted), row) {
		return corestore.ErrUniqueViolation
	}
	if errors.Is(err, sql.ErrNoRows) {
		current, readErr := s.queries.PolicyGetReviewRevision(ctx, pgquery.PolicyGetReviewRevisionParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ReviewPolicyID: row.ReviewPolicyID})
		if readErr != nil {
			return mapError(ctx, readErr)
		}
		if !sameReviewPolicy(reviewPolicyRecord(current), row) {
			return corestore.ErrUniqueViolation
		}
	} else if err != nil {
		return mapError(ctx, err)
	}
	var expectedID *string
	if expected != "" {
		expectedID = &expected
	}
	count, err := s.queries.PolicySetReviewCurrent(ctx, pgquery.PolicySetReviewCurrentParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ReviewPolicyID: row.ReviewPolicyID, PolicyID: row.PolicyRevisionID, Revision: row.Revision, ExpectedReviewPolicyID: expectedID})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func sameProjectPolicy(left, right policystore.Revision) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyID == right.PolicyID && sameOptionalBool(left.GitHubPullRequest, right.GitHubPullRequest) && left.ApprovedBy == right.ApprovedBy && left.ApprovedAt.Equal(right.ApprovedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameOptionalBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameReviewPolicy(left, right eventstore.ReviewPolicy) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyRevisionID == right.PolicyRevisionID && left.ReviewPolicyID == right.ReviewPolicyID && left.Revision == right.Revision && left.UpdatedBy == right.UpdatedBy && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}
