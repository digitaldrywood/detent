package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReadScheduledDecision(ctx context.Context, project tracker.ProjectID, input progresscheck.CIFailure) (progresscheck.CIDecision, error) {
	row, err := s.queries.ReadScheduledDecision(ctx, pgquery.ReadScheduledDecisionParams{OrganizationID: s.organization, ProjectID: project, Repository: input.Repository, RunID: input.RunID, RunAttempt: input.RunAttempt, JobID: input.JobID})
	if err != nil {
		return progresscheck.CIDecision{}, mapError(ctx, err)
	}
	if row.HeadSHA != input.HeadSHA {
		return progresscheck.CIDecision{}, corestore.ErrRevisionConflict
	}
	result := progresscheck.CIDecision{Outcome: progresscheck.CIOutcome(row.Outcome), Target: row.Target, Reason: row.Reason}
	return result, result.Validate()
}

func (s *session) StoreScheduledDecision(ctx context.Context, project tracker.ProjectID, input progresscheck.CIFailure, result progresscheck.CIDecision, at time.Time, limit int64) (billingstore.UsageResult, error) {
	if project == "" || input.Validate() != nil || result.Validate() != nil || at.IsZero() {
		return billingstore.UsageResult{}, eventstore.ErrInvalidRecord
	}
	bytes := scheduledDecisionBytes(s.organization, project, input, result)
	inserted, err := s.queries.StoreScheduledDecision(ctx, pgquery.StoreScheduledDecisionParams{
		OrganizationID: s.organization, ProjectID: project, Repository: input.Repository, HeadSHA: input.HeadSHA,
		RunID: input.RunID, RunAttempt: input.RunAttempt, JobID: input.JobID, Outcome: string(result.Outcome),
		Target: result.Target, Reason: result.Reason, CreatedAt: at.UTC(), CollaborationBytes: bytes,
	})
	if err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	if inserted == 0 {
		return billingstore.UsageResult{Allowed: true}, nil
	}
	return s.AdjustCollaborationBytes(ctx, bytes, limit)
}

func (s *session) UpdateScheduledDecision(ctx context.Context, project tracker.ProjectID, input progresscheck.CIFailure, result progresscheck.CIDecision, limit int64) (billingstore.UsageResult, error) {
	if project == "" || input.Validate() != nil || result.Validate() != nil {
		return billingstore.UsageResult{}, eventstore.ErrInvalidRecord
	}
	bytes := scheduledDecisionBytes(s.organization, project, input, result)
	previous, err := s.queries.UpdateScheduledDecision(ctx, pgquery.UpdateScheduledDecisionParams{
		OrganizationID: s.organization, ProjectID: project, Repository: input.Repository, HeadSHA: input.HeadSHA,
		RunID: input.RunID, RunAttempt: input.RunAttempt, JobID: input.JobID, Outcome: string(result.Outcome),
		Target: result.Target, Reason: result.Reason, CollaborationBytes: bytes,
	})
	if err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	return s.AdjustCollaborationBytes(ctx, bytes-previous, limit)
}

func scheduledDecisionBytes(org tracker.OrganizationID, project tracker.ProjectID, input progresscheck.CIFailure, result progresscheck.CIDecision) int64 {
	return int64(len(org) + len(project) + len(input.Repository) + len(input.HeadSHA) + len(input.RunID) + len(input.RunAttempt) + len(input.JobID) + len(result.Outcome) + len(result.Target) + len(result.Reason) + 8)
}

func (s *session) ClaimScheduledClassification(ctx context.Context, project tracker.ProjectID, jobKey, receiptID string, now, expiresAt time.Time) (bool, error) {
	if project == "" || jobKey == "" || receiptID == "" || now.IsZero() || !expiresAt.After(now) {
		return false, eventstore.ErrInvalidRecord
	}
	count, err := s.queries.ClaimScheduledClassification(ctx, pgquery.ClaimScheduledClassificationParams{
		OrganizationID: s.organization, ProjectID: project, JobKey: jobKey, ReceiptID: receiptID, ExpiresAt: expiresAt, AsOf: now,
	})
	return count == 1, mapError(ctx, err)
}

func (s *session) ReleaseScheduledClassification(ctx context.Context, project tracker.ProjectID, jobKey, receiptID string, validAt *time.Time) (bool, error) {
	count, err := s.queries.ReleaseScheduledClassification(ctx, pgquery.ReleaseScheduledClassificationParams{
		OrganizationID: s.organization, ProjectID: project, JobKey: jobKey, ReceiptID: receiptID, ValidAt: validAt,
	})
	return count == 1, mapError(ctx, err)
}

func (s *session) ScheduledLandedWorkItems(ctx context.Context, project tracker.ProjectID, sha string) ([]string, error) {
	rows, err := s.queries.ScheduledLandedWorkItems(ctx, pgquery.ScheduledLandedWorkItemsParams{OrganizationID: s.organization, ProjectID: project, LandedMergeSHA: new(sha)})
	result := make([]string, len(rows))
	for i, row := range rows {
		result[i] = string(row)
	}
	return result, mapError(ctx, err)
}
