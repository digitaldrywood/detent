package hubmaintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

type selectionCounters struct {
	mu           sync.Mutex
	pages        int
	maxTickSteps int
	steps        map[string]int
	served       map[string]map[string]int
	tick         map[string]int
}

func (c *selectionCounters) registry(size int, generation func(string) int64) Registry {
	return registryFunc(func(_ context.Context, after string, limit int) ([]Tenant, error) {
		c.mu.Lock()
		c.pages++
		c.mu.Unlock()
		start := 0
		if after != "" {
			if _, err := fmt.Sscanf(after, "org_%05d", &start); err != nil {
				return nil, err
			}
			start++
		}
		var page []Tenant
		for i := start; i < size && len(page) < limit; i++ {
			id := fmt.Sprintf("org_%05d", i)
			page = append(page, Tenant{Organization: id, Generation: generation(id)})
		}
		return page, nil
	})
}

func (c *selectionCounters) feature(name string, bytes int64, step func(Tenant, Budget) (Outcome, error)) Feature {
	return Feature{Name: name, Bytes: bytes, Step: func(ctx context.Context, tenant Tenant, budget Budget) (Outcome, error) {
		if err := RequireTenant(ctx, tenant); err != nil {
			return Outcome{}, err
		}
		c.mu.Lock()
		key := name + "/" + tenant.Organization
		c.tick[key]++
		duplicate := c.tick[key] > 1
		c.steps[name]++
		if c.served[name] == nil {
			c.served[name] = map[string]int{}
		}
		c.served[name][tenant.Organization]++
		c.mu.Unlock()
		if duplicate {
			return Outcome{}, errors.New("family visited a tenant twice in one tick")
		}
		return step(tenant, budget)
	}}
}

func (c *selectionCounters) startTick() {
	c.mu.Lock()
	c.pages = 0
	c.tick = map[string]int{}
	c.mu.Unlock()
}

func newSelectionCounters() *selectionCounters {
	return &selectionCounters{steps: map[string]int{}, served: map[string]map[string]int{}, tick: map[string]int{}}
}

func idle(Tenant, Budget) (Outcome, error) { return Outcome{Items: 1}, nil }

func busy(_ Tenant, budget Budget) (Outcome, error) {
	return Outcome{Progress: true, Items: budget.Items}, nil
}

func one(int64) func(string) int64 { return func(string) int64 { return 1 } }

