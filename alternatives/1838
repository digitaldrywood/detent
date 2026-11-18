package credits

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestIncludedAICreditAllowance(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		ref    billingstore.PlanReference
		paid   bool
		want   int64
		custom bool
	}{
		{"Starter", billingstore.PlanReference{ID: "starter", Version: 1}, true, 4900000, false},
		{"Team", billingstore.PlanReference{ID: "team", Version: 2}, true, 9900000, false},
		{"Growth", billingstore.PlanReference{ID: "growth", Version: 2}, true, 24900000, false},
		{"Scale", billingstore.PlanReference{ID: "scale", Version: 2}, true, 0, true},
		{"Free", billingstore.PlanReference{ID: "free", Version: 1}, true, 0, false},
		{"pilot", billingstore.PlanReference{ID: "pilot_free", Version: 1}, true, 0, false},
		{"complimentary team", billingstore.PlanReference{ID: "comp_team", Version: 1}, true, 0, false},
		{"complimentary grant", billingstore.PlanReference{ID: "team", Version: 2}, false, 0, false},
		{"legacy growth", billingstore.PlanReference{ID: "growth", Version: 1}, true, 0, false},
	} {

		t.Run(test.name, func(t *testing.T) {
			for range 2 {
				d := creditFixture(t, repository, 0)
				access := now.AddDate(0, 1, 0)
				if !test.paid {
					access = now
				}
				cents := map[string]int64{"starter": 4900, "team": 9900, "growth": 24900, "free": 0, "pilot_free": 0, "comp_team": 0, "scale": 0}[test.ref.ID]
				if test.ref.ID == "growth" && test.ref.Version == 1 {
					cents = 0
				}
				plans := []billingstore.Plan{{PlanReference: billingstore.PlanReference{ID: "base", Version: 1}}, {PlanReference: test.ref, Name: test.name, MonthlyUSDCents: new(cents)}}
				hubtest.PlanCatalog(t, repository, d.deps.Organization, plans)
				hubtest.PlanAssignment(t, repository, d.deps.Organization, hubtest.PlanAssignmentOptions{Base: plans[0].PlanReference, Subscription: &test.ref, StartsAt: now, EndsAt: now.AddDate(0, 1, 0), AccessUntil: access})
				d.deps.Plan = func(ctx context.Context, r billingstore.Reader, ref billingstore.PlanReference) (Plan, error) {
					plan, err := r.BillingPlan(ctx, ref)
					amount := *plan.MonthlyUSDCents * 1000
					if test.custom {
						return Plan{Name: plan.Name}, err
					}
					return Plan{Name: plan.Name, AllowanceMicros: &amount}, err
				}
				included, err := d.Included(t.Context(), d.deps.Organization, now)
				if err != nil || included.AllowanceMicros != test.want {
					t.Fatalf("included=%+v err=%v", included, err)
				}
			}
		})
	}
}
