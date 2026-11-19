package operatorcommands

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledSourceMaintenanceRepository struct {
	calls   map[tracker.OrganizationID][]postgres.ScheduledSourceCursor
	failure error
}

func (r *scheduledSourceMaintenanceRepository) BackfillScheduledSourcesAfter(_ context.Context, organization tracker.OrganizationID, _ *eventstore.Content, _ int, after postgres.ScheduledSourceCursor) (int, postgres.ScheduledSourceCursor, error) {
	r.calls[organization] = append(r.calls[organization], after)
	if len(r.calls[organization]) == 1 {
		return 0, postgres.ScheduledSourceCursor{ProjectID: "prj_example", RecordID: "poison"}, r.failure
	}
	return 1, postgres.ScheduledSourceCursor{}, nil
}

type sourceTenantPage struct{ tenant hubmaintenance.Tenant }

func (p sourceTenantPage) Page(_ context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	if after >= p.tenant.Organization || limit < 1 {
		return nil, nil
	}
	return []hubmaintenance.Tenant{p.tenant}, nil
}

func TestScheduledSourceMaintenanceKeepsEachTenantOwnerAndPassesPoison(t *testing.T) {
	t.Parallel()
	failure := errors.New("unreadable source")
	repository := &scheduledSourceMaintenanceRepository{calls: map[tracker.OrganizationID][]postgres.ScheduledSourceCursor{}, failure: failure}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		sources := NewScheduledSources(ScheduledSourceDeps{
			Owns:    func(tenant hubmaintenance.Tenant) bool { return tenant.Organization == string(organization) },
			Store:   func() ScheduledSourceBackfiller { return repository },
			Details: func() attachment.DetailStore { return nil },
		})
		feature := hubmaintenance.Feature{Name: "scheduled-sources", Bytes: 64 << 20, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			used, err := sources.Step(ctx, tenant, budget.Items)
			return hubmaintenance.Outcome{Progress: used > 0, Items: max(used, 1)}, err
		}}
		tenant := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
		scheduler, err := hubmaintenance.New(sourceTenantPage{tenant: tenant}, []hubmaintenance.Feature{feature})
		if err != nil {
			t.Fatal(err)
		}
		if err := scheduler.Tick(t.Context()); !errors.Is(err, failure) {
			t.Fatal("source failure was not recorded by existing scheduler")
		}
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(repository.calls[organization], []postgres.ScheduledSourceCursor{{}, {ProjectID: "prj_example", RecordID: "poison"}}) {
			t.Fatal("failed source reset cursor or prevented next owned source")
		}
		foreign := hubmaintenance.Tenant{Organization: "org_foreign", Generation: 1}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: 32 << 20}
		if _, err := feature.Step(budget.Context(t.Context(), foreign), foreign, budget); !errors.Is(err, corestore.ErrTenantMismatch) || repository.calls["org_foreign"] != nil {
			t.Fatal("private tenant maintenance crossed archive ownership")
		}
	}
}
