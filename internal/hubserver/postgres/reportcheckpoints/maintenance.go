package reportcheckpoints

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Refresher interface {
	RefreshReportCheckpoints(context.Context, tracker.OrganizationID, Cursor) (Cursor, bool, error)
}

func MaintenanceFeature(owner any) hubmaintenance.Feature {
	var mu sync.Mutex
	cursors := map[string]Cursor{}
	return hubmaintenance.Feature{Name: "report-checkpoints", Bytes: 1 << 20, Step: func(ctx context.Context, selected hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		if err := hubmaintenance.RequireTenant(ctx, selected); err != nil {
			return hubmaintenance.Outcome{}, err
		}
		refresher, ok := owner.(Refresher)
		if !ok {
			return hubmaintenance.Outcome{}, nil
		}
		mu.Lock()
		defer mu.Unlock()
		cursor := cursors[selected.Organization]
		items := 0
		for items < min(budget.Items, hubmaintenance.ItemLimit) && ctx.Err() == nil {
			next, processed, err := refresher.RefreshReportCheckpoints(ctx, tracker.OrganizationID(selected.Organization), cursor)
			if err != nil {
				cursors[selected.Organization] = Cursor{}
				return hubmaintenance.Outcome{Progress: items > 0, Items: items}, err
			}
			cursor = next
			if !processed {
				break
			}
			items++
		}
		cursors[selected.Organization] = cursor
		return hubmaintenance.Outcome{Progress: items > 0, Items: items}, nil
	}}
}
