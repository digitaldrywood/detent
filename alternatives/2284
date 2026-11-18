package maintenance_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type pagedRegistry int

func (n pagedRegistry) Page(_ context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	start := 0
	if after != "" {
		if _, err := fmt.Sscanf(after, "org_%05d", &start); err != nil {
			return nil, err
		}
		start++
	}
	var page []hubmaintenance.Tenant
	for i := start; i < int(n) && len(page) < limit; i++ {
		page = append(page, hubmaintenance.Tenant{Organization: fmt.Sprintf("org_%05d", i), Generation: 1})
	}
	return page, nil
}

type singleTenant struct{ hubmaintenance.Tenant }

func (s singleTenant) Page(_ context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	if limit <= 0 || s.Organization <= after {
		return nil, nil
	}
	return []hubmaintenance.Tenant{s.Tenant}, nil
}

func quietHub(t *testing.T) *hubfixture.Hub {
	t.Helper()
	return hubfixture.OpenDeps(t, hubfixture.Config{}, hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}})
}

type serviceLog struct {
	mu     sync.Mutex
	tick   int
	served map[string]map[string][]int
}

func (l *serviceLog) busy(features []hubmaintenance.Feature) []hubmaintenance.Feature {
	out := make([]hubmaintenance.Feature, len(features))
	for i, feature := range features {
		name := feature.Name
		out[i] = hubmaintenance.Feature{Name: name, Bytes: feature.Bytes, Lead: feature.Lead, Step: func(_ context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.served[name] == nil {
				l.served[name] = map[string][]int{}
			}
			l.served[name][tenant.Organization] = append(l.served[name][tenant.Organization], l.tick)
			return hubmaintenance.Outcome{Progress: true, Items: budget.Items}, nil
		}}
	}
	return out
}

func leads(features []hubmaintenance.Feature) int {
	count := 0
	for _, feature := range features {
		if feature.Lead {
			count++
		}
	}
	return count
}

func traversalTicks(bytes int64, tenants int) int {
	batch := int(min(int64(hubmaintenance.TenantLimit), hubmaintenance.ByteLimit/bytes))
	return (tenants + batch - 1) / batch
}

func TestRegisteredFamiliesAllBusyServiceGap(t *testing.T) {
	h := quietHub(t)
	features := h.Service.ReadModelMaintenanceFeatures()
	run := func(t *testing.T, registry hubmaintenance.Registry, tenants int) (*serviceLog, int) {
		t.Helper()
		bound := 0
		for _, feature := range features {
			if !feature.Lead {
				bound += traversalTicks(feature.Bytes, tenants)
			}
		}
		log := &serviceLog{served: map[string]map[string][]int{}}
		scheduler, err := hubmaintenance.New(registry, log.busy(features))
		if err != nil {
			t.Fatal(err)
		}
		for tick := range 2 * bound {
			log.mu.Lock()
			log.tick = tick
			log.mu.Unlock()
			if err := scheduler.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		for _, feature := range features {
			if len(log.served[feature.Name]) != tenants {
				t.Fatalf("family %s reached %d of %d tenants in two derived rotations", feature.Name, len(log.served[feature.Name]), tenants)
			}
			for tenant, ticks := range log.served[feature.Name] {
				if feature.Lead {
					gap := traversalTicks(feature.Bytes, tenants)
					if ticks[0] >= gap || len(ticks) != (2*bound-ticks[0]+gap-1)/gap {
						t.Fatalf("lead %s tenant %s served at %d ticks from %d, want every %d", feature.Name, tenant, len(ticks), ticks[0], gap)
					}
					continue
				}
				if len(ticks) != 2 || ticks[0] >= bound || ticks[1]-ticks[0] != bound {
					t.Fatalf("family %s tenant %s served at ticks %v, derived rotation %d", feature.Name, tenant, ticks, bound)
				}
			}
		}
		return log, bound
	}
	t.Run("registered single-tenant registry", func(t *testing.T) {
		_, bound := run(t, singleTenant{h.Service.MaintenanceTenant()}, 1)
		if bound != len(features)-leads(features) {
			t.Fatalf("single-tenant all-busy rotation=%d ticks want one per rotating family (%d)", bound, len(features)-leads(features))
		}
		t.Logf("production registry: one tenant, %d families, all-busy service gap %d ticks", len(features), bound)
	})
	t.Run("hypothetical 10000-tenant registry with real reservations", func(t *testing.T) {
		const tenants = 10000
		_, bound := run(t, pagedRegistry(tenants), tenants)
		for _, feature := range features {
			t.Logf("family %s reservation=%d bytes traversal=%d ticks", feature.Name, feature.Bytes, traversalTicks(feature.Bytes, tenants))
		}
		t.Logf("10000 tenants: all-busy service gap %d ticks (about %s at one tick per second)", bound, time.Duration(bound)*time.Second)
	})
}

func TestIdleTicksStayInsideResourceCeilings(t *testing.T) {
	h := quietHub(t)
	features := h.Service.ReadModelMaintenanceFeatures()
	var mu sync.Mutex
	stepped := 0
	for i := range features {
		step := features[i].Step
		features[i].Step = func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			mu.Lock()
			stepped++
			mu.Unlock()
			return step(ctx, tenant, budget)
		}
	}
	scheduler, err := hubmaintenance.New(singleTenant{h.Service.MaintenanceTenant()}, features)
	if err != nil {
		t.Fatal(err)
	}
	maxTransactions, maxStatements, maxSteps := 0, 0, 0
	for range 3 * len(features) {
		transactions, statements := 0, 0
		ctx := postgres.WithTransactionTiming(t.Context(), func(name string, _ time.Time) {
			if name == "pg_commit" {
				transactions++
			}
		}, func(time.Time, string) { statements++ })
		mu.Lock()
		stepped = 0
		mu.Unlock()
		if err := scheduler.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		steps := stepped
		mu.Unlock()
		if steps > hubmaintenance.ProbeLimit+leads(features) || transactions > 256 || statements > 4096 {
			t.Fatalf("idle tick exceeded fixed work: steps=%d transactions=%d statements=%d", steps, transactions, statements)
		}
		maxTransactions, maxStatements, maxSteps = max(maxTransactions, transactions), max(maxStatements, statements), max(maxSteps, steps)
	}
	if maxSteps != hubmaintenance.ProbeLimit+leads(features) {
		t.Fatalf("idle ticks never reached the four-probe maximum plus leads (max steps %d)", maxSteps)
	}
	t.Logf("idle real feature mix: max steps=%d transactions=%d statements=%d per tick", maxSteps, maxTransactions, maxStatements)
}
