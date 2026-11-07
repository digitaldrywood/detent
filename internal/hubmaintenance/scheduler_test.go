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
	scheduler, err := New(registry(10000), []Feature{{Name: "board", Bytes: 1, Step: func(_ context.Context, tenant Tenant, budget Budget) error {
		mu.Lock()
		defer mu.Unlock()
		visits[tenant.Organization]++
		if budget.Items != ItemLimit || tenant.Generation != 1 {
			t.Errorf("invalid visit: %+v %+v", tenant, budget)
		}
		if tenant.Organization == "org_00000" {
			return failure
		}
		return nil
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
		features = append(features, Feature{Name: name, Bytes: ByteLimit / 2, Step: func(_ context.Context, tenant Tenant, budget Budget) error {
			mu.Lock()
			defer mu.Unlock()
			if visits[tenant.Organization] == nil {
				visits[tenant.Organization] = make(map[string]int)
			}
			visits[tenant.Organization][name]++
			if budget.Bytes != ByteLimit/2 {
				t.Errorf("wrong byte allowance: %d", budget.Bytes)
			}
			return nil
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
			scheduler, err := New(registryFunc(func(context.Context, string, int) ([]Tenant, error) { return page, nil }), []Feature{{Name: "metrics", Bytes: 1, Step: func(context.Context, Tenant, Budget) error {
				t.Error("dispatched malformed authority")
				return nil
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
