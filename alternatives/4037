package tenantstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DispatchState struct {
	Revision    int64
	Generation  int64
	RequestedAt *time.Time
}

type DispatchReader interface {
	IssueDispatchState(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (DispatchState, error)
}

type DispatchWriter interface {
	DispatchReader
	RequestIssueDispatch(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, leasestore.Fence, time.Time) error
	QueueIssueDispatch(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, leasestore.Fence, time.Time) error
}
