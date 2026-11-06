package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChangeVersionApproval struct {
	Policy       policy.Descriptor
	ReviewPolicy tracker.ChangeReviewPolicy
}

type ChangeVersionAuthority interface {
	Prepare(context.Context, nativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, time.Time) (ChangeVersionApproval, error)
	Authorize(context.Context, tenantstore.Writer, nativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, ChangeVersionApproval, time.Time) error
}
