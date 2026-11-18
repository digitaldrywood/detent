package maintenanceprobe

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
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
	case "project-pages":
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

const claimIntegrationPage = "SELECT claimed_id,claimed_generation,claimed_at FROM public.claim_integration_page($1)"

func (r Reader) ClaimIntegrationPage(ctx context.Context, org tracker.OrganizationID) (projectstore.IntegrationCandidate, error) {
	release, err := r.acquire(ctx, corestore.Background)
	if err != nil {
		return projectstore.IntegrationCandidate{}, err
	}
	defer release()
	defer pgcore.ObserveStatement(ctx, time.Now(), claimIntegrationPage)
	return ClaimIntegrationPage(ctx, r.pool, org)
}

func ClaimIntegrationPage(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, org tracker.OrganizationID) (projectstore.IntegrationCandidate, error) {
	var candidate projectstore.IntegrationCandidate
	err := db.QueryRow(ctx, claimIntegrationPage, org).Scan(&candidate.ProjectID, &candidate.Generation, &candidate.SourceAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return candidate, nil
	}
	return candidate, pgcore.MapError(ctx, err)
}
