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

func TestGlobalDispatchGateReselectsHigherPriorityWaitingLane(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 25, 12, 15, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 1}))
	todoProject := scheduler.ProjectCandidate{ID: "alpha", Weight: 1}
	reworkProject := scheduler.ProjectCandidate{ID: "bravo", Weight: 1}
	mergeProject := scheduler.ProjectCandidate{ID: "charlie", Weight: 1}

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

	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, reworkProject, scheduler.SlotRequest{
		State:    "Rework",
		Priority: 1,
	}, now.Add(time.Second)); err != nil {
		t.Fatalf("rework waiting TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("rework waiting TryAcquireWithDecision() ok = true while the global slot is held, want false")
	} else if decision.SelectedProjectID != reworkProject.ID || decision.Reason != scheduler.DispatchGateReasonGlobalCapacityFull {
		t.Fatalf("rework waiting decision = %#v, want selected rework with global capacity full", decision)
	}

	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Priority: 0,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("merge waiting TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("merge waiting TryAcquireWithDecision() ok = true while the global slot is held, want false")
	} else if decision.SelectedProjectID != mergeProject.ID || decision.Reason != scheduler.DispatchGateReasonGlobalCapacityFull {
		t.Fatalf("merge waiting decision = %#v, want selected merge with global capacity full", decision)
	}

	if err := gate.Release(todoSlot); err != nil {
		t.Fatalf("todo Release() error = %v", err)
	}

	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, reworkProject, scheduler.SlotRequest{
		State:    "Rework",
		Priority: 1,
	}, now.Add(3*time.Second)); err != nil {
		t.Fatalf("rework retry TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("rework retry TryAcquireWithDecision() ok = true while merge has the reserved turn, want false")
	} else if decision.SelectedProjectID != mergeProject.ID || decision.Reason != scheduler.DispatchGateReasonReservedForHigherPriority {
		t.Fatalf("rework retry decision = %#v, want merge reserved for higher-priority state", decision)
	}

	mergeSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Priority: 0,
	}, now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("merge TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("merge TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}
	if err := gate.Release(mergeSlot); err != nil {
		t.Fatalf("merge Release() error = %v", err)
	}
}

func TestGlobalDispatchGateUsesUnreservedCapacityBehindPendingHigherPriorityLane(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 5}))

	runningSlots := make([]scheduler.Slot, 0, 5)
	for index, project := range []scheduler.ProjectCandidate{
		{ID: "running-alpha", Weight: 1},
		{ID: "running-bravo", Weight: 1},
		{ID: "running-charlie", Weight: 1},
		{ID: "running-delta", Weight: 1},
		{ID: "running-echo", Weight: 1},
	} {
		slot, ok, decision, err := gate.TryAcquireWithDecision(ctx, project, scheduler.SlotRequest{
			State:    "Todo",
			Priority: 2,
		}, now.Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatalf("%s TryAcquireWithDecision() error = %v", project.ID, err)
		}
		if !ok {
			t.Fatalf("%s TryAcquireWithDecision() ok = false, want true; decision = %#v", project.ID, decision)
		}
		runningSlots = append(runningSlots, slot)
	}
	t.Cleanup(func() {
		for _, slot := range runningSlots {
			if err := gate.Release(slot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
				t.Fatalf("Release() error = %v", err)
			}
		}
	})

	mergeProject := scheduler.ProjectCandidate{ID: "merge", Weight: 1}
	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Priority: 0,
	}, now.Add(5*time.Second)); err != nil {
		t.Fatalf("merge waiting TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("merge waiting TryAcquireWithDecision() ok = true while all global slots are held, want false")
	} else if decision.SelectedProjectID != mergeProject.ID || decision.Reason != scheduler.DispatchGateReasonGlobalCapacityFull {
		t.Fatalf("merge waiting decision = %#v, want selected merge with global capacity full", decision)
	}

	for _, slot := range runningSlots[:3] {
		if err := gate.Release(slot); err != nil {
			t.Fatalf("Release() error = %v", err)
		}
	}

	reworkProject := scheduler.ProjectCandidate{ID: "rework", Weight: 1}
	reworkSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, reworkProject, scheduler.SlotRequest{
		State:    "Rework",
		Priority: 1,
	}, now.Add(6*time.Second))
	if err != nil {
		t.Fatalf("rework TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("rework TryAcquireWithDecision() ok = false, want true with unreserved capacity; decision = %#v", decision)
	}
	if decision.Reason != scheduler.DispatchGateReasonGranted {
		t.Fatalf("rework decision reason = %q, want %q", decision.Reason, scheduler.DispatchGateReasonGranted)
	}
	if decision.GlobalCapacity != 5 || decision.GlobalUsed != 3 || decision.GlobalAvailable != 2 {
		t.Fatalf("rework decision global capacity = %d used = %d available = %d, want 5/3/2",
			decision.GlobalCapacity, decision.GlobalUsed, decision.GlobalAvailable)
	}
	t.Cleanup(func() {
		if err := gate.Release(reworkSlot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
			t.Fatalf("Release() error = %v", err)
		}
	})

	mergeSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Priority: 0,
	}, now.Add(7*time.Second))
	if err != nil {
		t.Fatalf("merge TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("merge TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}
	t.Cleanup(func() {
		if err := gate.Release(mergeSlot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
			t.Fatalf("Release() error = %v", err)
		}
	})
}

