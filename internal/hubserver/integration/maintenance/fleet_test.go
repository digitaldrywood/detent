package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
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

func TestIdleTicksReachEveryFeature(t *testing.T) {
	h := quietHub(t)
	features := h.Service.ReadModelMaintenanceFeatures()
	var mu sync.Mutex
	visited := make(map[string]bool, len(features))
	for i, feature := range features {
		features[i].Step = func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			mu.Lock()
			visited[feature.Name] = true
			mu.Unlock()
			return feature.Step(ctx, tenant, budget)
		}
	}
	scheduler, err := hubmaintenance.New(singleTenant{h.Service.MaintenanceTenant()}, features)
	if err != nil {
		t.Fatal(err)
	}
	for range len(features) {
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, feature := range features {
		if !visited[feature.Name] {
			t.Errorf("idle maintenance skipped %s", feature.Name)
		}
	}
}

func TestSharedTenThousandTenantFamiliesInterleavePages(t *testing.T) {
	var mu sync.Mutex
	firstVisits := map[string]int{}
	secondPages := map[string]bool{}
	pageSize := map[string]int{}
	failure := errors.New("unavailable work item")
	features := hubserver.HostedMaintenanceSchedule(func(_ context.Context, t hubmaintenance.Tenant, b hubmaintenance.Budget, name string) (hubmaintenance.Outcome, error) {
		mu.Lock()
		defer mu.Unlock()
		if t.Organization == "org_00000" {
			firstVisits[name]++
			if name == "work-item-pages" {
				return hubmaintenance.Outcome{Items: 1}, failure
			}
		}
		if t.Organization == fmt.Sprintf("org_%05d", pageSize[name]) {
			secondPages[name] = true
		}
		return hubmaintenance.Outcome{Progress: true, Items: b.Items}, nil
	})
	rotating := 0
	for _, f := range features {
		if f.Lead {
			continue
		}
		rotating++
		pageSize[f.Name] = min(hubmaintenance.TenantLimit, int(hubmaintenance.ByteLimit/f.Bytes))
	}
	scheduler, err := hubmaintenance.NewShared(pagedRegistry(10000), features)
	if err != nil {
		t.Fatal(err)
	}
	failedTicks := 0
	for range 2 * rotating {
		err := scheduler.Tick(t.Context())
		if errors.Is(err, failure) {
			failedTicks++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if failedTicks != 1 {
		t.Fatalf("failing first row ran %d times", failedTicks)
	}
	for name := range pageSize {
		if firstVisits[name] != 1 || !secondPages[name] {
			t.Fatalf("family %s first tenant visits=%d resumed next page=%v", name, firstVisits[name], secondPages[name])
		}
	}
}
