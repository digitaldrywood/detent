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

// Release must rank a retained executable borrower with a newly enrolled caller
// from another pool before either acquires the last shared slot.
func TestPoolRegistryRanksBorrowersAcrossPools(t *testing.T) {
	for _, tc := range []struct {
		name           string
		kind           string
		lowerKind      string
		higherPriority int
		lowerPriority  int
		higherLane     int
		lowerLane      int
	}{
		{name: "strict project rank", kind: "strict", higherPriority: 1, lowerPriority: 4},
		{name: "round robin lane rank", kind: "round_robin", higherLane: 1, lowerLane: 4},
		{name: "weighted lane rank", kind: "weighted", higherLane: 1, lowerLane: 4},
		{name: "mixed mode neutral project rank", kind: "round_robin", lowerKind: "strict", lowerPriority: 4, higherLane: 4, lowerLane: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lowerKind := tc.lowerKind
			if lowerKind == "" {
				lowerKind = tc.kind
			}
			lender := ProjectCandidate{ID: "lender"}
			higher := ProjectCandidate{ID: "higher", Pool: "alpha", Priority: tc.higherPriority}
			lower := ProjectCandidate{ID: "lower", Pool: "beta", Priority: tc.lowerPriority}
			registry, err := NewPoolRegistry([]PoolConfig{
				{Name: DefaultPoolName, Scheduler: Config{Kind: tc.kind, Capacity: 1}},
				{Name: "alpha", BurstTo: 2, Scheduler: Config{Kind: tc.kind, Capacity: 1}},
				{Name: "beta", BurstTo: 2, Scheduler: Config{Kind: lowerKind, Capacity: 1}},
			}, []ProjectCandidate{lender, higher, lower})
			if err != nil {
				t.Fatal(err)
			}
			var held []Slot
			for _, project := range []ProjectCandidate{lender, higher, lower} {
				slot, ok, err := registry.TryAcquire(t.Context(), project, SlotRequest{State: "Todo"}, time.Now())
				if err != nil || !ok {
					t.Fatalf("fill %s: granted=%t err=%v", project.ID, ok, err)
				}
				held = append(held, slot)
			}
			response, cancel, decision := registry.Submit(t.Context(), higher, SlotRequest{State: "Todo", Priority: tc.higherLane}, time.Now(), nil)
			defer cancel()
			if decision.Reason != DispatchGateReasonGlobalCapacityFull {
				t.Fatalf("queued decision = %+v", decision)
			}
			// Freeze the synchronous caller after intake enrollment, just as it
			// waits for reconfigureMu. Release must include it in the same ranking.
			call := &dispatchRequest{ctx: t.Context(), project: lower, request: SlotRequest{State: "Todo", Priority: tc.lowerLane}, now: time.Now()}
			registry.requestMu.Lock()
			registry.pending = append(registry.pending, call)
			registry.requestMu.Unlock()
			if err := registry.Release(held[0]); err != nil {
				t.Fatal(err)
			}
			var grant DispatchResult
			select {
			case grant = <-response:
				if grant.Err != nil {
					t.Fatal(grant.Err)
				}
			default:
				t.Fatal("higher-ranked borrower did not receive freed shared capacity")
			}
			if call.slot != (Slot{}) || call.granted || call.err != nil || call.decision.Reason != DispatchGateReasonGlobalCapacityFull || call.decision.SharedAvailable != 0 {
				t.Fatalf("lower borrower = %+v", call)
			}
			if err := registry.Release(grant.Slot); err != nil {
				t.Fatal(err)
			}
			next, ok, decision, err := registry.TryAcquireWithDecision(t.Context(), lower, call.request, time.Now())
			if err != nil || !ok {
				t.Fatalf("next borrow: granted=%t decision=%+v err=%v", ok, decision, err)
			}
			for _, slot := range append(held[1:], next) {
				if err := registry.Release(slot); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
