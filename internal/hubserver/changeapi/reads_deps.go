package changeapi

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReadsDeps struct {
	Repository                           tenantstore.Repository
	Clock                                func() time.Time
	RunDetailStoreFor                    func(context.Context) attachment.DetailStore
	AuthorizeAccessScope                 func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	ReadChangeRecord                     func(context.Context, hubscope.NativeScope, string, string) (tracker.ChangeRequest, error)
	ReadChangeVersionRecord              func(context.Context, tracker.OrganizationID, string, string) (eventstore.Version, error)
	HydrateChangeVersion                 func(context.Context, hubscope.NativeScope, tracker.NativeWorkItemID, eventstore.Version) (tracker.ChangeVersion, error)
	ReadWorkItemRecord                   func(context.Context, hubscope.NativeScope, string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	HydrateWorkItemRecord                func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	LoadChangeExternalRecord             func(context.Context, hubscope.NativeScope, tracker.NativeWorkItemID, *tracker.ChangeDetail) error
	AccessAuthority                      func(hubscope.NativeScope, bool) accessstore.Authority
	ReadNativeAttemptSnapshotTenant      func(context.Context, hubscope.NativeScope, string, string) (hubrecords.TenantAttemptSnapshot, error)
	ReadNativeAttemptRunTenant           func(context.Context, hubrecords.TenantAttemptSnapshot) (tracker.NativeRunData, error)
	SummarizeChangeWithAuditDispositions func(context.Context, hubscope.NativeScope, tracker.ChangeDetail, string, string, time.Time, bool) (tracker.ChangeSummary, error)
	HydrateChangeSummaryPolicy           func(context.Context, *eventstore.Content, hubscope.NativeScope, hubrecords.CurrentChangePolicyRows) (hubrecords.ChangeVersionApproval, error)
	BoundedChangeVersion                 func(tracker.ChangeVersion) tracker.ChangeVersion
	ChangeDetailResponse                 func(tracker.ChangeDetail) tracker.ChangeDetail
	ValidChangeReadSection               func(string) bool
	ChangeLimit                          func(operatortool.ChangeArguments) int
	RecoveryRecordFragment               func(*tracker.NativeRecoveryPage, []byte, int64, string) error
	BoundReview                          func(*gate.ValidatorResult) *gate.ValidatorResult
}

type Reads struct {
	deps ReadsDeps
}

func NewReads(deps ReadsDeps) *Reads {
	return &Reads{deps: deps}
}
