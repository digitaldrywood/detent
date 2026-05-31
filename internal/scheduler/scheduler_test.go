package scheduler_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/digitaldrywood/symphony-go/internal/scheduler"
)

func TestNewFromConfigSelectsMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		want scheduler.Mode
	}{
		{name: "empty defaults to weighted fair", kind: "", want: scheduler.ModeWeightedFair},
		{name: "weighted fair", kind: "weighted_fair", want: scheduler.ModeWeightedFair},
		{name: "weighted alias", kind: " weighted ", want: scheduler.ModeWeightedFair},
		{name: "strict priority", kind: "strict_priority", want: scheduler.ModeStrictPriority},
		{name: "strict alias", kind: "strict", want: scheduler.ModeStrictPriority},
		{name: "round robin", kind: "round_robin", want: scheduler.ModeRoundRobin},
		{name: "round-robin alias", kind: "round-robin", want: scheduler.ModeRoundRobin},
		{name: "fair share", kind: "fair_share", want: scheduler.ModeFairShare},
		{name: "fair-share alias", kind: "fair-share", want: scheduler.ModeFairShare},
		{name: "counting semaphore compatibility", kind: "counting_semaphore", want: scheduler.ModeCountingSemaphore},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := scheduler.NewFromConfig(scheduler.Config{
				Kind:           tt.kind,
				Capacity:       2,
				FairShareStore: &fairShareStore{},
			})
			if err != nil {
				t.Fatalf("NewFromConfig() error = %v", err)
			}
			if got.Mode() != tt.want {
				t.Fatalf("Mode() = %q, want %q", got.Mode(), tt.want)
			}
		})
	}
}

func TestNewFromConfigRejectsFairShareWithoutStore(t *testing.T) {
	t.Parallel()

	got, err := scheduler.NewFromConfig(scheduler.Config{Kind: "fair_share"})
	if got != nil {
		t.Fatalf("scheduler = %T, want nil", got)
	}
	if !errors.Is(err, scheduler.ErrFairShareStoreRequired) {
		t.Fatalf("error = %v, want ErrFairShareStoreRequired", err)
	}
}

func TestNewFromConfigRejectsUnsupportedBackend(t *testing.T) {
	t.Parallel()

	got, err := scheduler.NewFromConfig(scheduler.Config{Kind: "remote"})
	if got != nil {
		t.Fatalf("scheduler = %T, want nil", got)
	}
	if !errors.Is(err, scheduler.ErrUnsupportedBackend) {
		t.Fatalf("error = %v, want ErrUnsupportedBackend", err)
	}
}

func TestCountingSemaphoreRequestSlotEnforcesGlobalWeightedCapacity(t *testing.T) {
	t.Parallel()

	sched := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: 3})
	first, err := sched.RequestSlot(context.Background(), scheduler.SlotRequest{
		State:  "Todo",
		Weight: 2,
	})
	if err != nil {
		t.Fatalf("RequestSlot() first error = %v", err)
	}

	_, err = sched.RequestSlot(context.Background(), scheduler.SlotRequest{
		State:  "In Progress",
		Weight: 2,
	})
	if !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("RequestSlot() second error = %v, want ErrNoSlots", err)
	}

	if err := sched.ReleaseSlot(first); err != nil {
		t.Fatalf("ReleaseSlot() error = %v", err)
	}
	if _, err := sched.RequestSlot(context.Background(), scheduler.SlotRequest{Weight: 2}); err != nil {
		t.Fatalf("RequestSlot() after release error = %v", err)
	}
}

func TestCountingSemaphoreTracksStateAndHostCounters(t *testing.T) {
	t.Parallel()

	sched := scheduler.NewCountingSemaphore(scheduler.Config{
		Capacity: 4,
		CapacityByState: map[string]int{
			"todo": 3,
		},
		CapacityPerHost: 2,
	})

	first, err := sched.RequestSlot(context.Background(), scheduler.SlotRequest{
		State:  " Todo ",
		Host:   " worker-a ",
		Weight: 2,
	})
	if err != nil {
		t.Fatalf("RequestSlot() first error = %v", err)
	}

	assertCounters(t, sched.Counters(), scheduler.Counters{
		Used:        2,
		UsedByState: map[string]int{"todo": 2},
		UsedByHost:  map[string]int{"worker-a": 2},
	})

	_, err = sched.RequestSlot(context.Background(), scheduler.SlotRequest{
		State: "Todo",
		Host:  "worker-a",
	})
	if !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("RequestSlot() host-capped error = %v, want ErrNoSlots", err)
	}

	second, err := sched.RequestSlot(context.Background(), scheduler.SlotRequest{
		State: "Todo",
		Host:  "worker-b",
	})
	if err != nil {
		t.Fatalf("RequestSlot() second error = %v", err)
	}

	_, err = sched.RequestSlot(context.Background(), scheduler.SlotRequest{
		State: "Todo",
		Host:  "worker-b",
	})
	if !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("RequestSlot() state-capped error = %v, want ErrNoSlots", err)
	}

	if err := sched.ReleaseSlot(first); err != nil {
		t.Fatalf("ReleaseSlot() first error = %v", err)
	}
	assertCounters(t, sched.Counters(), scheduler.Counters{
		Used:        1,
		UsedByState: map[string]int{"todo": 1},
		UsedByHost:  map[string]int{"worker-b": 1},
	})

	if err := sched.ReleaseSlot(second); err != nil {
		t.Fatalf("ReleaseSlot() second error = %v", err)
	}
	assertCounters(t, sched.Counters(), scheduler.Counters{
		UsedByState: map[string]int{},
		UsedByHost:  map[string]int{},
	})
}

