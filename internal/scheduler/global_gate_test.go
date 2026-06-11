package scheduler_test

import (
	"context"
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
