package maintenanceprobe

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r Reader) MaintenanceDue(ctx context.Context, org, family string, now time.Time, linkedIntake bool) (bool, error) {
	return r.due(ctx, tracker.OrganizationID(org), "SELECT public.maintenance_family_due($1,$2,$3,$4)", org, family, now, linkedIntake)
}

func (r Reader) SweepDue(ctx context.Context, org, family, after string, limit int) (string, bool, error) {
	if org == "" {
		return after, false, corestore.ErrTenantMismatch
	}
	var query string
	args := []any{org, after}
	switch family {
	case "board":
		query = "SELECT next_id,due FROM public.board_sweep_due($1,$2)"
	case "project-pages", "integration-pages":
		query = "SELECT next_id,due FROM public.project_sweep_due($1,$2,$3)"
		args = []any{org, family, after}
	case "usage":
		query = "SELECT next_id,due FROM public.usage_sweep_due($1,$2,$3)"
		args = append(args, limit)
	default:
		return after, false, corestore.ErrInvalidPage
	}
	release, err := r.acquire(ctx, corestore.Background)
	if err != nil {
		return after, false, err
	}
	defer release()
	var next string
	var due bool
	defer pgcore.ObserveStatement(ctx, time.Now(), query)
	err = r.pool.QueryRow(ctx, query, args...).Scan(&next, &due)
	return next, due, pgcore.MapError(ctx, err)
}