func TestWorkConservingSelectionTables(t *testing.T) {
	failure := errors.New("family unavailable")
	for _, test := range []struct {
		name     string
		tenants  int
		ticks    int
		families func(*selectionCounters) []Feature
		check    func(*testing.T, *selectionCounters, []error)
	}{
		{"one busy family among idle families", 1, 360, func(c *selectionCounters) []Feature {
			features := []Feature{c.feature("pages", ByteLimit, busy)}
			for i := range 9 {
				features = append(features, c.feature(fmt.Sprintf("idle-%d", i), ByteLimit, idle))
			}
			return features
		}, func(t *testing.T, c *selectionCounters, errs []error) {
			if served := c.steps["pages"]; served < 120 {
				t.Fatalf("busy family served %d of 360 ticks; a plain rotation of 10 gives 36, conservation must give at least 120", served)
			}
			for i := range 9 {
				if c.steps[fmt.Sprintf("idle-%d", i)] < 36 {
					t.Fatalf("idle family %d starved: %d probes", i, c.steps[fmt.Sprintf("idle-%d", i)])
				}
			}
		}},
		{"all idle", 1, 30, func(c *selectionCounters) []Feature {
			var features []Feature
			for i := range 10 {
				features = append(features, c.feature(fmt.Sprintf("idle-%d", i), ByteLimit/4, idle))
			}
			return features
		}, func(t *testing.T, c *selectionCounters, errs []error) {
			for i := range 10 {
				if got := c.steps[fmt.Sprintf("idle-%d", i)]; got != 12 {
					t.Fatalf("idle family %d probes=%d want 12 (4 probes per tick over 30 ticks, 10 families)", i, got)
				}
			}
		}},
		{"all busy", 1, 30, func(c *selectionCounters) []Feature {
			var features []Feature
			for i := range 10 {
				features = append(features, c.feature(fmt.Sprintf("busy-%d", i), ByteLimit, busy))
			}
			return features
		}, func(t *testing.T, c *selectionCounters, errs []error) {
			for i := range 10 {
				if got := c.steps[fmt.Sprintf("busy-%d", i)]; got != 3 {
					t.Fatalf("busy family %d served=%d want 3 (one family per tick)", i, got)
				}
			}
		}},
		{"failing family stops the tick and stays eligible", 1, 20, func(c *selectionCounters) []Feature {
			return []Feature{c.feature("idle-a", ByteLimit, idle), c.feature("failing", ByteLimit, func(Tenant, Budget) (Outcome, error) { return Outcome{Items: 1}, failure }), c.feature("idle-b", ByteLimit, idle), c.feature("pages", ByteLimit, busy)}
		}, func(t *testing.T, c *selectionCounters, errs []error) {
			failed := 0
			for _, err := range errs {
				if errors.Is(err, failure) {
					failed++
				}
			}
			if failed != c.steps["failing"] || failed < 5 || c.steps["pages"] < 5 {
				t.Fatalf("failing=%d reported=%d pages=%d", c.steps["failing"], failed, c.steps["pages"])
			}
		}},
		{"byte-limited batches stop probing and keep the traversal", 5, 9, func(c *selectionCounters) []Feature {
			return []Feature{c.feature("half-a", ByteLimit/2, idle), c.feature("half-b", ByteLimit/2, idle), c.feature("half-c", ByteLimit/2, idle)}
		}, func(t *testing.T, c *selectionCounters, errs []error) {
			for _, name := range []string{"half-a", "half-b", "half-c"} {
				for i := range 5 {
					if got := c.served[name][fmt.Sprintf("org_%05d", i)]; got < 1 {
						t.Fatalf("%s tenant %d never visited", name, i)
					}
				}
			}
			if c.maxTickSteps > 4 {
				t.Fatalf("byte-limited tick ran %d steps; two half-allowance tenants per batch, at most two batches", c.maxTickSteps)
			}
		}},
		{"multi-page idle traversal continues from its cursor", 100, 10, func(c *selectionCounters) []Feature {
			return []Feature{c.feature("idle-a", 1, idle), c.feature("idle-b", 1, idle)}
		}, func(t *testing.T, c *selectionCounters, errs []error) {
			for _, name := range []string{"idle-a", "idle-b"} {
				low, high := c.served[name]["org_00000"], c.served[name]["org_00000"]
				for i := range 100 {
					got := c.served[name][fmt.Sprintf("org_%05d", i)]
					low, high = min(low, got), max(high, got)
				}
				if low < 1 || high-low > 1 {
					t.Fatalf("%s traversal visits range %d..%d: a yielded traversal must resume at its cursor", name, low, high)
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newSelectionCounters()
			scheduler, err := New(c.registry(test.tenants, one(1)), test.families(c))
			if err != nil {
				t.Fatal(err)
			}
			var errs []error
			for range test.ticks {
				c.startTick()
				if err := scheduler.Tick(t.Context()); err != nil {
					if errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					errs = append(errs, err)
				}
				if c.pages > ProbeLimit {
					t.Fatalf("tick read %d registry pages, limit %d", c.pages, ProbeLimit)
				}
				steps := 0
				for key, visits := range c.tick {
					steps += visits
					if visits > 1 {
						t.Fatalf("%s visited %d times in one tick", key, visits)
					}
				}
				if steps > ProbeLimit*TenantLimit {
					t.Fatalf("tick ran %d steps, limit %d", steps, ProbeLimit*TenantLimit)
				}
				c.maxTickSteps = max(c.maxTickSteps, steps)
			}
			test.check(t, c, errs)
		})
	}
}

func TestWorkConservingSelectionSharesOneTickEnvelope(t *testing.T) {
	c := newSelectionCounters()
	var budgets []Budget
	var mu sync.Mutex
	record := func(spent int64) func(Tenant, Budget) (Outcome, error) {
		return func(tenant Tenant, budget Budget) (Outcome, error) {
			mu.Lock()
			budgets = append(budgets, budget)
			mu.Unlock()
			return Outcome{Items: 10}, nil
		}
	}
	scheduler, err := New(c.registry(1, one(1)), []Feature{c.feature("a", ByteLimit/4, record(0)), c.feature("b", ByteLimit/4, record(0)), c.feature("c", ByteLimit/4, record(0)), c.feature("d", ByteLimit/4, record(0))})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []int{ItemLimit, ItemLimit - 10, ItemLimit - 20, ItemLimit - 30}
	if len(budgets) != len(want) {
		t.Fatalf("probes=%d want %d", len(budgets), len(want))
	}
	for i, budget := range budgets {
		if budget.Items != want[i] {
			t.Fatalf("probe %d items=%d want %d: each probe must draw from one tick envelope", i, budget.Items, want[i])
		}
	}
	overspent := Feature{Name: "overspent", Bytes: 1, Step: func(context.Context, Tenant, Budget) (Outcome, error) { return Outcome{Items: ItemLimit + 1}, nil }}
	scheduler, err = New(c.registry(1, one(1)), []Feature{overspent})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Tick(t.Context()); err == nil {
		t.Fatal("step reporting more items than its budget was accepted")
	}
}

func TestBusyTenantLateInLargeRegistryHasFiniteServiceBound(t *testing.T) {
	const tenants = 10000
	c := newSelectionCounters()
	late := fmt.Sprintf("org_%05d", tenants-1)
	var mu sync.Mutex
	generations := map[string]int64{}
	generation := func(id string) int64 {
		mu.Lock()
		defer mu.Unlock()
		if value := generations[id]; value > 0 {
			return value
		}
		return 1
	}
	var seen []int64
	lateBusy := func(tenant Tenant, budget Budget) (Outcome, error) {
		if tenant.Organization != late {
			return Outcome{Items: 1}, nil
		}
		mu.Lock()
		seen = append(seen, tenant.Generation)
		mu.Unlock()
		return Outcome{Progress: true, Items: budget.Items}, nil
	}
	var features []Feature
	for i := range 9 {
		features = append(features, c.feature(fmt.Sprintf("idle-%d", i), 1, idle))
	}
	features = append(features, c.feature("late", 1, lateBusy))
	scheduler, err := New(c.registry(tenants, generation), features)
	if err != nil {
		t.Fatal(err)
	}
	pages := (tenants + TenantLimit - 1) / TenantLimit
	bound := len(features) * pages
	served := -1
	for tick := range 2 * bound {
		c.startTick()
		if tick == bound/2 {
			mu.Lock()
			generations[late] = 2
			mu.Unlock()
		}
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if c.pages > ProbeLimit {
			t.Fatalf("tick %d read %d registry pages", tick, c.pages)
		}
		mu.Lock()
		count := len(seen)
		mu.Unlock()
		if served < 0 && count > 0 {
			served = tick
		}
	}
	if served < 0 || served > bound {
		t.Fatalf("late busy tenant first served at tick %d, derived bound %d", served, bound)
	}
	for name, visits := range c.served {
		if len(visits) != tenants {
			t.Fatalf("family %s reached %d of %d tenants within %d ticks", name, len(visits), tenants, 2*bound)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[len(seen)-1] != 2 {
		t.Fatalf("generation change not observed by later service: %v", seen)
	}
	t.Logf("10000 tenants, %d families, %d pages per traversal: late tenant first served at tick %d, derived bound %d ticks", len(features), pages, served, bound)
}

func TestWorkConservingSelectionStopsOnCancellationAndServesNewWork(t *testing.T) {
	c := newSelectionCounters()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelling := func(Tenant, Budget) (Outcome, error) {
		cancel()
		return Outcome{Items: 1}, nil
	}
	scheduler, err := New(c.registry(1, one(1)), []Feature{c.feature("cancelling", 1, cancelling), c.feature("after", 1, busy)})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Tick(ctx); !errors.Is(err, context.Canceled) || c.steps["after"] != 0 {
		t.Fatalf("cancelled tick err=%v later probes=%d", err, c.steps["after"])
	}
	c = newSelectionCounters()
	var mu sync.Mutex
	ready := false
	arriving := func(Tenant, Budget) (Outcome, error) {
		mu.Lock()
		defer mu.Unlock()
		if ready {
			return Outcome{Progress: true, Items: 1}, nil
		}
		return Outcome{Items: 1}, nil
	}
	features := []Feature{c.feature("arriving", 1, arriving)}
	for i := range 5 {
		features = append(features, c.feature(fmt.Sprintf("busy-%d", i), 1, busy))
	}
	scheduler, err = New(c.registry(1, one(1)), features)
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		c.startTick()
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	before := c.steps["arriving"]
	mu.Lock()
	ready = true
	mu.Unlock()
	for range len(features) {
		c.startTick()
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if c.steps["arriving"] != before+1 {
		t.Fatalf("family that became busy served %d times in one rotation, want 1", c.steps["arriving"]-before)
	}
}
