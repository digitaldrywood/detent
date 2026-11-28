package coordinator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type IssueProposalDeps struct {
	ProposalDeps
	Commands IssueCommandDeps
}

type IssueCommandDeps struct {
	StorageAvailable   bool
	ReadTenant         func(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error
	WriteTenant        func(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Writer) error) error
	Now                func() time.Time
	Operation          func(hubscope.NativeScope, string, string) string
	HydrateIssue       func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	ReadWorkItem       func(context.Context, hubscope.NativeScope, string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	ReadProject        func(context.Context, hubscope.NativeScope) (projectstore.Record, error)
	BeginAuthority     func(context.Context, hubscope.NativeScope) (interface{ Rollback() error }, error)
	RecheckMutation    func(context.Context, hubscope.NativeScope) error
	Authorize          func(context.Context, accessstore.Reader, hubscope.NativeScope) error
	LoadArchivePlan    func(context.Context, hubscope.NativeScope, string, tracker.Revision) (hubrecords.WorkItemRecordPlan, error)
	PlanArchive        func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan, tracker.Revision) (hubrecords.WorkItemRecordPlan, error)
	LoadPlan           func(context.Context, hubscope.NativeScope, string, string, any) (hubrecords.WorkItemRecordPlan, error)
	PlanWorkItem       func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan, any) (hubrecords.WorkItemRecordPlan, error)
	PrepareFields      func(context.Context, tracker.OrganizationID, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
	IssueResponse      func(tracker.NativeIssue) tracker.NativeIssue
	Receipt            func(context.Context, hubscope.NativeScope, string, string, string) (eventstore.Receipt, bool, error)
	RecordEntryAudit   func(context.Context, tenantstore.Writer, *hubscope.EntryKeyAudit, int) error
	SignalDispatch     func(hubscope.NativeScope)
	Details            func() attachment.DetailStore
	Plans              func() *hostedplans.Service
	ValidateCreate     func(context.Context, hubscope.NativeScope, projectstore.Record, tracker.CreateIssue) error
	ValidateFiling     func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan) (policystore.Revision, bool, error)
	PrepareAttachments func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemAttachmentReferences, error)
	AttachmentQuota    func(context.Context, tenantstore.Reader) (int64, error)
	WakeSpriteRunners  func(context.Context, hubscope.NativeScope, json.RawMessage)
}
