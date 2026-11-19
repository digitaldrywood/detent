package changeapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RecordMutationsDeps struct {
	Repository                        tenantstore.Repository
	Clock                             func() time.Time
	BoardRefresh                      chan<- struct{}
	CurrentTime                       func() (time.Time, error)
	RunDetailContext                  func(ctx context.Context) context.Context
	RunDetailStore                    func() attachment.DetailStore
	NativeAPIError                    func(c echo.Context, err error, attributes ...any) error
	ReadChangeRecord                  func(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error)
	ReadChangeVersionRecord           func(ctx context.Context, organization tracker.OrganizationID, changeID, versionID string) (eventstore.Version, error)
	ChangeRecordReceipt               func(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (json.RawMessage, bool, error)
	ReadWorkItemRecord                func(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	AuthorizeAccessScope              func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error
	AuthorizeWorkItemRecordScope      func(ctx context.Context, reader accessstore.Reader, scope hubscope.NativeScope) error
	RunnerSessionFence                func(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, lock bool) (leasestore.Attempt, policystore.Revision, error)
	RecheckHostedChangeCheckPrincipal func(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, approved eventstore.ReviewPolicy) error
	RequireHostedChangeCheckPrincipal func(ctx context.Context, scope hubscope.NativeScope) (eventstore.ReviewPolicy, error)
	ChangeReviewRecordMutation        func(c echo.Context, request tracker.ReviewChange) error
	HydrateChangeSummaryPolicy        func(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows) (hubrecords.ChangeVersionApproval, error)
	ValidateNativeProvenance          func(scope hubscope.NativeScope, provenance *tracker.Provenance) error
	PrepareQualityEscapeTenant        func(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.DiscussChange, now time.Time) (*changerules.PreparedQualityEscape, error)
	PrepareChangeCheckWorkflow        func(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, check tracker.ChangeCheck, approval hubrecords.ChangeVersionApproval, now time.Time) (*changelanding.ReviewWorkflow, error)
	RecheckChangeReviewWorkflow       func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.ReviewWorkflow, current eventstore.Change) (bool, error)
	LockChangeWorkflowTransition      func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) (bool, error)
	ApplyChangeWorkflowTransition     func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) error
	Hosted                            bool
	InvalidClock                      error
	HasPlans                          func() bool
	RequireCollaboration              func(context.Context, tenantstore.Reader, time.Time) error
	AttachmentReferences              changefiles.AttachmentReferences
}

type RecordMutations struct {
	deps RecordMutationsDeps
}

func NewRecordMutations(deps RecordMutationsDeps) *RecordMutations {
	return &RecordMutations{deps: deps}
}
