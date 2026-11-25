package changeapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type VersionPublicationDeps struct {
	RunEventsAvailable func() bool
	ValidateRunOwner   func(context.Context, tenantstore.Reader, VersionRunAuthority) error
	ExecutionConflict  func(string) error
	Repository         tenantstore.Repository
	CurrentTime        func() (time.Time, error)
	BoardRefresh       chan<- struct{}
	Authority          VersionAuthority
	DefaultAuthority   func() VersionAuthority

	ValidateVersionAttachments      func(context.Context, tenantstore.Reader, hubscope.NativeScope, string, []tracker.ChangeArtifact) error
	BindVersionAttachments          func(context.Context, tenantstore.Writer, hubscope.NativeScope, string, []tracker.ChangeArtifact) error
	RunDetailContext                func(ctx context.Context) context.Context
	RunDetailStore                  func() attachment.DetailStore
	NativeAPIError                  func(c echo.Context, err error, attributes ...any) error
	InvalidAPIRequest               func(c echo.Context, err error) error
	ChangeOperationRecordReceipt    func(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID, operation, key, hash string) (json.RawMessage, bool, error)
	ReadChangeRecord                func(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error)
	VerifyChangeSource              func(ctx context.Context, source tracker.ChangeSource, bundle []byte) error
	LockChangeRecordOperation       func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item, changeID, versionID string) (eventstore.Change, error)
	PrepareChangeWorkflowTransition func(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, approval hubrecords.ChangeVersionApproval, rework bool, now time.Time) (*changelanding.WorkflowTransition, error)
	LockChangeWorkflowTransition    func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) (bool, error)
	ApplyChangeWorkflowTransition   func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changelanding.WorkflowTransition) error
	LockRunnerCredential            func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope) error
	NativeRejection                 func(err error, category, field string) error
	ValidateChangeVersion           func(input tracker.ChangeVersionInput) error
	ValidateChangeVersionEvidence   func(scope hubscope.NativeScope, item string, request tracker.PublishChangeVersion) error
}

type VersionPublication struct {
	deps VersionPublicationDeps
}

func NewVersionPublication(deps VersionPublicationDeps) *VersionPublication {
	return &VersionPublication{deps: deps}
}

type VersionAuthority interface {
	Prepare(context.Context, hubscope.NativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, time.Time) (hubrecords.ChangeVersionApproval, error)
	Authorize(context.Context, tenantstore.Writer, hubscope.NativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, hubrecords.ChangeVersionApproval, time.Time) error
}

type VersionRunAuthority struct {
	Scope              hubscope.NativeScope
	OrganizationID     tracker.OrganizationID
	ProjectID          tracker.ProjectID
	WorkItem           corestore.WorkItem
	Lease              leasestore.Lease
	DispatchGeneration int64
	UsageCorrection    bool
	Event              tracker.NativeRunEvent
}
