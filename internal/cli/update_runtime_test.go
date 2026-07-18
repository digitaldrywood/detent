package cli

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestRuntimeUpdateIdleIsConservative(t *testing.T) {
	t.Parallel()

	if runtimeUpdateIdle(context.Background(), nil) {
		t.Fatal("runtimeUpdateIdle() with nil registry = true, want false")
	}
	if !runtimeUpdateIdle(context.Background(), project.NewRegistry()) {
		t.Fatal("runtimeUpdateIdle() with empty registry = false, want true")
	}
}

func TestRuntimeUpdateIdleReservationBlocksDispatch(t *testing.T) {
	t.Parallel()

	gate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
	if release, ok := runtimeUpdateIdleReservation(context.Background(), nil, gate); ok || release != nil {
		t.Fatalf("runtimeUpdateIdleReservation() with nil registry ok = %t release nil = %t, want false/true", ok, release == nil)
	}
	release, ok := runtimeUpdateIdleReservation(context.Background(), project.NewRegistry(), gate)
	if !ok || release == nil {
		t.Fatal("runtimeUpdateIdleReservation() did not reserve an idle runtime")
	}
	candidate := scheduler.ProjectCandidate{ID: "detent", Weight: 1}
	request := scheduler.SlotRequest{State: "Todo"}
	if _, acquired, decision, err := gate.TryAcquireWithDecision(context.Background(), candidate, request, time.Now()); err != nil {
		t.Fatalf("TryAcquireWithDecision() while reserved error = %v", err)
	} else if acquired || decision.Reason != scheduler.DispatchGateReasonPaused {
		t.Fatalf("TryAcquireWithDecision() while reserved acquired = %t decision = %#v", acquired, decision)
	}

	release()
	slot, acquired, err := gate.TryAcquire(context.Background(), candidate, request, time.Now())
	if err != nil {
		t.Fatalf("TryAcquire() after release error = %v", err)
	}
	if !acquired {
		t.Fatal("TryAcquire() after release acquired = false, want true")
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestRequestUpdateRestartUsesShutdownDrainState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		prepare        func(*ShutdownController)
		wantAccepted   bool
		wantBinary     string
		wantDrainEvent bool
	}{
		{
			name:           "idle controller accepts update restart",
			wantAccepted:   true,
			wantBinary:     "/opt/detent/bin/detent",
			wantDrainEvent: true,
		},
		{
			name: "manual drain already in progress",
			prepare: func(controller *ShutdownController) {
				controller.RequestDrain()
			},
			wantDrainEvent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			controller := NewShutdownController()
			deactivate := controller.activate()
			t.Cleanup(deactivate)
			if tt.prepare != nil {
				tt.prepare(controller)
			}
			restart := NewRestartRequest()
			accepted := requestUpdateRestart(controller, restart, "/opt/detent/bin/detent")
			if accepted != tt.wantAccepted {
				t.Fatalf("requestUpdateRestart() = %t, want %t", accepted, tt.wantAccepted)
			}
			if got := restart.Binary(); got != tt.wantBinary {
				t.Fatalf("Binary() = %q, want %q", got, tt.wantBinary)
			}
			select {
			case request := <-controller.Requests():
				if !tt.wantDrainEvent || request != ShutdownRequestDrain {
					t.Fatalf("shutdown request = %v, want drain=%t", request, tt.wantDrainEvent)
				}
			default:
				if tt.wantDrainEvent {
					t.Fatal("shutdown request missing, want drain")
				}
			}
		})
	}
}

func TestRequestUpdateRestartRacesManualDrainWithoutOverwritingOwner(t *testing.T) {
	t.Parallel()

	for range 100 {
		controller := NewShutdownController()
		deactivate := controller.activate()
		restart := NewRestartRequest()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			controller.RequestDrain()
		}()
		go func() {
			defer wg.Done()
			<-start
			requestUpdateRestart(controller, restart, "/opt/detent/bin/detent")
		}()
		close(start)
		wg.Wait()
		if restart.Binary() != "" && restart.Binary() != "/opt/detent/bin/detent" {
			t.Fatalf("Binary() = %q, want empty or update binary", restart.Binary())
		}
		deactivate()
	}
}
