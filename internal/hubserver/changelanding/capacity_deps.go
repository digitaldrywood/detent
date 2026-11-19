package changelanding

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CapacityDeps struct {
	Repository                 tenantstore.Repository
	Clock                      func() time.Time
	NoClaimableWork            error
	HydrateCurrentChangeDetail func(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time) (tracker.ChangeDetail, error)
}

type Capacity struct {
	deps CapacityDeps
}

func NewCapacity(deps CapacityDeps) *Capacity {
	return &Capacity{deps: deps}
}
