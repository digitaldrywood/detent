package changelanding

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type LandingDeps struct {
	Repository              tenantstore.Repository
	CurrentTime             func() (time.Time, error)
	BoardRefresh            chan<- struct{}
	SourceVerification      chan struct{}
	RunDetailContext        func(ctx context.Context) context.Context
	RunDetailStore          func() attachment.DetailStore
	NativeAPIError          func(c echo.Context, err error, attributes ...any) error
	InvalidAPIRequest       func(c echo.Context, err error) error
	AuthorizeAccessScope    func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error
	ChangeRecordReceipt     func(ctx context.Context, scope hubscope.NativeScope, operation, key string, hashes ...string) (json.RawMessage, bool, error)
	ReadChangeRecord        func(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error)
	ReadChangeVersionRecord func(ctx context.Context, organization tracker.OrganizationID, changeID, versionID string) (eventstore.Version, error)
	ReadWorkItemRecord      func(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	HydrateWorkItemRecord   func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	RunnerSessionFence      func(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, lock bool) (leasestore.Attempt, policystore.Revision, error)
	SelectedChangeDetail    func(ctx context.Context, scope hubscope.NativeScope, item, id string, now time.Time) (tracker.ChangeDetail, error)
}

type Landing struct {
	deps LandingDeps
}

func NewLanding(deps LandingDeps) *Landing {
	return &Landing{deps: deps}
}