func TestGlobalDispatchGateReservesPendingSelectionWeight(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 6, 26, 9, 30, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 3}))
	runningProject := scheduler.ProjectCandidate{ID: "running", Weight: 1}

	runningSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, runningProject, scheduler.SlotRequest{
		State:    "Todo",
		Weight:   3,
		Priority: 2,
	}, now)
	if err != nil {
		t.Fatalf("running TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("running TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}
	t.Cleanup(func() {
		if err := gate.Release(runningSlot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
			t.Fatalf("Release() error = %v", err)
		}
	})

	mergeProject := scheduler.ProjectCandidate{ID: "merge", Weight: 1}
	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Weight:   2,
		Priority: 0,
	}, now.Add(time.Second)); err != nil {
		t.Fatalf("merge waiting TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("merge waiting TryAcquireWithDecision() ok = true while all global slots are held, want false")
	} else if decision.SelectedProjectID != mergeProject.ID || decision.Reason != scheduler.DispatchGateReasonGlobalCapacityFull {
		t.Fatalf("merge waiting decision = %#v, want selected merge with global capacity full", decision)
	}

	if err := gate.Release(runningSlot); err != nil {
		t.Fatalf("running Release() error = %v", err)
	}

	reworkProject := scheduler.ProjectCandidate{ID: "rework", Weight: 1}
	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, reworkProject, scheduler.SlotRequest{
		State:    "Rework",
		Weight:   2,
		Priority: 1,
	}, now.Add(2*time.Second)); err != nil {
		t.Fatalf("rework TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("rework TryAcquireWithDecision() ok = true while only one unreserved slot remains, want false")
	} else {
		if decision.SelectedProjectID != mergeProject.ID {
			t.Fatalf("rework decision selected project = %q, want %q", decision.SelectedProjectID, mergeProject.ID)
		}
		if decision.Reason != scheduler.DispatchGateReasonReservedForHigherPriority {
			t.Fatalf("rework decision reason = %q, want %q", decision.Reason, scheduler.DispatchGateReasonReservedForHigherPriority)
		}
		if decision.GlobalCapacity != 3 || decision.GlobalUsed != 0 || decision.GlobalAvailable != 3 {
			t.Fatalf("rework decision global capacity = %d used = %d available = %d, want 3/0/3",
				decision.GlobalCapacity, decision.GlobalUsed, decision.GlobalAvailable)
		}
	}

	mergeSlot, ok, decision, err := gate.TryAcquireWithDecision(ctx, mergeProject, scheduler.SlotRequest{
		State:    "Merging",
		Weight:   2,
		Priority: 0,
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("merge TryAcquireWithDecision() error = %v", err)
	}
	if !ok {
		t.Fatalf("merge TryAcquireWithDecision() ok = false, want true; decision = %#v", decision)
	}
	t.Cleanup(func() {
		if err := gate.Release(mergeSlot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
			t.Fatalf("Release() error = %v", err)
		}
	})
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
