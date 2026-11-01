package tenantstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	billingstore.Reader
	accessstore.Reader
	corestore.Reader
	projectstore.Reader
	eventstore.Reader
	leasestore.Reader
}

type Writer interface {
	billingstore.Writer
	accessstore.Writer
	corestore.Writer
	projectstore.Writer
	eventstore.Writer
	leasestore.Writer
}

type Repository interface {
	ReadTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}

type ProjectActor struct {
	CredentialID  string
	PrincipalID   string
	HostedSubject string
}

type ProjectParticipants interface {
	CreateProject(context.Context, Writer, ProjectActor, projectstore.Record) error
	UpdateProject(context.Context, Writer, ProjectActor, projectstore.Record, projectstore.Record) error
	RetireProject(context.Context, Writer, ProjectActor, projectstore.Record, time.Time) error
}
