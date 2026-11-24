package hubmaintenance

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		if tenant.Organization == "org_00001" {
			panic("private fixture content")
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
		if tick == 0 && (!strings.Contains(err.Error(), "org_00001 feature board") || !strings.Contains(err.Error(), "maintenance step panicked at") || strings.Contains(err.Error(), "private fixture content")) {
			t.Fatalf("unexpected panic diagnostic: %v", err)
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

func TestDueLeadAndRotatingWorkShareTenantItems(t *testing.T) {
	var selected []string
	costs := make(map[string]int)
	var order []string
	var mu sync.Mutex
	step := func(name string) func(context.Context, Tenant, Budget) (Outcome, error) {
		return func(_ context.Context, t Tenant, b Budget) (Outcome, error) {
			mu.Lock()
			defer mu.Unlock()
			if name == "billing" {
				selected = append(selected, t.Organization)
			}
			costs[t.Organization] += b.Items
			if costs[t.Organization] > ItemLimit {
				return Outcome{}, fmt.Errorf("tenant %s used %d items", t.Organization, costs[t.Organization])
			}
			order = append(order, name)
			return Outcome{Items: b.Items, Progress: true}, nil
		}
	}
	features := []Feature{
		{Name: "billing", Lead: true, Bytes: 1, Step: step("billing"), Due: func(_ context.Context, limit int) ([]Tenant, error) {
			if limit != TenantLimit {
				return nil, fmt.Errorf("due page limit %d", limit)
			}
			return []Tenant{{Organization: "org_99999", Generation: 2}}, nil
		}},
		{Name: "board", Lead: true, Bytes: 1, Step: step("board")},
		{Name: "retention", Bytes: 1, Step: step("retention")},
	}
	scheduler, err := New(registry(10000), features)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		clear(costs)
		order = nil
		selected = nil
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(selected) != 1 || selected[0] != "org_99999" || order[0] != "billing" || order[len(order)-1] != "retention" {
			t.Fatalf("selected=%v order=%v", selected, order)
		}
	}
}

func TestLeadsAndFullReservationShareOneByteLimit(t *testing.T) {
	var consumed int64
	var lead, rotation int
	step := func(bytes int64, visits *int) func(context.Context, Tenant, Budget) (Outcome, error) {
		return func(ctx context.Context, t Tenant, b Budget) (Outcome, error) {
			if err := charge(ctx, t.Organization, bytes); err != nil {
				return Outcome{}, err
			}
			consumed += bytes
			*visits++
			return Outcome{Progress: true, Items: 1}, nil
		}
	}
	scheduler, err := NewShared(registry(1), []Feature{{Name: "lead", Bytes: ByteLimit / 4, Lead: true, Step: step(ByteLimit/4, &lead)}, {Name: "rotation", Bytes: ByteLimit, Step: step(ByteLimit, &rotation)}})
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		consumed = 0
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if consumed > ByteLimit || consumed == 0 {
			t.Fatalf("charged %d bytes in one tick, budget %d", consumed, ByteLimit)
		}
	}
	if lead != 3 || rotation != 3 {
		t.Fatalf("lead visits %d, full-reservation visits %d", lead, rotation)
	}
}
