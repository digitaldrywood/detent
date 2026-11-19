package releasefeed

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DeliveryDeps struct {
	Repository                          tenantstore.Repository
	Clock                               func() time.Time
	BoardRefresh                        chan<- struct{}
	RunDetailContext                    func(ctx context.Context) context.Context
	RunDetailStore                      func() attachment.DetailStore
	InvalidAPIRequest                   func(c echo.Context, err error) error
	NativeAPIError                      func(c echo.Context, err error, attributes ...any) error
	RecheckProjectRecordMutation        func(ctx context.Context, scope hubscope.NativeScope) error
	AuthorizeWorkItemRecordScope        func(ctx context.Context, reader accessstore.Reader, scope hubscope.NativeScope) error
	ReadWorkItemRecord                  func(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	HydrateWorkItemRecord               func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	ReadChangeRecord                    func(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error)
	NativeIssueResponse                 func(issue tracker.NativeIssue) tracker.NativeIssue
	PrepareWorkItemFields               func(ctx context.Context, organization tracker.OrganizationID, plan hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
	PrepareChangeWorkflowTransition     func(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, approval hubrecords.ChangeVersionApproval, rework bool, now time.Time) (*changelanding.WorkflowTransition, error)
	LockChangeWorkflowTransition        func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) (bool, error)
	ApplyChangeWorkflowTransition       func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) error
	HydrateNativeAttemptTenant          func(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot, now time.Time) (tracker.NativeAttempt, error)
	HydrateCurrentChangeVersionApproval func(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows, policyID string) (hubrecords.ChangeVersionApproval, error)
	AnsweredClaimKind                   func(data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint) string
	ApproveReleaseDeliveryContract      func(issue *tracker.NativeIssue, approved policy.Descriptor, now time.Time) error
	ProjectRecordActor                  func(scope hubscope.NativeScope) tenantstore.ProjectActor
	ValidCheckoutRepository             func(repository string) bool

	ValidateReleasePick         func(r tracker.AssociateReleasePick) error
	ValidatePreparedReleasePick func(r tracker.ReleasePickInput, revision tracker.Revision) error
	SameReleasePickDelivery     func(prior, next tracker.ReleasePickInput) bool
	RequireCollaboration        func(context.Context, tenantstore.Reader, time.Time) error
	HasPlans                    func() bool
	CheckoutRepository          func(context.Context, hubscope.NativeScope) (string, bool, error)
	InvalidatePullRequests      func(hubscope.NativeScope, string)
	DeliveryVerifier            func() (githubcontract.ReleaseDeliveryVerifier, bool)
	PickVerifier                func() (githubcontract.ReleasePickVerifier, bool)
	PickPublisher               func() (githubcontract.ReleasePickPublisher, bool)
}

type Delivery struct {
	deps DeliveryDeps
}

func NewDelivery(deps DeliveryDeps) *Delivery {
	return &Delivery{deps: deps}
}
