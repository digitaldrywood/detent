package scheduler_test

import (
	"context"
	"errors"
	"slices"
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

func TestGlobalDispatchGateReleaseIsIdempotentWhenSlotIsNotHeld(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name             string
		releaseOutOfBand bool
	}{
		{name: "held slot"},
		{name: "slot released out of band", releaseOutOfBand: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			global := scheduler.NewStrictPriority(scheduler.Config{Capacity: 1})
			gate := scheduler.NewGlobalDispatchGate(global)
			project := scheduler.ProjectCandidate{ID: "alpha", Weight: 1}
			slot, acquired, err := gate.TryAcquire(
				t.Context(),
				project,
				scheduler.SlotRequest{State: "Todo"},
				time.Date(2026, 7, 31, 13, 2, 34, 0, time.UTC),
			)
			if err != nil {
				t.Fatalf("TryAcquire() error = %v", err)
			}
			if !acquired {
				t.Fatal("TryAcquire() acquired = false, want true")
			}
			if tt.releaseOutOfBand {
				if err := global.ReleaseSlot(slot); err != nil {
					t.Fatalf("out-of-band ReleaseSlot() error = %v", err)
				}
			}

			if err := gate.Release(slot); err != nil {
				t.Fatalf("Release() error = %v, want nil", err)
			}
			if snapshot := gate.PoolSnapshot(); snapshot.Used != 0 || len(snapshot.Holders) != 0 {
				t.Fatalf("PoolSnapshot() = %#v, want no usage or holders", snapshot)
			}
			if err := gate.Release(slot); err != nil {
				t.Fatalf("duplicate Release() error = %v, want nil", err)
			}
		})
	}
}

func TestGlobalDispatchGatePauseBlocksNewSlotsUntilEveryReservationReleases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
	project := scheduler.ProjectCandidate{ID: "alpha", Weight: 1}
	firstRelease := gate.PauseDispatch()
	secondRelease := gate.PauseDispatch()

	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, project, scheduler.SlotRequest{State: "Todo"}, now); err != nil {
		t.Fatalf("TryAcquireWithDecision() while paused error = %v", err)
	} else if ok || decision.Reason != scheduler.DispatchGateReasonPaused {
		t.Fatalf("TryAcquireWithDecision() while paused ok = %t decision = %#v", ok, decision)
	}

	firstRelease()
	firstRelease()
	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, project, scheduler.SlotRequest{State: "Todo"}, now.Add(time.Second)); err != nil {
		t.Fatalf("TryAcquireWithDecision() with nested pause error = %v", err)
	} else if ok || decision.Reason != scheduler.DispatchGateReasonPaused {
		t.Fatalf("TryAcquireWithDecision() with nested pause ok = %t decision = %#v", ok, decision)
	}

	secondRelease()
	slot, ok, decision, err := gate.TryAcquireWithDecision(ctx, project, scheduler.SlotRequest{State: "Todo"}, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("TryAcquireWithDecision() after release error = %v", err)
	}
	if !ok || decision.Reason != scheduler.DispatchGateReasonGranted {
		t.Fatalf("TryAcquireWithDecision() after release ok = %t decision = %#v", ok, decision)
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("Release() error = %v", err)
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
	} else if !slices.Equal(decision.Holders, []string{todoProject.ID}) {
		t.Fatalf("merge waiting holders = %#v, want refusal-time holder %q", decision.Holders, todoProject.ID)
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

func TestGlobalDispatchGateStrictProjectPriorityIsIndependentOfDemandOrder(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		order []string
	}{
		{name: "higher priority first", order: []string{"higher", "lower"}},
		{name: "lower priority first", order: []string{"lower", "higher"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			now := time.Date(2026, 7, 10, 14, 7, 38, 0, time.Local)
			projects := map[string]scheduler.ProjectCandidate{
				"higher": {ID: "detent", Weight: 1, Priority: 0},
				"lower":  {ID: "gopher-ai", Weight: 1, Priority: 3},
			}
			gate := scheduler.NewGlobalDispatchGate(
				scheduler.NewStrictPriority(scheduler.Config{Capacity: 5}),
				projects["higher"],
				projects["lower"],
			)
			demand := map[string]int{"higher": 5, "lower": 3}
			grants := map[string]int{}
			decisions := map[string][]scheduler.DispatchGateDecision{}

			for _, projectName := range tt.order {
				gate.BeginProjectCycle(projects[projectName])
				for range demand[projectName] {
					project := projects[projectName]
					slot, ok, decision, err := gate.TryAcquireWithDecision(
						ctx,
						project,
						scheduler.SlotRequest{State: "Todo"},
						now,
					)
					if err != nil {
						t.Fatalf("%s TryAcquireWithDecision() error = %v", projectName, err)
					}
					decisions[projectName] = append(decisions[projectName], decision)
					if !ok {
						continue
					}
					grants[projectName]++
					t.Cleanup(func() {
						if err := gate.Release(slot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
							t.Errorf("Release() error = %v", err)
						}
					})
				}
				gate.EndProjectCycle(projects[projectName].ID)
			}

			if grants["higher"] != 5 || grants["lower"] != 0 {
				t.Fatalf("grants = %#v, want higher=5 lower=0; decisions = %#v", grants, decisions)
			}
			for _, decision := range decisions["lower"] {
				if decision.Reason != scheduler.DispatchGateReasonReservedForHigherPriorityProject {
					t.Fatalf("lower-priority decision reason = %q, want %q; decision = %#v",
						decision.Reason,
						scheduler.DispatchGateReasonReservedForHigherPriorityProject,
						decision,
					)
				}
			}
		})
	}
}

