package tenantstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	corestore.Reader
	eventstore.Reader
	leasestore.Reader
}

type Writer interface {
	corestore.Writer
	eventstore.Writer
	leasestore.Writer
}

type Repository interface {
	ReadTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}
