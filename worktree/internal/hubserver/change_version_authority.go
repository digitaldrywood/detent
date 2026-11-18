package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChangeVersionAuthority interface {
	Prepare(context.Context, hubscope.NativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, time.Time) (hubrecords.ChangeVersionApproval, error)
	Authorize(context.Context, tenantstore.Writer, hubscope.NativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, hubrecords.ChangeVersionApproval, time.Time) error
}
