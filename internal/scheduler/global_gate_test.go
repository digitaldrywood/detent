package scheduler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestGlobalDispatchGateUsesConfiguredProjectSelection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
	alpha := scheduler.ProjectCandidate{ID: "alpha", Weight: 1}
	bravo := scheduler.ProjectCandidate{ID: "bravo", Weight: 1}

	slot, ok, err := gate.TryAcquire(ctx, alpha, scheduler.SlotRequest{State: "Todo"}, now)
	if err != nil {
		t.Fatalf("alpha TryAcquire() error = %v", err)
	}
	if !ok {
		t.Fatal("alpha TryAcquire() ok = false, want true")
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	gate.MarkReady(bravo)
	if _, ok, err := gate.TryAcquire(ctx, alpha, scheduler.SlotRequest{State: "Todo"}, now.Add(time.Second)); err != nil {
		t.Fatalf("alpha second TryAcquire() error = %v", err)
	} else if ok {
		t.Fatal("alpha second TryAcquire() ok = true, want false while bravo has the round-robin turn")
	}

	slot, ok, err = gate.TryAcquire(ctx, bravo, scheduler.SlotRequest{State: "Todo"}, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("bravo TryAcquire() error = %v", err)
	}
	if !ok {
		t.Fatal("bravo TryAcquire() ok = false, want true")
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("bravo Release() error = %v", err)
	}
}

func TestGlobalDispatchGateReservesFreedSlotForPendingMergeLane(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 1}))
	todoProject := scheduler.ProjectCandidate{ID: "alpha", Weight: 1}
	mergeProject := scheduler.ProjectCandidate{ID: "zulu", Weight: 1}

	todoSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, todoProject, scheduler.SlotRequest{
		State:    "Todo",
		Priority: 2,
	}, now)
	if err != nil {
		t.Fatalf("todo TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("todo TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}

	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Priority: 0,
	}, now.Add(time.Second)); err != nil {
		t.Fatalf("merge waiting TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("merge waiting TryAcquireWithDecision() ok = true while the global slot is held, want false")
	} else if decision.SelectedProjectID != mergeProject.ID || decision.Reason != scheduler.DispatchGateReasonGlobalCapacityFull {
		t.Fatalf("merge waiting decision = %#v, want selected merge with global capacity full", decision)
	}

	if err := gate.Release(todoSlot); err != nil {
		t.Fatalf("todo Release() error = %v", err)
	}

	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, todoProject, scheduler.SlotRequest{
		State:    "Rework",
		Priority: 1,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("rework TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("rework TryAcquireWithDecision() ok = true while pending merge has the reserved turn, want false")
	} else {
		if decision.SelectedProjectID != mergeProject.ID {
			t.Fatalf("rework decision selected project = %q, want %q", decision.SelectedProjectID, mergeProject.ID)
		}
		if decision.SelectedState != "merging" {
			t.Fatalf("rework decision selected state = %q, want merging", decision.SelectedState)
		}
		if decision.Reason != scheduler.DispatchGateReasonReservedForHigherPriority {
			t.Fatalf("rework decision reason = %q, want %q", decision.Reason, scheduler.DispatchGateReasonReservedForHigherPriority)
		}
		if decision.GlobalCapacity != 1 || decision.GlobalUsed != 0 || decision.GlobalAvailable != 1 {
			t.Fatalf("rework decision global capacity = %d used = %d available = %d, want 1/0/1",
				decision.GlobalCapacity, decision.GlobalUsed, decision.GlobalAvailable)
		}
		if decision.LowerPriorityRunning != 0 {
			t.Fatalf("rework decision lower-priority running = %d, want 0 after release", decision.LowerPriorityRunning)
		}
	}

	mergeSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Priority: 0,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("merge TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("merge TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}
	if decision.Reason != scheduler.DispatchGateReasonGranted {
		t.Fatalf("merge decision reason = %q, want %q", decision.Reason, scheduler.DispatchGateReasonGranted)
	}
	if err := gate.Release(mergeSlot); err != nil {
		t.Fatalf("merge Release() error = %v", err)
	}
}

func TestSchedulersAllowOneMergeLanePerProject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 25, 12, 30, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 2}))
	alphaProject := scheduler.ProjectCandidate{ID: "alpha", Weight: 1}
	bravoProject := scheduler.ProjectCandidate{ID: "bravo", Weight: 1}
	alphaLocal := scheduler.NewCountingSemaphore(scheduler.Config{
		Capacity:        2,
		CapacityByState: map[string]int{"Merging": 1},
	})
	bravoLocal := scheduler.NewCountingSemaphore(scheduler.Config{
		Capacity:        2,
		CapacityByState: map[string]int{"Merging": 1},
	})
	mergeReq := scheduler.SlotRequest{State: "Merging", Priority: 0}

	alphaLocalSlot, err := alphaLocal.RequestSlot(ctx, mergeReq)
	if err != nil {
		t.Fatalf("alpha local RequestSlot() error = %v", err)
	}
	alphaGlobalSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, alphaProject, mergeReq, now)
	if err != nil {
		t.Fatalf("alpha global TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("alpha global TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}

	bravoLocalSlot, err := bravoLocal.RequestSlot(ctx, mergeReq)
	if err != nil {
		t.Fatalf("bravo local RequestSlot() error = %v", err)
	}
	bravoGlobalSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, bravoProject, mergeReq, now.Add(time.Second))
	if err != nil {
		t.Fatalf("bravo global TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("bravo global TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}

	if _, err := alphaLocal.RequestSlot(ctx, mergeReq); !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("alpha second local RequestSlot() error = %v, want ErrNoSlots", err)
	}

	if err := alphaLocal.ReleaseSlot(alphaLocalSlot); err != nil {
		t.Fatalf("alpha local ReleaseSlot() error = %v", err)
	}
	if err := bravoLocal.ReleaseSlot(bravoLocalSlot); err != nil {
		t.Fatalf("bravo local ReleaseSlot() error = %v", err)
	}
	if err := gate.Release(alphaGlobalSlot); err != nil {
		t.Fatalf("alpha global Release() error = %v", err)
	}
	if err := gate.Release(bravoGlobalSlot); err != nil {
		t.Fatalf("bravo global Release() error = %v", err)
	}
}

func TestGlobalDispatchGateHonorsStrictPriorityPreemption(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
	low := scheduler.ProjectCandidate{ID: "low", Weight: 1, Priority: 4}
	urgent := scheduler.ProjectCandidate{ID: "urgent", Weight: 1, Priority: 1}

	lowSlot, ok, err := gate.TryAcquire(ctx, low, scheduler.SlotRequest{State: "Todo"}, now)
	if err != nil {
		t.Fatalf("low TryAcquire() error = %v", err)
	}
	if !ok {
		t.Fatal("low TryAcquire() ok = false, want true")
	}
	preempted := make(chan struct{})
	gate.SetPreempt(lowSlot, func() {
		close(preempted)
	})

	urgentSlot, ok, err := gate.TryAcquire(ctx, urgent, scheduler.SlotRequest{State: "Todo"}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("urgent TryAcquire() error = %v", err)
	}
	if !ok {
		t.Fatal("urgent TryAcquire() ok = false, want true")
	}
	select {
	case <-preempted:
	default:
		t.Fatal("low-priority project was not preempted")
	}
	if err := gate.Release(lowSlot); err != nil {
		t.Fatalf("low Release() after preemption error = %v", err)
	}
	if err := gate.Release(urgentSlot); err != nil {
		t.Fatalf("urgent Release() error = %v", err)
	}
}
