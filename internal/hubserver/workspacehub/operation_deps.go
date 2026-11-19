package workspacehub

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type OperationDeps struct {
	RequireWorkspaces                   func() error
	Repository                          func() tenantstore.Repository
	AccessRepository                    func() tenantstore.Repository
	Clock                               func() time.Time
	Config                              func() Config
	Plans                               func() *hostedplans.Service
	InvalidAPIRequest                   func(echo.Context, error) error
	NativeAPIError                      func(echo.Context, error, ...any) error
	ReadNativeAttemptTenant             func(context.Context, hubscope.NativeScope, string, string) (tracker.NativeAttempt, error)
	HydrateActionRecord                 func(context.Context, hubscope.NativeScope, actionstore.Record) (workspacesession.Action, error)
	RecheckProjectRecordMutation        func(context.Context, hubscope.NativeScope) error
	AuthorizeWorkItemRecordScope        func(context.Context, accessstore.Reader, hubscope.NativeScope) error
	LoadWorkItemPlan                    func(context.Context, hubscope.NativeScope, string, string, any) (hubrecords.WorkItemRecordPlan, error)
	PlanWorkItemRecord                  func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan, any) (hubrecords.WorkItemRecordPlan, error)
	ValidateWorkItemRecordFilingTenant  func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan) (policystore.Revision, bool, error)
	PrepareWorkItemAttachmentReferences func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemAttachmentReferences, error)
	PrepareWorkItemFields               func(context.Context, tracker.OrganizationID, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
	RunDetailStore                      func() attachment.DetailStore
	ReadProjectRecord                   func(context.Context, hubscope.NativeScope) (projectstore.Record, error)
	RecordEntryAccessKeyAuditWith       func(context.Context, tenantstore.Writer, *hubscope.EntryKeyAudit, int) error
	ChargeProjectCommand                func(context.Context, tenantstore.Writer, hubscope.NativeScope, string, string) error
	BeginProjectRecordAuthority         func(context.Context, hubscope.NativeScope) (interface{ Rollback() error }, error)
	AccessRead                          func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	AccessWrite                         func(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error
	AuthorizeAccessScope                func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	FirstDispatchableState              func(tracker.NativeProject) (string, bool)
	Committed                           func(context.Context, Record)
	PrepareCancellation                 func(context.Context, hubscope.NativeScope, string, tracker.NativeLeaseMutation) (PreparedCancellation, error)
	ReadRunnerAuthority                 func(context.Context, tenantstore.Reader, hubscope.NativeScope, string, time.Time) (runnerstore.Identity, error)
	UpdateWorkspaceState                func(context.Context, postgres.WorkspaceWriter, postgres.WorkspaceSession, postgres.WorkspaceSession, string, string, time.Time) (postgres.WorkspaceSession, error)
	WorkerIdentityFromHeaders           func(echo.Context) (WorkerIdentity, error)
	ValidActionRunReason                func(string) bool
	ForwardRunStatus                    func(string, string) bool
	ActionRunExitCode                   func(*int) (pgtype.Int4, error)
	ReadRelayWorker                     func(context.Context, tenantstore.Reader, hubscope.NativeScope, string, WorkerIdentity) (Record, error)
	PrepareRelayOutput                  func(context.Context, tracker.OrganizationID, []byte) (attachment.ArchiveReference, error)
	RelayArchiveReference               func(context.Context, tracker.OrganizationID, attachment.ArchiveReference) (attachment.ArchiveReference, error)
	RetainRelayArchive                  func(context.Context, tenantstore.Writer, tracker.OrganizationID, tracker.ProjectID, string, string, attachment.ArchiveReference, time.Time) error
	ActionRunResource                   func(context.Context, postgres.ActionRun, bool) (workspacesession.Run, error)
}

type Operations struct {
	OperationDeps
}

func NewOperations(deps OperationDeps) *Operations {
	return &Operations{OperationDeps: deps}
}

func (o *Operations) now() time.Time { return o.Clock().UTC() }

type PreparedCancellation struct {
	HasAttempt bool
	Record     func(context.Context, tenantstore.Writer, hubscope.NativeScope, leasestore.Lease) error
}
