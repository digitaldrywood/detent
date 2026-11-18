package maintenanceprobe

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader struct {
	pool    *pgxpool.Pool
	acquire func(context.Context, corestore.Workload) (func(), error)
}

func New(pool *pgxpool.Pool, acquire func(context.Context, corestore.Workload) (func(), error)) Reader {
	return Reader{pool: pool, acquire: acquire}
}

func (r Reader) GitHubOutboundDeadline(ctx context.Context, org tracker.OrganizationID) (time.Time, error) {
	if org == "" {
		return time.Time{}, corestore.ErrTenantMismatch
	}
	release, err := r.acquire(ctx, corestore.Background)
	if err != nil {
		return time.Time{}, err
	}
	defer release()
	const query = "SELECT public.github_outbound_deadline($1)"
	defer pgcore.ObserveStatement(ctx, time.Now(), query)
	var next *time.Time
	if err := r.pool.QueryRow(ctx, query, org).Scan(&next); err != nil {
		return time.Time{}, pgcore.MapError(ctx, err)
	}
	if next == nil {
		return time.Time{}, nil
	}
	return *next, nil
}

func (r Reader) due(ctx context.Context, org tracker.OrganizationID, query string, args ...any) (bool, error) {
	if org == "" {
		return false, corestore.ErrTenantMismatch
	}
	release, err := r.acquire(ctx, corestore.Background)
	if err != nil {
		return false, err
	}
	defer release()
	var due bool
	defer pgcore.ObserveStatement(ctx, time.Now(), query)
	err = r.pool.QueryRow(ctx, query, args...).Scan(&due)
	return due, pgcore.MapError(ctx, err)
}