func TestCountingSemaphoreValidatesWeight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  scheduler.Config
		req  scheduler.SlotRequest
		want error
	}{
		{
			name: "negative weight",
			cfg:  scheduler.Config{Capacity: 2},
			req:  scheduler.SlotRequest{Weight: -1},
			want: scheduler.ErrInvalidWeight,
		},
		{
			name: "global capacity",
			cfg:  scheduler.Config{Capacity: 2},
			req:  scheduler.SlotRequest{Weight: 3},
			want: scheduler.ErrWeightExceedsCapacity,
		},
		{
			name: "state capacity",
			cfg: scheduler.Config{
				Capacity:        3,
				CapacityByState: map[string]int{"todo": 1},
			},
			req:  scheduler.SlotRequest{State: "Todo", Weight: 2},
			want: scheduler.ErrWeightExceedsCapacity,
		},
		{
			name: "host capacity",
			cfg: scheduler.Config{
				Capacity:        3,
				CapacityPerHost: 1,
			},
			req:  scheduler.SlotRequest{Host: "worker-a", Weight: 2},
			want: scheduler.ErrWeightExceedsCapacity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sched := scheduler.NewCountingSemaphore(tt.cfg)
			_, err := sched.RequestSlot(context.Background(), tt.req)
			if !errors.Is(err, tt.want) {
				t.Fatalf("RequestSlot() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCountingSemaphoreRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sched := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: 1})
	_, err := sched.RequestSlot(ctx, scheduler.SlotRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RequestSlot() error = %v, want context.Canceled", err)
	}
}

func TestCountingSemaphoreRejectsUnknownOrDuplicateRelease(t *testing.T) {
	t.Parallel()

	sched := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: 1})
	if err := sched.ReleaseSlot(scheduler.Slot{}); !errors.Is(err, scheduler.ErrSlotNotHeld) {
		t.Fatalf("ReleaseSlot() unknown error = %v, want ErrSlotNotHeld", err)
	}

	slot, err := sched.RequestSlot(context.Background(), scheduler.SlotRequest{})
	if err != nil {
		t.Fatalf("RequestSlot() error = %v", err)
	}
	if err := sched.ReleaseSlot(slot); err != nil {
		t.Fatalf("ReleaseSlot() first error = %v", err)
	}
	if err := sched.ReleaseSlot(slot); !errors.Is(err, scheduler.ErrSlotNotHeld) {
		t.Fatalf("ReleaseSlot() duplicate error = %v, want ErrSlotNotHeld", err)
	}
}

func TestCountingSemaphoreCountersAreDefensiveCopies(t *testing.T) {
	t.Parallel()

	sched := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: 2})
	if _, err := sched.RequestSlot(context.Background(), scheduler.SlotRequest{
		State: "Todo",
		Host:  "worker-a",
	}); err != nil {
		t.Fatalf("RequestSlot() error = %v", err)
	}

	counters := sched.Counters()
	counters.UsedByState["todo"] = 99
	counters.UsedByHost["worker-a"] = 99

	assertCounters(t, sched.Counters(), scheduler.Counters{
		Used:        1,
		UsedByState: map[string]int{"todo": 1},
		UsedByHost:  map[string]int{"worker-a": 1},
	})
}

func TestCountingSemaphoreSupportsConcurrentRequests(t *testing.T) {
	t.Parallel()

	const capacity = 5
	const workers = 20

	sched := scheduler.NewCountingSemaphore(scheduler.Config{Capacity: capacity})
	start := make(chan struct{})
	results := make(chan error, workers)
	slots := make(chan scheduler.Slot, capacity)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			slot, err := sched.RequestSlot(context.Background(), scheduler.SlotRequest{})
			if err == nil {
				slots <- slot
			}
			results <- err
		}()
	}

	close(start)
	wg.Wait()
	close(results)
	close(slots)

	successes := 0
	failures := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, scheduler.ErrNoSlots):
			failures++
		default:
			t.Fatalf("RequestSlot() error = %v", err)
		}
	}

	if successes != capacity {
		t.Fatalf("successful requests = %d, want %d", successes, capacity)
	}
	if failures != workers-capacity {
		t.Fatalf("failed requests = %d, want %d", failures, workers-capacity)
	}

	for slot := range slots {
		if err := sched.ReleaseSlot(slot); err != nil {
			t.Fatalf("ReleaseSlot() error = %v", err)
		}
	}
}

func assertCounters(t *testing.T, got scheduler.Counters, want scheduler.Counters) {
	t.Helper()

	if got.Used != want.Used {
		t.Fatalf("Used = %d, want %d", got.Used, want.Used)
	}
	if !reflect.DeepEqual(got.UsedByState, want.UsedByState) {
		t.Fatalf("UsedByState = %#v, want %#v", got.UsedByState, want.UsedByState)
	}
	if !reflect.DeepEqual(got.UsedByHost, want.UsedByHost) {
		t.Fatalf("UsedByHost = %#v, want %#v", got.UsedByHost, want.UsedByHost)
	}
}
