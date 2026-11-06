package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectPolicyRecord(row pgquery.PolicyRevision) policystore.Revision {
	return projectPolicyFields(row.OrganizationID, row.ProjectID, row.PolicyID, row.ApprovedBy, row.ApprovedAt, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, row.GateGithubPullRequest)
}

func projectPolicyCurrentRecord(row pgquery.PolicyGetProjectCurrentRow) policystore.Revision {
	return projectPolicyFields(row.OrganizationID, row.ProjectID, row.PolicyID, row.ApprovedBy, row.ApprovedAt, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, row.GateGithubPullRequest)
}

func projectPolicyRevisionRecord(row pgquery.PolicyGetProjectRevisionRow) policystore.Revision {
	return projectPolicyFields(row.OrganizationID, row.ProjectID, row.PolicyID, row.ApprovedBy, row.ApprovedAt, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, row.GateGithubPullRequest)
}

func projectPolicyFields(organization tracker.OrganizationID, project tracker.ProjectID, policyID, approvedBy string, approvedAt time.Time, objectKey, sha256 string, bytes, contentBytes int64, gate pgtype.Bool) policystore.Revision {
	record := policystore.Revision{OrganizationID: organization, ProjectID: project, PolicyID: policyID, ApprovedBy: approvedBy, ApprovedAt: approvedAt, PayloadObjectKey: objectKey, PayloadSHA256: sha256, PayloadBytes: bytes, PayloadContentBytes: contentBytes}
	if gate.Valid {
		record.GitHubPullRequest = new(gate.Bool)
	}
	return record
}

func (s *session) ProjectPolicy(ctx context.Context, project tracker.ProjectID) (policystore.Revision, error) {
	row, err := s.queries.PolicyGetProjectCurrent(ctx, pgquery.PolicyGetProjectCurrentParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil {
		return policystore.Revision{}, mapError(ctx, err)
	}
	return projectPolicyCurrentRecord(row), nil
}

func (s *session) ProjectPolicyRevision(ctx context.Context, project tracker.ProjectID, policyID string) (policystore.Revision, error) {
	row, err := s.queries.PolicyGetProjectRevision(ctx, pgquery.PolicyGetProjectRevisionParams{OrganizationID: s.organization, ProjectID: project, PolicyID: policyID})
	if err != nil {
		return policystore.Revision{}, mapError(ctx, err)
	}
	return projectPolicyRevisionRecord(row), nil
}

func (s *session) ProjectPolicyObservations(ctx context.Context, project tracker.ProjectID, approvedPolicyID, policyID string, now time.Time, limit int) ([]policystore.ObservedRevision, int64, error) {
	if project == "" || now.IsZero() || limit < 1 || limit > 1024 {
		return nil, 0, eventstore.ErrInvalidRecord
	}
	var approvedID, selectedID *string
	if approvedPolicyID != "" {
		approvedID = &approvedPolicyID
	}
	if policyID != "" {
		selectedID = &policyID
	}
	now = recordTime(now)
	rows, err := s.queries.PolicyProjectPolicyObservations(ctx, pgquery.PolicyProjectPolicyObservationsParams{
		OrganizationID:   s.organization,
		ProjectID:        project,
		ApprovedPolicyID: approvedID,
		PolicyID:         selectedID,
		Now:              now,
		PageSize:         int32(limit),
	})
	if err != nil {
		return nil, 0, mapError(ctx, err)
	}
	result := make([]policystore.ObservedRevision, 0, len(rows))
	var policyCount int64
	for _, row := range rows {
		policyCount = row.PolicyCount
		revision := policystore.Revision{
			OrganizationID:      row.OrganizationID,
			ProjectID:           row.ProjectID,
			PolicyID:            row.PolicyID,
			ApprovedBy:          row.RevisionApprovedBy,
			PayloadObjectKey:    row.PayloadObjectKey,
			PayloadSHA256:       row.PayloadSHA256,
			PayloadBytes:        row.PayloadBytes,
			PayloadContentBytes: row.PayloadContentBytes,
		}
		if row.GateGithubPullRequest.Valid {
			revision.GitHubPullRequest = new(row.GateGithubPullRequest.Bool)
		}
		observation := policystore.Observation{
			OrganizationID: row.OrganizationID,
			ProjectID:      row.ProjectID,
			RunnerID:       row.RunnerID,
			PolicyID:       row.PolicyID,
			ObservedAt:     row.ObservedAt,
			Revision:       revision,
		}
		if row.SourcePresent {
			observation.Source = &policy.RepositorySource{
				Repository:             row.SourceRepository,
				Commit:                 row.SourceCommit,
				DefaultBranch:          row.SourceDefaultBranch,
				DefaultBranchHead:      row.SourceDefaultBranchHead,
				DefaultBranchReachable: row.SourceDefaultBranchReachable,
			}
		}
		result = append(result, policystore.ObservedRevision{
			Observation:         observation,
			PreviouslyApproved:  row.PreviouslyApproved || row.RevisionApprovedBy != "runner-report",
			DistinctPolicyCount: row.DistinctPolicyCount,
			RunnerCount:         row.RunnerCount,
			RepositoryApproved:  row.RepositoryApproved,
		})
	}
	return result, policyCount, nil
}

func (s *session) CurrentProjectPolicyObservation(ctx context.Context, project tracker.ProjectID, policyID string, now time.Time) (bool, error) {
	if project == "" || policyID == "" || now.IsZero() {
		return false, eventstore.ErrInvalidRecord
	}
	now = recordTime(now)
	exists, err := s.queries.PolicyCurrentProjectObservation(ctx, pgquery.PolicyCurrentProjectObservationParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		PolicyID:       policyID,
		Now:            now,
	})
	if err != nil {
		return false, mapError(ctx, err)
	}
	return exists, nil
}

