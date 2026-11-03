package tenantstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/gotestreceiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	threadstore.Reader
	viewstore.Reader
	billingstore.Reader
	claimstore.Reader
	accessstore.Reader
	artifactstore.Reader
	attachmentstore.Reader
	conversationstore.Reader
	gotestreceiptstore.Reader
	corestore.Reader
	projectstore.Reader
	eventstore.Reader
	leasestore.Reader
	policystore.Reader
	runnerstore.Reader
	searchstore.Reader
}

type Writer interface {
	threadstore.Writer
	viewstore.Writer
	billingstore.Writer
	claimstore.Writer
	accessstore.Writer
	artifactstore.Writer
	attachmentstore.Writer
	conversationstore.Writer
	gotestreceiptstore.Writer
	corestore.Writer
	projectstore.Writer
	eventstore.Writer
	leasestore.Writer
	policystore.Writer
	runnerstore.Writer
	searchstore.Writer
}

type Repository interface {
	ReadTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}

type ClaimPreviewPlanRowsReader interface {
	ClaimPreviewPlanRows(context.Context, tracker.ProjectID, []claimstore.Candidate) ([]ClaimPreviewPlanRow, error)
}

type ClaimPreviewPlanRow struct {
	Item                 corestore.WorkItem
	Version              eventstore.CollaborationVersion
	LinkedSource         *eventstore.LinkedSource
	Change               *eventstore.Change
	ChangeVersion        *eventstore.Version
	ChangePolicyRevision *policystore.Revision
	LatestAttempt        *leasestore.Attempt
	LatestRunEvent       *eventstore.Event
}

type SnapshotRepository interface {
	ReadTenantSnapshot(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
}

type ProjectActor struct {
	CredentialID  string
	PrincipalID   string
	HostedSubject string
	Audit         accessstore.Audit
}

type ProjectParticipants interface {
	CreateProject(context.Context, Writer, ProjectActor, projectstore.Record) error
	UpdateProject(context.Context, Writer, ProjectActor, projectstore.Record, projectstore.Record) error
	RetireProject(context.Context, Writer, ProjectActor, projectstore.Record, time.Time) error
}

type DispatchRequests interface {
	RequestDispatch(context.Context, Writer, tracker.ProjectID, string, leasestore.Fence, time.Time) error
}
