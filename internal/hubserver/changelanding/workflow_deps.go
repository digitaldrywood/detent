package changelanding

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type WorkflowDeps struct {
	SettleLease                            func(context.Context, tenantstore.Writer, hubscope.NativeScope, leasestore.Lease, tracker.Transition, LeaseCancellation, string, func() time.Time) error
	Repository                             tenantstore.Repository
	Clock                                  func() time.Time
	ConversationDispatch                   tenantstore.DispatchRequests
	ReadWorkItemRecord                     func(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	HydrateWorkItemRecord                  func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	ReadProjectRecord                      func(ctx context.Context, scope hubscope.NativeScope) (projectstore.Record, error)
	RunDetailStore                         func() attachment.DetailStore
	NativeIssueResponse                    func(issue tracker.NativeIssue) tracker.NativeIssue
	PrepareCompatibilityWorkflowLabel      func(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, project projectstore.Record, label, key string, now time.Time) (*projectstore.GitHubOutbound, error)
	ValidateTenantTransitionIssueContract  func(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue, project *projectstore.Record, previousState string, request tracker.Transition, stopState, workflowLabel *string) (policystore.Revision, bool, error)
	AuthorizeWorkItemRecordScope           func(ctx context.Context, reader accessstore.Reader, scope hubscope.NativeScope) error
	AppendBoundedChangeEvidence            func(detail *tracker.ChangeDetail, row eventstore.Evidence, raw []byte) error
	PrepareValidatorAuditDispositions      func(ctx context.Context, scope hubscope.NativeScope, changeID string, run securityaudit.Run) ([]securityaudit.Disposition, []eventstore.AuditDisposition, error)
	CutoverSourceReference                 func(issue tracker.NativeIssue) (string, int, error)
	ValidateConfiguredWorkflowPolicy       func(project projectstore.Record, descriptor policy.Descriptor, found bool) error
	SameNativeClaimLinkedSource            func(left, right eventstore.LinkedSource) bool
	SameTenantProjectPolicyRevisionContent func(left, right policystore.Revision) bool
	ConversationDispatchUnavailable        func() error
	PrepareLeaseCancellation               func(context.Context, hubscope.NativeScope, int64) (LeaseCancellation, error)
}

type Workflow struct {
	deps WorkflowDeps
}

func NewWorkflow(deps WorkflowDeps) *Workflow {
	return &Workflow{deps: deps}
}

type LeaseCancellation func(context.Context, tenantstore.Writer, hubscope.NativeScope, leasestore.Lease, tracker.Transition, string, func() time.Time) error
