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
	inserted, err := s.queries.StoreScheduledDecision(ctx, pgquery.StoreScheduledDecisionParams{OrganizationID: s.organization, ProjectID: project, Repository: input.Repository, HeadSHA: input.HeadSHA, RunID: input.RunID, RunAttempt: input.RunAttempt, JobID: input.JobID, Outcome: string(result.Outcome), Target: result.Target, Reason: result.Reason, CreatedAt: at.UTC()})
	if err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	if inserted == 0 {
		return billingstore.UsageResult{Allowed: true}, nil
	}
	bytes := len(s.organization) + len(project) + len(input.Repository) + len(input.HeadSHA) + len(input.RunID) + len(input.RunAttempt) + len(input.JobID) + len(result.Outcome) + len(result.Target) + len(result.Reason) + 8
	return s.AdjustCollaborationBytes(ctx, int64(bytes), limit)
}

func (s *session) ScheduledLandedWorkItems(ctx context.Context, project tracker.ProjectID, sha string) ([]string, error) {
	rows, err := s.queries.ScheduledLandedWorkItems(ctx, pgquery.ScheduledLandedWorkItemsParams{OrganizationID: s.organization, ProjectID: project, LandedMergeSHA: new(sha)})
	result := make([]string, len(rows))
	for i, row := range rows {
		result[i] = string(row)
	}
	return result, mapError(ctx, err)
}
