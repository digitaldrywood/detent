package hubserver

import (
	"context"
	"errors"
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
	if s.config.runtimeHost != nil {
		org := tracker.OrganizationID(tenant.Organization)
		cursor, err := s.database.postgres.MaintenanceCursor(ctx, org, tenant.Generation, "scheduled-sources")
		if err != nil {
			return 0, err
		}
		after := postgres.ScheduledSourceCursor{ProjectID: tracker.ProjectID(cursor.Project), RecordID: cursor.Key}
		used, next, err := owner.BackfillScheduledSourcesAfter(ctx, org, eventstore.NewContent(hubmaintenance.LimitDetails(s.runDetailStore())), min(items, corestore.MaxPageSize), after)
		if next != after {
			cursor.Project, cursor.Key = string(next.ProjectID), next.RecordID
			err = errors.Join(err, s.database.postgres.SetMaintenanceCursor(ctx, org, tenant.Generation, "scheduled-sources", cursor))
		}
		return used, err
	}
	value, _ := m.states.LoadOrStore(tenant.Organization, &scheduledSourceState{})
	current, ok := value.(*scheduledSourceState)
	if !ok {
		return 0, errors.New("scheduled source maintenance state has an unexpected type")
	}
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
