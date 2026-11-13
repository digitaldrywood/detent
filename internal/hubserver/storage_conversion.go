package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const backgroundStorageBudget = storageconversion.BackgroundBudget
const storageConversionRows = storageconversion.MaxRows

var errStorageBatchFull = storageconversion.ErrBatchFull

func (s *Service) convertPostgresStorage(ctx context.Context, tenant hubmaintenance.Tenant, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, backgroundStorageBudget)
	defer cancel()
	storage := postgres.NewConversionStorage(s.database.postgres, tracker.OrganizationID(tenant.Organization), s.runDetailStore(), func(ctx context.Context, _ storageconversion.Candidate, raw []byte) ([]byte, error) {
		return resolveStorageJSON(s.freshRunDetailContext(ctx), raw)
	})
	usage, err := storageconversion.ConvertBatch(ctx, storage, min(limit, storageconversion.MaxRows))
	return usage.Rows, err
}

func expandStorageConversion(ctx context.Context, size int) error {
	return storageconversion.Expand(ctx, size)
}
