package runnerupdate

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) ExpireStarts(ctx context.Context, organization tracker.OrganizationID, now time.Time, budget int) (int, error) {
	if budget < 1 {
		return 0, nil
	}
	var ids []string
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(runnerstore.LifecycleMaintainer)
		if !ok {
			return hubapi.RunEventStorageUnavailable()
		}
		var err error
		ids, err = owner.RunnerStartDeadlines(ctx, now, min(budget, 100))
		return err
	})
	if err != nil {
		return 0, err
	}
	for index, id := range ids {
		if err := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			owner, ok := w.(runnerstore.LifecycleMaintainer)
			if !ok {
				return hubapi.RunEventStorageUnavailable()
			}
			return owner.ExpireRunnerStart(ctx, id, now)
		}); err != nil {
			return index, err
		}
	}
	return len(ids), nil
}

func (s *Service) MaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "runner-lifecycle", Bytes: 1 << 20, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return hubmaintenance.Outcome{}, err
		}
		used, err := s.ExpireStarts(ctx, tracker.OrganizationID(tenant.Organization), s.deps.Now().UTC(), min(budget.Items, 100))
		return hubmaintenance.Outcome{Progress: used > 0, Items: used}, err
	}}
}