func (s *session) ProjectWorkflowApplies(ctx context.Context, project tracker.ProjectID, before int64, limit int) ([]policystore.WorkflowApply, error) {
	if project == "" || before < 0 || limit < 1 || limit > 1024 {
		return nil, eventstore.ErrInvalidRecord
	}
	rows, err := s.queries.PolicyProjectWorkflowApplies(ctx, pgquery.PolicyProjectWorkflowAppliesParams{OrganizationID: s.organization, ProjectID: project, BeforeID: before, PageSize: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]policystore.WorkflowApply, 0, len(rows))
	for _, row := range rows {
		result = append(result, projectWorkflowApplyRecord(row))
	}
	return result, nil
}

func (s *session) CreateProjectWorkflowApply(ctx context.Context, record policystore.WorkflowApply) (policystore.WorkflowApply, error) {
	if record.OrganizationID != s.organization || record.ProjectID == "" || record.PolicyID == "" || len(record.Repository) > 1024 || len(record.SourceCommit) > 256 || record.DefinitionDigest == "" || record.AppliedBy == "" || record.AppliedAt.IsZero() {
		return policystore.WorkflowApply{}, eventstore.ErrInvalidRecord
	}
	record.AppliedAt = recordTime(record.AppliedAt)
	row, err := s.queries.PolicyInsertWorkflowApply(ctx, pgquery.PolicyInsertWorkflowApplyParams{
		OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, PolicyID: record.PolicyID,
		PreviousPolicyID: record.PreviousPolicyID, Repository: record.Repository, SourceCommit: record.SourceCommit,
		PreviousDefinitionDigest: record.PreviousDefinitionDigest, DefinitionDigest: record.DefinitionDigest,
		RunnerID: record.RunnerID, AppliedBy: record.AppliedBy, AppliedAt: record.AppliedAt,
		ChangeID: record.ChangeID, PullRequestUrl: record.PullRequestURL,
	})
	if err != nil {
		return policystore.WorkflowApply{}, mapError(ctx, err)
	}
	return projectWorkflowApplyRecord(row), nil
}

func projectWorkflowApplyRecord(row pgquery.ProjectWorkflowApply) policystore.WorkflowApply {
	return policystore.WorkflowApply{
		OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ID: row.ID, PolicyID: row.PolicyID,
		PreviousPolicyID: row.PreviousPolicyID, Repository: row.Repository, SourceCommit: row.SourceCommit,
		PreviousDefinitionDigest: row.PreviousDefinitionDigest, DefinitionDigest: row.DefinitionDigest,
		RunnerID: row.RunnerID, AppliedBy: row.AppliedBy, AppliedAt: row.AppliedAt,
		ChangeID: row.ChangeID, PullRequestURL: row.PullRequestUrl,
	}
}

