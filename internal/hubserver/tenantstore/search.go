package tenantstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SearchRepository struct {
	Repository
}

func (r SearchRepository) ReadSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(searchstore.Reader) error) error {
	return r.ReadTenant(ctx, organization, workload, func(reader Reader) error { return read(reader) })
}

func (r SearchRepository) WriteSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(searchstore.Writer) error) error {
	return r.WriteTenant(ctx, organization, workload, func(writer Writer) error { return write(writer) })
}
