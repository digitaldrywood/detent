package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) IssueDispatchState(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (tenantstore.DispatchState, error) {
	var state tenantstore.DispatchState
	statement := `SELECT revision,dispatch_generation,dispatch_requested_at FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`
	if !s.readOnly {
		statement += ` FOR NO KEY UPDATE`
	}
	err := s.db.QueryRow(ctx, statement, s.organization, project, item).Scan(&state.Revision, &state.Generation, &state.RequestedAt)
	return state, mapError(ctx, err)
}

func (s *session) RequestIssueDispatch(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, fence leasestore.Fence, at time.Time) error {
	return s.requestIssueDispatch(ctx, project, item, fence, at, false)
}

func (s *session) QueueIssueDispatch(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, fence leasestore.Fence, at time.Time) error {
	return s.requestIssueDispatch(ctx, project, item, fence, at, true)
}

func (s *session) requestIssueDispatch(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, fence leasestore.Fence, at time.Time, queued bool) error {
	if at.IsZero() {
		return tracker.ErrInvalidClaimRequest
	}
	current, err := s.LockWorkItem(ctx, project, item)
	if err != nil {
		return err
	}
	if current.Archived {
		return corestore.ErrRevisionConflict
	}
	state, err := s.IssueDispatchState(ctx, project, item)
	if err != nil {
		return err
	}
	if state.Revision != current.Revision {
		return tracker.ErrStaleFencingToken
	}
	attempt, err := s.LatestAttempt(ctx, current.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if err == nil {
		if attempt.LeaseID != fence.LeaseID || attempt.FencingToken != fence.FencingToken {
			return tracker.ErrStaleFencingToken
		}
	} else if fence != (leasestore.Fence{}) {
		return tracker.ErrStaleFencingToken
	}
	if lease, err := s.ActiveLeaseForIssue(ctx, current.ID); err == nil {
		if !queued || lease.LeaseID != fence.LeaseID || lease.FencingToken != fence.FencingToken {
			return tracker.ErrLeaseConflict
		}
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	result, err := s.db.Exec(ctx, `UPDATE issues SET dispatch_generation=GREATEST(dispatch_generation,$5)+1,dispatch_requested_at=$4 WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 AND revision=$6`, s.organization, project, item, at.UTC(), attempt.DispatchGeneration, current.Revision)
	return updateOwned(ctx, result, err)
}

var _ tenantstore.DispatchWriter = (*session)(nil)
