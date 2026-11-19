package contentrecords

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AttachmentRecordDeps struct {
	Now                   func() time.Time
	ReadTenant            func(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error
	WriteTenant           func(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Writer) error) error
	Authorize             func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	CheckEvidence         func(echo.Context, *attachment.EvidenceRequest) error
	APIError              func(echo.Context, error, ...any) error
	SharedClaims          func(echo.Context) (cloudassert.Claims, bool)
	Plans                 func() *hostedplans.Service
	Charge                func(context.Context, tenantstore.Writer, tracker.OrganizationID, string, time.Time, ...string) error
	LeaseMatches          func(runnerstore.Identity, tracker.OrganizationID, string, string, tracker.MachineID, time.Time) bool
	RequireReporterTriage func(hubscope.NativeScope, tracker.NativeIssue) error
	EscapeEvidenceCaption func(string) string
	ReadWorkItem          func(context.Context, hubscope.NativeScope, string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	Hydrate               func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	HydrateCanonical      func(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	PrepareFields         func(context.Context, tracker.OrganizationID, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
	Receipt               func(context.Context, hubscope.NativeScope, string, string, string) (eventstore.Receipt, bool, error)
	Details               func() attachment.DetailStore
}

type AttachmentRecords struct{ deps AttachmentRecordDeps }

func NewAttachmentRecords(deps AttachmentRecordDeps) AttachmentRecords {
	return AttachmentRecords{deps: deps}
}

type ArtifactRecordDeps struct {
	StorageAvailable       bool
	Now                    func() time.Time
	ReadTenant             func(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error
	WriteTenant            func(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Writer) error) error
	WebSession             func(context.Context, string, time.Time) (auth.Session, error)
	HostedMutationIdentity func(context.Context, hubapi.APICredential) (auth.HostedIdentity, auth.Membership, error)
	RefuseStaffSession     func(auth.Session) bool
	Authorize              func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	LeasePolicy            func(context.Context, hubscope.NativeScope, tracker.LeaseID) (policystore.Revision, policy.Requirements, error)
	AuthorizeLease         func(context.Context, tenantstore.Writer, hubscope.NativeScope, corestore.WorkItem, tracker.Mutation, policystore.Revision, policy.Requirements, time.Time) error
	Charge                 func(context.Context, billingstore.Writer, time.Time, string, bool, bool) error
}

type ArtifactRecords struct{ deps ArtifactRecordDeps }

func NewArtifactRecords(deps ArtifactRecordDeps) ArtifactRecords {
	return ArtifactRecords{deps: deps}
}

type PreferenceIssueDeps struct {
	LoadPlan      func(context.Context, hubscope.NativeScope, string, string, any) (hubrecords.WorkItemRecordPlan, error)
	Authorize     func(context.Context, hubscope.NativeScope) error
	Now           func() time.Time
	PrepareFields func(context.Context, tracker.OrganizationID, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
	Details       func() attachment.DetailStore
}
