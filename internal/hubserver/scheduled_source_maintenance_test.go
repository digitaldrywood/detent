package hubserver

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledSourceMaintenanceRepository struct {
	tenantstore.Repository
	calls   map[tracker.OrganizationID][]postgres.ScheduledSourceCursor
	failure error
}

func (r *scheduledSourceMaintenanceRepository) BackfillScheduledSourcesAfter(_ context.Context, organization tracker.OrganizationID, _ *eventstore.Content, limit int, after postgres.ScheduledSourceCursor) (int, postgres.ScheduledSourceCursor, error) {
	if limit != hubmaintenance.ItemLimit {
		return 0, after, errors.New("unexpected source maintenance item budget")
	}
	r.calls[organization] = append(r.calls[organization], after)
	if len(r.calls[organization]) == 1 {
		return 0, postgres.ScheduledSourceCursor{ProjectID: "prj_example", RecordID: "poison"}, r.failure
	}
	return 1, postgres.ScheduledSourceCursor{}, nil
}

func TestScheduledSourceMaintenanceKeepsEachTenantOwnerAndPassesPoison(t *testing.T) {
	t.Parallel()
	failure := errors.New("unreadable source")
	repository := &scheduledSourceMaintenanceRepository{calls: map[tracker.OrganizationID][]postgres.ScheduledSourceCursor{}, failure: failure}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		service := &Service{localOrganization: string(organization), config: Config{TenantRepository: repository}}
		sources := service.newScheduledSourceMaintenance()
		feature := hubmaintenance.Feature{Name: "scheduled-sources", Bytes: 64 << 20, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) error {
			_, err := sources.step(ctx, tenant, budget.Items)
			return err
		}}
		tenant := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
		scheduler, err := hubmaintenance.New(hostedMaintenanceTenant{tenant}, []hubmaintenance.Feature{feature})
		if err != nil {
			t.Fatal(err)
		}
		if err := scheduler.Tick(t.Context()); !errors.Is(err, failure) {
			t.Fatal("source failure was not recorded by existing scheduler")
		}
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		calls := repository.calls[organization]
		if len(calls) != 2 || calls[0] != (postgres.ScheduledSourceCursor{}) || calls[1] != (postgres.ScheduledSourceCursor{ProjectID: "prj_example", RecordID: "poison"}) {
			t.Fatal("failed source reset cursor or prevented next owned source")
		}
		foreign := hubmaintenance.Tenant{Organization: "org_foreign", Generation: 1}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: 32 << 20}
		if err := feature.Step(budget.Context(t.Context(), foreign), foreign, budget); err == nil || len(repository.calls["org_foreign"]) != 0 {
			t.Fatal("private tenant maintenance crossed archive ownership")
		}
	}
	if len(repository.calls) != 2 {
		t.Fatal("one tenant owner replaced another tenant's maintenance")
	}
}
