package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (p tenantNativeRunEventParticipant) RequestDispatch(ctx context.Context, writer tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time) error {
	return p.requestDispatch(ctx, writer, project, item, fence, at, false)
}

func (p tenantNativeRunEventParticipant) QueueDispatch(ctx context.Context, writer tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time) error {
	return p.requestDispatch(ctx, writer, project, item, fence, at, true)
}

func (p tenantNativeRunEventParticipant) requestDispatch(ctx context.Context, writer tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time, queued bool) error {
	dispatch, ok := writer.(tenantstore.DispatchWriter)
	if !ok {
		return conversationDispatchUnavailable()
	}
	if fence == (leasestore.Fence{}) {
		current, err := writer.LockWorkItem(ctx, project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		attempt, err := writer.LatestAttempt(ctx, current.ID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			fence = leasestore.Fence{LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken}
		}
	}
	if queued {
		return dispatch.QueueIssueDispatch(ctx, project, tracker.NativeWorkItemID(item), fence, at)
	}
	return dispatch.RequestIssueDispatch(ctx, project, tracker.NativeWorkItemID(item), fence, at)
}
