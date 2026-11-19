package operatorcommands

import (
	"context"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ScheduledSourceBackfiller interface {
	BackfillScheduledSourcesAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, postgres.ScheduledSourceCursor) (int, postgres.ScheduledSourceCursor, error)
}

type ScheduledSourceDeps struct {
	Owns    func(hubmaintenance.Tenant) bool
	Store   func() ScheduledSourceBackfiller
	Details func() attachment.DetailStore
}

type ScheduledSources struct {
	deps   ScheduledSourceDeps
	states sync.Map
}

type scheduledSourceState struct {
	mu         sync.Mutex
	generation int64
	after      postgres.ScheduledSourceCursor
}

func NewScheduledSources(deps ScheduledSourceDeps) *ScheduledSources {
	return &ScheduledSources{deps: deps}
}

func (m *ScheduledSources) Step(ctx context.Context, tenant hubmaintenance.Tenant, items int) (int, error) {
	if items < 1 {
		return 0, nil
	}
	if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
		return 0, err
	}
	if !m.deps.Owns(tenant) {
		return 0, corestore.ErrTenantMismatch
	}
	owner := m.deps.Store()
	if owner == nil {
		return 0, hubapi.NativeWorkItemStorageUnavailable()
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
	used, next, err := owner.BackfillScheduledSourcesAfter(ctx, tracker.OrganizationID(tenant.Organization), eventstore.NewContent(hubmaintenance.LimitDetails(m.deps.Details())), min(items, corestore.MaxPageSize), after)
	current.mu.Lock()
	if current.generation == tenant.Generation && current.after == after {
		current.after = next
	}
	current.mu.Unlock()
	return used, err
}