func TestGlobalDispatchGateStrictProjectPriorityUsesLeftoverCapacity(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		higherDemand int
	}{
		{name: "higher priority queue empty"},
		{name: "higher priority project cap reached", higherDemand: 2},
		{name: "higher priority candidates skipped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			now := time.Date(2026, 7, 10, 14, 8, 0, 0, time.Local)
			higher := scheduler.ProjectCandidate{ID: "detent", Weight: 1, Priority: 0}
			lower := scheduler.ProjectCandidate{ID: "gopher-ai", Weight: 1, Priority: 3}
			gate := scheduler.NewGlobalDispatchGate(
				scheduler.NewStrictPriority(scheduler.Config{Capacity: 5}),
				higher,
				lower,
			)

			gate.BeginProjectCycle(higher)
			for range tt.higherDemand {
				slot, ok, decision, err := gate.TryAcquireWithDecision(
					ctx,
					higher,
					scheduler.SlotRequest{State: "Todo"},
					now,
				)
				if err != nil {
					t.Fatalf("higher TryAcquireWithDecision() error = %v", err)
				}
				if !ok {
					t.Fatalf("higher TryAcquireWithDecision() ok = false; decision = %#v", decision)
				}
				t.Cleanup(func() {
					if err := gate.Release(slot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
						t.Errorf("higher Release() error = %v", err)
					}
				})
			}
			gate.EndProjectCycle(higher.ID)

			gate.BeginProjectCycle(lower)
			lowerGrants := 0
			for range 5 - tt.higherDemand {
				slot, ok, decision, err := gate.TryAcquireWithDecision(
					ctx,
					lower,
					scheduler.SlotRequest{State: "Todo"},
					now,
				)
				if err != nil {
					t.Fatalf("lower TryAcquireWithDecision() error = %v", err)
				}
				if !ok {
					t.Fatalf("lower TryAcquireWithDecision() ok = false; decision = %#v", decision)
				}
				lowerGrants++
				t.Cleanup(func() {
					if err := gate.Release(slot); err != nil && !errors.Is(err, scheduler.ErrSlotNotHeld) {
						t.Errorf("lower Release() error = %v", err)
					}
				})
			}
			gate.EndProjectCycle(lower.ID)

			if want := 5 - tt.higherDemand; lowerGrants != want {
				t.Fatalf("lower grants = %d, want %d", lowerGrants, want)
			}
		})
	}
}

