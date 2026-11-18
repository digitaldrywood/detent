package hubmaintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type registryFunc func(context.Context, string, int) ([]Tenant, error)

func (f registryFunc) Page(ctx context.Context, after string, limit int) ([]Tenant, error) {
	return f(ctx, after, limit)
}

func registry(size int) Registry {
	return registryFunc(func(_ context.Context, after string, limit int) ([]Tenant, error) {
		var page []Tenant
		for i := range size {
			id := fmt.Sprintf("org_%05d", i)
			if id > after && len(page) < limit {
				page = append(page, Tenant{Organization: id, Generation: 1})
			}
		}
		return page, nil
	})
}

func TestBusyAndFailingTenantsCannotStarveCircularTraversal(t *testing.T) {
	var mu sync.Mutex
	visits := make(map[string]int)
	failure := errors.New("candidate unavailable")
	scheduler, err := New(registry(10000), []Feature{{Name: "board", Bytes: 1, Step: func(_ context.Context, tenant Tenant, budget Budget) (Outcome, error) {
		mu.Lock()
		defer mu.Unlock()
		visits[tenant.Organization]++
		if budget.Items != ItemLimit || tenant.Generation != 1 {
			t.Errorf("invalid visit: %+v %+v", tenant, budget)
		}
		if tenant.Organization == "org_00000" {
			return Outcome{}, failure
		}
		return Outcome{Progress: true, Items: 1}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for tick := range 313 {
		before := len(visits)
		err := scheduler.Tick(t.Context())
		if tick == 0 && !errors.Is(err, failure) || tick > 0 && err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if len(visits)-before > TenantLimit {
			t.Fatalf("tick exceeded tenant budget: %d", len(visits)-before)
		}
	}
	if len(visits) != 10000 || visits["org_00000"] != 1 {
		t.Fatalf("unfair traversal: tenants=%d first=%d", len(visits), visits["org_00000"])
	}
}

func TestByteBudgetRetainsUndispatchedTenantsAndRotatesFeatures(t *testing.T) {
	var mu sync.Mutex
	visits := make(map[string]map[string]int)
	var features []Feature
	for _, name := range []string{"board", "billing", "metrics"} {
		features = append(features, Feature{Name: name, Bytes: ByteLimit / 2, Step: func(_ context.Context, tenant Tenant, budget Budget) (Outcome, error) {
			mu.Lock()
			defer mu.Unlock()
			if visits[tenant.Organization] == nil {
				visits[tenant.Organization] = make(map[string]int)
			}
			visits[tenant.Organization][name]++
			if budget.Bytes != ByteLimit/2 {
				t.Errorf("wrong byte allowance: %d", budget.Bytes)
			}
			return Outcome{Progress: true, Items: 1}, nil
		}})
	}
	scheduler, err := New(registry(5), features)
	if err != nil {
		t.Fatal(err)
	}
	for range 9 {
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		for _, feature := range features {
			if got := visits[fmt.Sprintf("org_%05d", i)][feature.Name]; got != 1 {
				t.Fatalf("tenant %d feature %s visits=%d", i, feature.Name, got)
			}
		}
	}
}

func TestMalformedRegistryPageFailsBeforeDispatch(t *testing.T) {
	for _, page := range [][]Tenant{{{Organization: "org_a"}}, {{Organization: "org_b", Generation: 1}, {Organization: "org_a", Generation: 1}}} {
		t.Run(fmt.Sprint(page), func(t *testing.T) {
			scheduler, err := New(registryFunc(func(context.Context, string, int) ([]Tenant, error) { return page, nil }), []Feature{{Name: "metrics", Bytes: 1, Step: func(context.Context, Tenant, Budget) (Outcome, error) {
				t.Error("dispatched malformed authority")
				return Outcome{}, nil
			}}})
			if err != nil {
				t.Fatal(err)
			}
			if scheduler.Tick(t.Context()) == nil {
				t.Fatal("accepted invalid registry page")
			}
		})
	}
}

func TestLeadFeatureServesEveryTenantEveryTickWhileRotationAdvances(t *testing.T) {
	for _, tenants := range []int{2, TenantLimit} {
		t.Run(fmt.Sprint(tenants), func(t *testing.T) {
			type tickKey struct{}
			var scopeCount, tick int
			var mu sync.Mutex
			visits := make(map[string]map[string]int)
			step := func(name string, lead bool) Feature {
				bytes := int64(ByteLimit / (2 * TenantLimit))
				if lead {
					bytes = ByteLimit / TenantLimit
				}
				return Feature{Name: name, Bytes: bytes, Lead: lead, Step: func(ctx context.Context, tenant Tenant, budget Budget) (Outcome, error) {
					if ctx.Value(tickKey{}) != tick {
						return Outcome{}, fmt.Errorf("%s received scope %v for tick %d", name, ctx.Value(tickKey{}), tick)
					}
					if err := RequireTenant(ctx, tenant); err != nil {
						return Outcome{}, err
					}
					mu.Lock()
					if visits[name] == nil {
						visits[name] = make(map[string]int)
					}
					visits[name][tenant.Organization]++
					mu.Unlock()
					if lead && budget.Items != ItemLimit/2 || !lead && budget.Items < ItemLimit/2 {
						t.Errorf("%s budget for %s: %d items", name, tenant.Organization, budget.Items)
					}
					if err := charge(ctx, tenant.Organization, bytes/2); err != nil {
						return Outcome{}, err
					}
					return Outcome{Progress: true, Items: budget.Items}, nil
				}}
			}
			scheduler, err := NewScoped(registry(tenants), []Feature{step("billing", false), step("board", true), step("metrics", false)}, func(ctx context.Context) context.Context {
				scopeCount++
				return context.WithValue(ctx, tickKey{}, scopeCount)
			})
			if err != nil {
				t.Fatal(err)
			}
			const ticks = 6
			for tick = 1; tick <= ticks; tick++ {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			for i := range tenants {
				tenant := fmt.Sprintf("org_%05d", i)
				if got := visits["board"][tenant]; got != ticks {
					t.Fatalf("lead served %s %d times in %d ticks", tenant, got, ticks)
				}
				for _, family := range []string{"billing", "metrics"} {
					if got := visits[family][tenant]; got < 2 {
						t.Fatalf("rotation family %s served %s %d times in %d ticks", family, tenant, got, ticks)
					}
				}
			}
		})
	}
}
