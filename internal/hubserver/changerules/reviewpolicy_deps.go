package changerules

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReviewPolicyDeps struct {
	PrepareStaleLanding          func(context.Context, hubscope.NativeScope, hubrecords.ChangeVersionApproval, time.Time) (ApplyStaleLanding, bool, error)
	Repository                   tenantstore.Repository
	CurrentTime                  func() (time.Time, error)
	BoardRefresh                 chan<- struct{}
	RunDetailStore               func() attachment.DetailStore
	RunDetailContext             func(ctx context.Context) context.Context
	NativeAPIError               func(c echo.Context, err error, attributes ...any) error
	RecheckProjectRecordMutation func(ctx context.Context, scope hubscope.NativeScope) error
	AuthorizeAccessScope         func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error
	ChangeRecordReceipt          func(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (json.RawMessage, bool, error)

	HydrateChangeSummaryPolicy func(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows) (hubrecords.ChangeVersionApproval, error)
	FollowsRepositoryGate      func(rules tracker.ChangeReviewPolicy) bool
}

type ReviewPolicy struct {
	deps ReviewPolicyDeps
}

func NewReviewPolicy(deps ReviewPolicyDeps) *ReviewPolicy {
	return &ReviewPolicy{deps: deps}
}

type ApplyStaleLanding func(context.Context, tenantstore.Writer) error
