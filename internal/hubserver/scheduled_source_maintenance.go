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

type scheduledSourceMaintenance struct {
	service *Service
	states  sync.Map
}

type scheduledSourceState struct {
	mu         sync.Mutex
	generation int64
	after      postgres.ScheduledSourceCursor
}

func (s *Service) newScheduledSourceMaintenance() *scheduledSourceMaintenance {
	return &scheduledSourceMaintenance{service: s}
}

func (m *scheduledSourceMaintenance) owns(tenant hubmaintenance.Tenant) bool {
	s := m.service
	return tenant.Organization == s.appOrganizationID() && (!s.hostedShared() || tenant.Generation == s.config.Hosted.SharedEntry.Generation)
}

func (m *scheduledSourceMaintenance) step(ctx context.Context, tenant hubmaintenance.Tenant, items int) (int, error) {
	if items < 1 {
		return 0, nil
	}
	s := m.service
	if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
		return 0, err
	}
	if !m.owns(tenant) {
		return 0, corestore.ErrTenantMismatch
	}
	owner, ok := s.config.TenantRepository.(interface {
		BackfillScheduledSourcesAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, postgres.ScheduledSourceCursor) (int, postgres.ScheduledSourceCursor, error)
	})
	if !ok {
		return 0, hubapi.NativeWorkItemStorageUnavailable()
	}
	value, _ := m.states.LoadOrStore(tenant.Organization, &scheduledSourceState{})
	current := value.(*scheduledSourceState)
	current.mu.Lock()
	if current.generation != tenant.Generation {
		current.generation = tenant.Generation
		current.after = postgres.ScheduledSourceCursor{}
	}
	after := current.after
	current.mu.Unlock()
	used, next, err := owner.BackfillScheduledSourcesAfter(ctx, tracker.OrganizationID(tenant.Organization), eventstore.NewContent(hubmaintenance.LimitDetails(s.runDetailStore())), min(items, corestore.MaxPageSize), after)
	current.mu.Lock()
	if current.generation == tenant.Generation && current.after == after {
		current.after = next
	}
	current.mu.Unlock()
	return used, err
}
