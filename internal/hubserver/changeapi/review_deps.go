package changeapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReviewDeps struct {
	Repository                          tenantstore.Repository
	CurrentTime                         func() (time.Time, error)
	BoardRefresh                        chan<- struct{}
	RunDetailContext                    func(ctx context.Context) context.Context
	RunDetailStore                      func() attachment.DetailStore
	NativeAPIError                      func(c echo.Context, err error, attributes ...any) error
	ChangeOperationRecordReceipt        func(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID, operation, key, hash string) (json.RawMessage, bool, error)
	ReadChangeRecord                    func(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error)
	ReadChangeVersionRecord             func(ctx context.Context, organization tracker.OrganizationID, changeID, versionID string) (eventstore.Version, error)
	ReadWorkItemRecord                  func(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	HydrateWorkItemRecord               func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	LockChangeRecordOperation           func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item, changeID, versionID string) (eventstore.Change, error)
	ValidateReviewBundleRecord          func(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, bundle tracker.ChangeReviewBundle, now time.Time) error
	RegisterChangeContentRecord         func(ctx context.Context, w tenantstore.Writer, organization string, ref attachment.ArchiveReference, billable, withinAllowance bool) error
	CheckChangeContentAllowance         func(ctx context.Context, organization string, refs map[string]attachment.ArchiveReference, next attachment.ArchiveReference) error
	HydrateCurrentChangeVersionApproval func(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows, policyID string) (hubrecords.ChangeVersionApproval, error)
	NativeRejection                     func(err error, category, field string) error
	PrepareChangeReviewWorkflow         func(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, review tracker.ChangeReview, approval hubrecords.ChangeVersionApproval, now time.Time) (*changelanding.ReviewWorkflow, error)
	RecheckChangeReviewWorkflow         func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.ReviewWorkflow, current eventstore.Change) (bool, error)
	LockChangeWorkflowTransition        func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) (bool, error)
	ApplyChangeWorkflowTransition       func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) error
	Validator                           ValidatorDeps
	AttachmentReferences                changefiles.AttachmentReferences
	PrepareRepairs                      func(context.Context, hubscope.NativeScope, tracker.NativeWorkItemID, []failureattribution.Evidence, string, time.Time) (*PreparedRepairs, error)
	ChangeReviewContentFields           func(tracker.ReviewChange) []changefiles.ContentField
}

type Review struct {
	deps      ReviewDeps
	validator *validator
}

func NewReview(deps ReviewDeps) *Review {
	return &Review{deps: deps, validator: newValidator(deps.Validator)}
}

type PreparedRepairs struct {
	Dependencies []tracker.NativeWorkItemID
	Refusal      string
	Check        func(context.Context, tenantstore.Writer, hubscope.NativeScope) (string, error)
	Apply        func(context.Context, tenantstore.Writer, hubscope.NativeScope) error
}
