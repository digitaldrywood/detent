package scheduler

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestPoolRegistrySerializesReadyRouteWithReconfigure(t *testing.T) {
	t.Parallel()

	project := ProjectCandidate{ID: "worker", Pool: "code"}
	registry, err := NewPoolRegistry([]PoolConfig{
		{Name: DefaultPoolName, Scheduler: Config{Kind: "weighted", Capacity: 1}},
		{Name: "code", Scheduler: Config{Kind: "weighted", Capacity: 1}},
		{Name: "video", Scheduler: Config{Kind: "weighted", Capacity: 1}},
	}, []ProjectCandidate{project})
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	if registry == nil {
		t.Fatal("NewPoolRegistry() = nil")
		return
	}
	oldRuntime, ok := registry.active["code"]
	if !ok || oldRuntime == nil || oldRuntime.gate == nil {
		t.Fatalf("code pool runtime = %#v, want configured gate", oldRuntime)
		return
	}
	oldRuntime.gate.mu.Lock()

	readyDone := make(chan struct{})
	go func() {
		defer close(readyDone)
		registry.MarkReady(project)
	}()

	deadline := time.Now().Add(time.Second)
	for registry.reconfigureMu.TryLock() {
		registry.reconfigureMu.Unlock()
		if time.Now().After(deadline) {
			oldRuntime.gate.mu.Unlock()
			<-readyDone
			t.Fatal("MarkReady() did not lock routing")
		}
		runtime.Gosched()
	}

	reconfigureDone := make(chan error, 1)
	go func() {
		reconfigureDone <- registry.Reconfigure([]PoolConfig{
			{Name: DefaultPoolName, Scheduler: Config{Kind: "weighted", Capacity: 1}},
			{Name: "code", Scheduler: Config{Kind: "weighted", Capacity: 1}},
			{Name: "video", Scheduler: Config{Kind: "weighted", Capacity: 1}},
		}, []ProjectCandidate{{ID: project.ID, Pool: "video"}})
	}()

	oldRuntime.gate.mu.Unlock()
	<-readyDone
	if err := <-reconfigureDone; err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	oldRuntime.gate.mu.Lock()
	_, ready := oldRuntime.gate.ready[project.ID]
	_, configured := oldRuntime.gate.projects[project.ID]
	oldRuntime.gate.mu.Unlock()
	if ready || configured {
		t.Fatalf("old pool retained project after reassignment: ready=%t configured=%t", ready, configured)
	}
	if snapshot := registry.PoolSnapshotFor(project.ID); snapshot.Name != "video" {
		t.Fatalf("PoolSnapshotFor() = %#v, want video", snapshot)
	}
}

func TestPoolRegistryConcurrentAcquisitionRetainsSlotIdentity(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		for _, higherFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("capacity=%d/higherFirst=%t", capacity, higherFirst), func(t *testing.T) {
				higher := ProjectCandidate{ID: "higher", Pool: "code", Priority: 1}
				lower := ProjectCandidate{ID: "lower", Pool: "code", Priority: 4}
				projects := []ProjectCandidate{lower, higher}
				if higherFirst {
					projects[0], projects[1] = projects[1], projects[0]
				}
				registry, err := NewPoolRegistry([]PoolConfig{{Name: DefaultPoolName, Scheduler: Config{Kind: "strict", Capacity: 1}}, {Name: "code", Scheduler: Config{Kind: "strict", Capacity: capacity}}}, projects)
				if err != nil {
					t.Fatal(err)
				}
				registry.reconfigureMu.Lock()
				type result struct {
					project ProjectCandidate
					slot    Slot
					ok      bool
					err     error
				}
				results := make(chan result, 2)
				for index, project := range projects {
					go func() {
						slot, ok, err := registry.TryAcquire(t.Context(), project, SlotRequest{State: "Todo"}, time.Time{})
						results <- result{project, slot, ok, err}
					}()
					deadline := time.Now().Add(5 * time.Second)
					for {
						registry.requestMu.Lock()
						n := len(registry.pending)
						registry.requestMu.Unlock()
						if n == index+1 {
							break
						}
						if time.Now().After(deadline) {
							registry.reconfigureMu.Unlock()
							t.Fatal("caller did not queue")
						}
						runtime.Gosched()
					}
				}
				registry.reconfigureMu.Unlock()
				var slots []Slot
				for range 2 {
					result := <-results
					want := capacity == 2 || result.project.ID == higher.ID
					if result.err != nil || result.ok != want {
						t.Fatalf("acquisition = %+v, want granted %t", result, want)
					}
					if result.ok {
						slots = append(slots, result.slot)
					}
				}
				for _, slot := range slots {
					if slot.poolName != "code" || slot.poolGeneration == 0 {
						t.Fatalf("missing pool identity: %+v", slot)
					}
					if err := registry.Release(slot); err != nil {
						t.Fatal(err)
					}
				}
				if used := registry.PoolSnapshotFor(higher.ID).Used; used != 0 {
					t.Fatalf("leaked %d slots", used)
				}
			})
		}
	}
}
