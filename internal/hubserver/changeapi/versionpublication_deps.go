package changeapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type VersionPublicationDeps struct {
	Repository                      tenantstore.Repository
	CurrentTime                     func() (time.Time, error)
	BoardRefresh                    chan<- struct{}
	Authority                       VersionAuthority
	DefaultAuthority                func() VersionAuthority
	PrepareVersionSource            func(context.Context, hubscope.NativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion) (*VersionSource, error)
	ValidateVersionAttachments      func(context.Context, tenantstore.Reader, hubscope.NativeScope, []tracker.ChangeArtifact) error
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

type VersionSource struct {
	SessionID int64
	MachineID string
	Recheck   func(context.Context, tenantstore.Writer) error
}
