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
	return dispatch.RequestIssueDispatch(ctx, project, tracker.NativeWorkItemID(item), fence, at)
}
