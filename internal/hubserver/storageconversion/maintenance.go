package storageconversion

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ConvertTenant(ctx context.Context, tenant hubmaintenance.Tenant, limit int, openStorage func(tracker.OrganizationID) Storage) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, BackgroundBudget)
	defer cancel()
	storage := openStorage(tracker.OrganizationID(tenant.Organization))
	usage, err := ConvertBatch(ctx, storage, min(limit, MaxRows))
	return usage.Rows, err
}