func (s *session) RecordProjectPolicyObservation(ctx context.Context, observation policystore.Observation) (bool, error) {
	if observation.OrganizationID != s.organization || observation.ProjectID == "" || observation.RunnerID == "" || observation.PolicyID == "" || observation.ObservedAt.IsZero() || observation.Revision.PolicyID != observation.PolicyID || observation.Revision.ProjectID != observation.ProjectID || observation.Revision.OrganizationID != observation.OrganizationID {
		return false, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(observation.OrganizationID, observation.Revision.PayloadObjectKey, observation.Revision.PayloadSHA256, observation.Revision.PayloadBytes, observation.Revision.PayloadContentBytes); err != nil {
		return false, err
	}
	if observation.Source != nil && (len(observation.Source.Repository) > 1024 || len(observation.Source.Commit) > 256 || len(observation.Source.DefaultBranch) > 255 || len(observation.Source.DefaultBranchHead) > 256) {
		return false, eventstore.ErrInvalidRecord
	}
	if err := s.LockWorkItemProject(ctx, observation.ProjectID); err != nil {
		return false, err
	}
	revision := observation.Revision
	revision.ApprovedBy = "runner-report"
	revision.ApprovedAt = recordTime(observation.ObservedAt)
	gate := pgtype.Bool{}
	if revision.GitHubPullRequest != nil {
		gate = pgtype.Bool{Bool: *revision.GitHubPullRequest, Valid: true}
	}
	inserted, err := s.queries.PolicyInsertProjectRevision(ctx, pgquery.PolicyInsertProjectRevisionParams{
		OrganizationID:        revision.OrganizationID,
		ProjectID:             revision.ProjectID,
		PolicyID:              revision.PolicyID,
		GateGithubPullRequest: gate,
		ApprovedBy:            revision.ApprovedBy,
		ApprovedAt:            revision.ApprovedAt,
		PayloadObjectKey:      revision.PayloadObjectKey,
		PayloadSHA256:         revision.PayloadSHA256,
		PayloadBytes:          revision.PayloadBytes,
		PayloadContentBytes:   revision.PayloadContentBytes,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		current, readErr := s.queries.PolicyGetProjectRevision(ctx, pgquery.PolicyGetProjectRevisionParams{OrganizationID: revision.OrganizationID, ProjectID: revision.ProjectID, PolicyID: revision.PolicyID})
		if readErr != nil {
			return false, mapError(ctx, readErr)
		}
		if !sameProjectPolicyContent(projectPolicyRevisionRecord(current), revision) {
			return false, corestore.ErrUniqueViolation
		}
	} else if err != nil {
		return false, mapError(ctx, err)
	} else if !sameProjectPolicyContent(projectPolicyRecord(inserted), revision) {
		return false, corestore.ErrTenantMismatch
	}
	var source policy.RepositorySource
	sourcePresent := observation.Source != nil
	if sourcePresent {
		source = *observation.Source
	}
	rows, err := s.queries.PolicyRecordProjectObservation(ctx, pgquery.PolicyRecordProjectObservationParams{
		OrganizationID:               observation.OrganizationID,
		ProjectID:                    observation.ProjectID,
		RunnerID:                     observation.RunnerID,
		PolicyID:                     observation.PolicyID,
		ObservedAt:                   recordTime(observation.ObservedAt),
		SourcePresent:                sourcePresent,
		SourceRepository:             source.Repository,
		SourceCommit:                 source.Commit,
		SourceDefaultBranch:          source.DefaultBranch,
		SourceDefaultBranchHead:      source.DefaultBranchHead,
		SourceDefaultBranchReachable: source.DefaultBranchReachable,
	})
	if err != nil {
		return false, mapError(ctx, err)
	}
	return rows == 1, nil
}

func (s *session) LeasePolicy(ctx context.Context, lease tracker.LeaseID) (policystore.LeasePolicy, error) {
	var row policystore.LeasePolicy
	err := s.db.QueryRow(ctx, `SELECT organization_id,project_id,lease_id,policy_id,bound_at
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
	_, err := s.db.Exec(ctx, `INSERT INTO lease_policy_bindings (organization_id,project_id,lease_id,policy_id,bound_at)
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
		GateGithubPullRequest: pgtype.Bool{Bool: *row.GitHubPullRequest, Valid: true},
		ApprovedBy:            row.ApprovedBy,
		ApprovedAt:            recordTime(row.ApprovedAt),
		PayloadObjectKey:      row.PayloadObjectKey,
		PayloadSHA256:         row.PayloadSHA256,
		PayloadBytes:          row.PayloadBytes,
		PayloadContentBytes:   row.PayloadContentBytes,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		current, readErr := s.queries.PolicyGetProjectRevision(ctx, pgquery.PolicyGetProjectRevisionParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, PolicyID: row.PolicyID})
		if readErr != nil {
			return mapError(ctx, readErr)
		}
		if !sameProjectPolicyContent(projectPolicyRevisionRecord(current), row) {
			return corestore.ErrUniqueViolation
		}
	} else if err != nil {
		return mapError(ctx, err)
	} else if !sameProjectPolicyContent(projectPolicyRecord(inserted), row) {
		return corestore.ErrTenantMismatch
	}
	if err := s.queries.PolicyInsertRevisionApproval(ctx, pgquery.PolicyInsertRevisionApprovalParams{
		OrganizationID: row.OrganizationID,
		ProjectID:      row.ProjectID,
		PolicyID:       row.PolicyID,
		ApprovedBy:     row.ApprovedBy,
		ApprovedAt:     row.ApprovedAt,
	}); err != nil {
		return mapError(ctx, err)
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

func (s *session) ImportProjectPolicyRevision(ctx context.Context, row policystore.Revision, approved bool) error {
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
		GateGithubPullRequest: pgtype.Bool{Bool: *row.GitHubPullRequest, Valid: true},
		ApprovedBy:            row.ApprovedBy,
		ApprovedAt:            row.ApprovedAt,
		PayloadObjectKey:      row.PayloadObjectKey,
		PayloadSHA256:         row.PayloadSHA256,
		PayloadBytes:          row.PayloadBytes,
		PayloadContentBytes:   row.PayloadContentBytes,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		current, readErr := s.queries.PolicyGetProjectRevision(ctx, pgquery.PolicyGetProjectRevisionParams{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, PolicyID: row.PolicyID})
		if readErr != nil {
			return mapError(ctx, readErr)
		}
		if !sameProjectPolicyContent(projectPolicyRevisionRecord(current), row) {
			return corestore.ErrUniqueViolation
		}
		var approvedBy string
		var approvedAt time.Time
		if readErr := s.db.QueryRow(ctx, `SELECT approved_by,approved_at FROM policy_revisions WHERE organization_id=$1 AND project_id=$2 AND policy_id=$3`, row.OrganizationID, row.ProjectID, row.PolicyID).Scan(&approvedBy, &approvedAt); readErr != nil {
			return mapError(ctx, readErr)
		}
		if approvedBy != row.ApprovedBy || !approvedAt.Equal(row.ApprovedAt) {
			return corestore.ErrUniqueViolation
		}
	} else if err != nil {
		return mapError(ctx, err)
	} else if !sameProjectPolicyContent(projectPolicyRecord(inserted), row) {
		return corestore.ErrTenantMismatch
	}
	var approvedBy string
	var approvedAt time.Time
	err = s.db.QueryRow(ctx, `SELECT approved_by,approved_at FROM policy_revision_approvals WHERE organization_id=$1 AND project_id=$2 AND policy_id=$3`, row.OrganizationID, row.ProjectID, row.PolicyID).Scan(&approvedBy, &approvedAt)
	if !approved {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
		}
		return corestore.ErrUniqueViolation
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.queries.PolicyInsertRevisionApproval(ctx, pgquery.PolicyInsertRevisionApprovalParams{
			OrganizationID: row.OrganizationID,
			ProjectID:      row.ProjectID,
			PolicyID:       row.PolicyID,
			ApprovedBy:     row.ApprovedBy,
			ApprovedAt:     row.ApprovedAt,
		}); err != nil {
			return mapError(ctx, err)
		}
		approvedBy, approvedAt = row.ApprovedBy, row.ApprovedAt
	} else if err != nil {
		return mapError(ctx, err)
	}
	if approvedBy != row.ApprovedBy || !approvedAt.Equal(row.ApprovedAt) {
		return corestore.ErrUniqueViolation
	}
	return nil
}

func (s *session) ImportProjectPolicyCurrent(ctx context.Context, project tracker.ProjectID, policyID string) error {
	if project == "" || policyID == "" {
		return eventstore.ErrInvalidRecord
	}
	if err := s.LockWorkItemProject(ctx, project); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO project_policies (organization_id,project_id,policy_id)
SELECT $1,$2,$3 WHERE EXISTS (SELECT 1 FROM policy_revisions WHERE organization_id=$1 AND project_id=$2 AND policy_id=$3)
ON CONFLICT (organization_id,project_id) DO NOTHING`, s.organization, project, policyID); err != nil {
		return mapError(ctx, err)
	}
	var current string
	if err := s.db.QueryRow(ctx, `SELECT policy_id FROM project_policies WHERE organization_id=$1 AND project_id=$2`, s.organization, project).Scan(&current); err != nil {
		return mapError(ctx, err)
	}
	if current != policyID {
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
	if errors.Is(err, pgx.ErrNoRows) {
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

func sameProjectPolicyContent(left, right policystore.Revision) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyID == right.PolicyID && sameOptionalBool(left.GitHubPullRequest, right.GitHubPullRequest) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
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
