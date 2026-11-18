package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/roleprovision"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrTenantRequired = pgtransport.ErrTenantRequired
var ErrRuntimeRole = roleprovision.ErrRuntimeRole

type Runtime struct {
	reader *pgxpool.Pool
	writer *pgxpool.Pool
	pools  *pgtransport.Pools
}

func NewRuntime(ctx context.Context, reader, writer *pgxpool.Pool) (*Runtime, error) {
	for _, pool := range []*pgxpool.Pool{reader, writer} {
		if err := roleprovision.ValidateRuntime(ctx, pool); err != nil {
			return nil, err
		}
	}
	pools, err := pgtransport.Adopt(reader, writer)
	if err != nil {
		return nil, err
	}
	return &Runtime{reader: reader, writer: writer, pools: pools}, nil
}

func (r *Runtime) Begin(ctx context.Context, organization tracker.OrganizationID, options pgtransport.Options) (*pgtransport.Transaction, error) {
	return r.pools.Begin(ctx, pgtransport.Scope{Organization: organization}, options)
}
