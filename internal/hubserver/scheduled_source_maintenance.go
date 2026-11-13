package hubserver

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) ScheduledSourceMaintenanceFeature() hubmaintenance.Feature {
	type state struct {
		mu         sync.Mutex
		generation int64
		after      postgres.ScheduledSourceCursor
	}
	var states sync.Map
	return hubmaintenance.Feature{Name: "scheduled-sources", Bytes: 64 << 20, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		if tenant.Organization != s.appOrganizationID() {
			return corestore.ErrTenantMismatch
		}
		if s.hostedShared() && tenant.Generation != s.config.Hosted.SharedEntry.Generation {
			return corestore.ErrTenantMismatch
		}
		owner, ok := s.config.TenantRepository.(interface {
			BackfillScheduledSourcesAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, postgres.ScheduledSourceCursor) (int, postgres.ScheduledSourceCursor, error)
		})
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		value, _ := states.LoadOrStore(tenant.Organization, &state{})
		current := value.(*state)
		current.mu.Lock()
		if current.generation != tenant.Generation {
			current.generation = tenant.Generation
			current.after = postgres.ScheduledSourceCursor{}
		}
		after := current.after
		current.mu.Unlock()
		_, next, err := owner.BackfillScheduledSourcesAfter(ctx, tracker.OrganizationID(tenant.Organization), eventstore.NewContent(hubmaintenance.LimitDetails(s.runDetailStore())), min(budget.Items, corestore.MaxPageSize), after)
		current.mu.Lock()
		if current.generation == tenant.Generation && current.after == after {
			current.after = next
		}
		current.mu.Unlock()
		return err
	}}
}
