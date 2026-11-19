package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (p tenantNativeRunEventParticipant) RequestDispatch(ctx context.Context, writer tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time) error {
	return conversations.RequestDispatch(ctx, writer, project, item, fence, at, false, conversationDispatchUnavailable)
}

func (p tenantNativeRunEventParticipant) QueueDispatch(ctx context.Context, writer tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time) error {
	return conversations.RequestDispatch(ctx, writer, project, item, fence, at, true, conversationDispatchUnavailable)
}
