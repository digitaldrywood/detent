package maintenance_test

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthDiagnosticsReadsPerInterval(t *testing.T) {
	clock := hubtest.NewClock(time.Now())
	h := hubfixture.Open(t, hubfixture.Config{Clock: clock.Now})
	organizations := []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary}
	for _, org := range organizations {
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
		feature := h.Service.HealthPageMaintenanceFeature()
		for _, want := range []bool{true, false} {
			out, err := feature.Step(budget.Context(t.Context(), tenant), tenant, budget)
			if err != nil || out.Progress != want {
				t.Fatalf("%s empty detector cycle progress=%t want=%t err=%v", org, out.Progress, want, err)
			}
		}
		hubfixture.NewNative(t, h, org, "Example project")
	}
	feature := h.Service.HealthPageMaintenanceFeature()
	for range 2 {
		for _, org := range organizations {
			reads := 0
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
				if strings.Contains(query, "WITH RECURSIVE lease_machines") {
					reads++
				}
			})
			tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
			budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
			for range 12 {
				if _, err := feature.Step(budget.Context(ctx, tenant), tenant, budget); err != nil {
					t.Fatal(err)
				}
			}
			if reads != 1 {
				t.Errorf("%s health interval read diagnostics history %d times, want 1", org, reads)
			}
		}
		clock.Advance(healthsignals.Interval)
	}
}