func TestGlobalDispatchGateStrictProjectReservationTracksDemand(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		arrange     func(*testing.T, *scheduler.GlobalDispatchGate, scheduler.ProjectCandidate, scheduler.ProjectCandidate)
		wantGranted bool
		wantReason  string
	}{
		{
			name: "idle higher priority project stays idle after capacity release",
			arrange: func(t *testing.T, gate *scheduler.GlobalDispatchGate, higher, lower scheduler.ProjectCandidate) {
				t.Helper()
				gate.BeginProjectCycle(higher)
				gate.EndProjectCycle(higher.ID)
				slot, ok, decision, err := gate.TryAcquireWithDecision(
					t.Context(),
					lower,
					scheduler.SlotRequest{State: "Todo"},
					time.Date(2026, 7, 30, 9, 0, 0, 0, time.Local),
				)
				if err != nil {
					t.Fatalf("initial lower TryAcquireWithDecision() error = %v", err)
				}
				if !ok {
					t.Fatalf("initial lower TryAcquireWithDecision() decision = %#v, want granted", decision)
				}
				if err := gate.Release(slot); err != nil {
					t.Fatalf("initial lower Release() error = %v", err)
				}
			},
			wantGranted: true,
			wantReason:  scheduler.DispatchGateReasonGranted,
		},
		{
			name: "higher priority project release invalidates its own idle state",
			arrange: func(t *testing.T, gate *scheduler.GlobalDispatchGate, higher, _ scheduler.ProjectCandidate) {
				t.Helper()
				gate.BeginProjectCycle(higher)
				slot, ok, decision, err := gate.TryAcquireWithDecision(
					t.Context(),
					higher,
					scheduler.SlotRequest{State: "Todo"},
					time.Date(2026, 7, 30, 9, 0, 0, 0, time.Local),
				)
				if err != nil {
					t.Fatalf("higher TryAcquireWithDecision() error = %v", err)
				}
				if !ok {
					t.Fatalf("higher TryAcquireWithDecision() decision = %#v, want granted", decision)
				}
				gate.EndProjectCycle(higher.ID)
				if err := gate.Release(slot); err != nil {
					t.Fatalf("higher Release() error = %v", err)
				}
			},
			wantReason: scheduler.DispatchGateReasonReservedForHigherPriorityProject,
		},
		{
			name: "pending higher priority project reserves capacity",
			arrange: func(_ *testing.T, gate *scheduler.GlobalDispatchGate, higher, _ scheduler.ProjectCandidate) {
				gate.MarkReady(higher)
			},
			wantReason: scheduler.DispatchGateReasonReservedForHigherPriorityProject,
		},
		{
			name: "idle to ready higher priority project reclaims reservation",
			arrange: func(_ *testing.T, gate *scheduler.GlobalDispatchGate, higher, _ scheduler.ProjectCandidate) {
				gate.BeginProjectCycle(higher)
				gate.EndProjectCycle(higher.ID)
				gate.MarkReady(higher)
			},
			wantReason: scheduler.DispatchGateReasonReservedForHigherPriorityProject,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			higher := scheduler.ProjectCandidate{ID: "detent", Weight: 1, Priority: 0}
			lower := scheduler.ProjectCandidate{ID: "gopher-ai", Weight: 1, Priority: 3}
			gate := scheduler.NewGlobalDispatchGate(
				scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}),
				higher,
				lower,
			)
			tt.arrange(t, gate, higher, lower)

			gate.BeginProjectCycle(lower)
			slot, granted, decision, err := gate.TryAcquireWithDecision(
				t.Context(),
				lower,
				scheduler.SlotRequest{State: "Todo"},
				time.Date(2026, 7, 30, 9, 1, 0, 0, time.Local),
			)
			gate.EndProjectCycle(lower.ID)
			if err != nil {
				t.Fatalf("lower TryAcquireWithDecision() error = %v", err)
			}
			if granted != tt.wantGranted || decision.Reason != tt.wantReason {
				t.Fatalf(
					"lower TryAcquireWithDecision() granted = %t reason = %q, want granted = %t reason = %q; decision = %#v",
					granted,
					decision.Reason,
					tt.wantGranted,
					tt.wantReason,
					decision,
				)
			}
			if granted {
				if err := gate.Release(slot); err != nil {
					t.Fatalf("lower Release() error = %v", err)
				}
			}
		})
	}
}

func TestGlobalDispatchGateNonStrictModesDoNotReserveForProjectPriority(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		new  func(scheduler.Config) scheduler.GlobalScheduler
	}{
		{name: "weighted", new: scheduler.NewWeightedFair},
		{name: "round robin", new: scheduler.NewRoundRobin},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			higher := scheduler.ProjectCandidate{ID: "detent", Weight: 1, Priority: 0}
			lower := scheduler.ProjectCandidate{ID: "gopher-ai", Weight: 1, Priority: 3}
			gate := scheduler.NewGlobalDispatchGate(
				tt.new(scheduler.Config{Capacity: 1}),
				higher,
				lower,
			)
			gate.BeginProjectCycle(lower)
			slot, ok, decision, err := gate.TryAcquireWithDecision(
				t.Context(),
				lower,
				scheduler.SlotRequest{State: "Todo"},
				time.Date(2026, 7, 10, 14, 9, 0, 0, time.Local),
			)
			gate.EndProjectCycle(lower.ID)
			if err != nil {
				t.Fatalf("TryAcquireWithDecision() error = %v", err)
			}
			if !ok || decision.Reason != scheduler.DispatchGateReasonGranted {
				t.Fatalf("TryAcquireWithDecision() ok = %t decision = %#v, want granted", ok, decision)
			}
			if err := gate.Release(slot); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
		})
	}
}
