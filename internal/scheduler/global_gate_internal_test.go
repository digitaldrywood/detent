package scheduler

import (
	"runtime"
	"testing"
	"time"
)

func TestGlobalDispatchGateSetProjectsCleansCycleRecords(t *testing.T) {
	t.Parallel()

	configured := ProjectCandidate{ID: "configured", Weight: 1}
	orphan := ProjectCandidate{ID: "orphan", Weight: 1}
	for _, tt := range []struct {
		name      string
		cycle     ProjectCandidate
		wantCycle bool
	}{
		{name: "configured project cycle remains", cycle: configured, wantCycle: true},
		{name: "cycle-only orphan is removed", cycle: orphan},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gate := NewGlobalDispatchGate(
				NewStrictPriority(Config{Capacity: 1}),
				configured,
			)
			gate.MarkIdle(tt.cycle)
			gate.SetProjects([]ProjectCandidate{configured})

			cycle, ok := gate.projectCycles[tt.cycle.ID]
			if ok != tt.wantCycle {
				t.Fatalf("projectCycles[%q] present = %t, want %t", tt.cycle.ID, ok, tt.wantCycle)
			}
			if ok && !cycle.idle {
				t.Fatalf("projectCycles[%q].idle = false, want true", tt.cycle.ID)
			}
		})
	}
}

func TestGlobalDispatchGateReadyRequests(t *testing.T) {
	for _, tt := range []struct {
		name         string
		capacity     int
		higherWeight int
		wantHigher   bool
		wantLower    bool
	}{
		{"highest rank wins last slot", 1, 1, true, false},
		{"fill remaining capacity", 2, 1, true, true},
		{"oversized request owns nothing", 1, 2, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gate := NewGlobalDispatchGate(NewStrictPriority(Config{Capacity: tt.capacity}))
			lower := &dispatchRequest{ctx: t.Context(), project: ProjectCandidate{ID: "lower", Priority: 4}, request: SlotRequest{State: "Todo"}}
			higher := &dispatchRequest{ctx: t.Context(), project: ProjectCandidate{ID: "higher", Priority: 1}, request: SlotRequest{State: "Merging", Weight: tt.higherWeight}}
			gate.dispatchLocked([]*dispatchRequest{lower, higher})
			if higher.granted != tt.wantHigher || lower.granted != tt.wantLower {
				t.Fatalf("higher/lower grants = %t/%t, want %t/%t", higher.granted, lower.granted, tt.wantHigher, tt.wantLower)
			}
			if !lower.granted && (lower.err != nil || lower.decision.Reason != DispatchGateReasonGlobalCapacityFull || lower.decision.GlobalAvailable != 0) {
				t.Fatalf("lower refusal = %+v, %v", lower.decision, lower.err)
			}
			if higher.granted {
				if err := gate.Release(higher.slot); err != nil {
					t.Fatal(err)
				}
			}
			if !lower.granted {
				slot, ok, err := gate.TryAcquire(t.Context(), lower.project, lower.request, time.Time{})
				if err != nil || !ok {
					t.Fatalf("next freed slot = %t, %v", ok, err)
				}
				lower.slot = slot
			}
			if err := gate.Release(lower.slot); err != nil {
				t.Fatal(err)
			}
			if got := gate.PoolSnapshot().Used; got != 0 {
				t.Fatalf("used after release = %d", got)
			}
		})
	}
}

func TestGlobalDispatchGateConcurrentReadyRequests(t *testing.T) {
	for _, kind := range []string{"strict", "weighted", "round_robin"} {
		t.Run(kind, func(t *testing.T) {
			global, err := NewFromConfig(Config{Kind: kind, Capacity: 1})
			if err != nil {
				t.Fatal(err)
			}
			gate := NewGlobalDispatchGate(global.(GlobalScheduler))
			gate.mu.Lock()
			type result struct {
				id       string
				slot     Slot
				ok       bool
				err      error
				decision DispatchGateDecision
			}
			results := make(chan result, 2)
			for _, project := range []ProjectCandidate{{ID: "lower", Priority: 4}, {ID: "higher", Priority: 1}} {
				go func() {
					slot, ok, decision, err := gate.TryAcquireWithDecision(t.Context(), project, SlotRequest{State: "Todo", Priority: project.Priority}, time.Time{})
					results <- result{project.ID, slot, ok, err, decision}
				}()
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				gate.requestMu.Lock()
				n := len(gate.pending)
				gate.requestMu.Unlock()
				if n == 2 {
					break
				}
				if time.Now().After(deadline) {
					gate.mu.Unlock()
					t.Fatal("acquisition callers did not reach gate")
				}
				runtime.Gosched()
			}
			gate.mu.Unlock()
			var held Slot
			for range 2 {
				result := <-results
				if result.err != nil || result.ok != (result.id == "higher") {
					t.Fatalf("acquisition = %+v", result)
				}
				if result.ok {
					held = result.slot
				} else if result.decision.Reason != DispatchGateReasonGlobalCapacityFull {
					t.Fatalf("refusal = %+v", result.decision)
				}
			}
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
		})
	}
}
