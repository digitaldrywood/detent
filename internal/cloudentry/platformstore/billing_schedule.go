package platformstore

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (r *Repository) ConfigureBillingSchedule(ctx context.Context, tenant hubmaintenance.Tenant, seconds int64, now time.Time) error {
	if tenant.Organization == "" || tenant.Generation < 1 || now.IsZero() || seconds != 0 && (seconds < 60 || seconds > 3600) {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Background, func(tx pgx.Tx) error {
		return pgquery.New(tx).ConfigureBillingSchedule(ctx, pgquery.ConfigureBillingScheduleParams{OrganizationID: tenant.Organization, Generation: tenant.Generation, NowAt: now, Seconds: int32(seconds)})
	})
}

func (r *Repository) DueBillingTenants(ctx context.Context, now time.Time, limit int) ([]hubmaintenance.Tenant, error) {
	if now.IsZero() || limit < 1 || limit > hubmaintenance.TenantLimit {
		return nil, ErrInvalid
	}
	var tenants []hubmaintenance.Tenant
	err := r.read(ctx, corestore.Background, func(tx pgquery.DBTX) error {
		rows, err := pgquery.New(tx).DueBillingTenants(ctx, pgquery.DueBillingTenantsParams{NowAt: now, PageLimit: int32(limit)})
		for _, row := range rows {
			tenants = append(tenants, hubmaintenance.Tenant{Organization: row.ID, Generation: row.Generation})
		}
		return err
	})
	return tenants, err
}

func (r *Repository) BeginBillingReconciliation(ctx context.Context, tenant hubmaintenance.Tenant, now time.Time) (bool, error) {
	if tenant.Organization == "" || tenant.Generation < 1 || now.IsZero() {
		return false, ErrInvalid
	}
	claimed := false
	err := r.transaction(ctx, corestore.Background, func(tx pgx.Tx) error {
		n, err := pgquery.New(tx).BeginBillingReconciliation(ctx, pgquery.BeginBillingReconciliationParams{OrganizationID: tenant.Organization, Generation: tenant.Generation, NowAt: now})
		claimed = n == 1
		return err
	})
	return claimed, err
}

type BillingScheduleDiscovery struct {
	after    string
	complete bool
}

func (d *BillingScheduleDiscovery) Page(ctx context.Context, r *Repository, now time.Time, limit int) ([]hubmaintenance.Tenant, error) {
	due, err := r.DueBillingTenants(ctx, now, limit)
	if err != nil || d.complete {
		return due, err
	}
	var candidates []hubmaintenance.Tenant
	for range 2 {
		var rows []pgquery.LegacyBillingSchedulePageRow
		err := r.read(ctx, corestore.Background, func(tx pgquery.DBTX) error {
			var err error
			rows, err = pgquery.New(tx).LegacyBillingSchedulePage(ctx, pgquery.LegacyBillingSchedulePageParams{AfterID: d.after, PageLimit: int32(limit)})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.NeedsDiscovery && (len(candidates) == 0 || candidates[len(candidates)-1].Organization != row.ID) {
				candidates = append(candidates, hubmaintenance.Tenant{Organization: row.ID, Generation: row.Generation})
			}
		}
		if len(candidates) > 0 {
			break
		}
		if len(rows) < limit {
			d.complete = true
			break
		}
		d.after = rows[len(rows)-1].ID
	}
	if len(candidates) == 0 {
		return due, nil
	}
	n := min(len(due), max(1, limit/2))
	tenants := make([]hubmaintenance.Tenant, 0, limit)
	seen := make(map[string]bool, limit)
	for _, group := range [][]hubmaintenance.Tenant{due[:n], candidates, due[n:]} {
		for _, tenant := range group {
			if !seen[tenant.Organization] && len(tenants) < limit {
				seen[tenant.Organization] = true
				tenants = append(tenants, tenant)
			}
		}
	}
	slices.SortFunc(tenants, func(a, b hubmaintenance.Tenant) int { return strings.Compare(a.Organization, b.Organization) })
	return tenants, nil
}
