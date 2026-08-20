package cli

import (
	"context"
	"errors"
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

	candidates := []scheduler.ProjectCandidate{
		{ID: "detent", Weight: 1},
		{ID: "video", Pool: "video", Weight: 1},
	}
	gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{
		{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}},
		{Name: "video", Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}},
	}, candidates)
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	if release, ok := runtimeUpdateIdleReservation(context.Background(), nil, gate); ok || release != nil {
		t.Fatalf("runtimeUpdateIdleReservation() with nil registry ok = %t release nil = %t, want false/true", ok, release == nil)
	}
	release, ok := runtimeUpdateIdleReservation(context.Background(), project.NewRegistry(), gate)
	if !ok || release == nil {
		t.Fatal("runtimeUpdateIdleReservation() did not reserve an idle runtime")
	}
	request := scheduler.SlotRequest{State: "Todo"}
	for _, candidate := range candidates {
		if _, acquired, decision, err := gate.TryAcquireWithDecision(context.Background(), candidate, request, time.Now()); err != nil {
			t.Fatalf("TryAcquireWithDecision(%s) while reserved error = %v", candidate.ID, err)
		} else if acquired || decision.Reason != scheduler.DispatchGateReasonPaused {
			t.Fatalf("TryAcquireWithDecision(%s) while reserved acquired = %t decision = %#v", candidate.ID, acquired, decision)
		}
	}

	release()
	for _, candidate := range candidates {
		slot, acquired, err := gate.TryAcquire(context.Background(), candidate, request, time.Now())
		if err != nil {
			t.Fatalf("TryAcquire(%s) after release error = %v", candidate.ID, err)
		}
		if !acquired {
			t.Fatalf("TryAcquire(%s) after release acquired = false, want true", candidate.ID)
		}
		if err := gate.Release(slot); err != nil {
			t.Fatalf("Release(%s) error = %v", candidate.ID, err)
		}
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

func TestWaitForRuntimeUpdateIdleHonorsCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		idleAfter time.Duration
		wantWait  time.Duration
		wantErr   error
	}{
		{name: "in-flight attempts finish before ceiling", idleAfter: time.Second, wantWait: time.Second},
		{name: "in-flight attempts consume full ceiling", idleAfter: 4 * time.Second, wantWait: 3 * time.Second, wantErr: ErrRuntimeUpdateDrainTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			startedAt := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
			now := startedAt
			waited := time.Duration(0)
			err := waitForRuntimeUpdateIdle(
				context.Background(),
				func(context.Context) bool { return now.Sub(startedAt) >= tt.idleAfter },
				3*time.Second,
				func() time.Time { return now },
				func(_ context.Context, delay time.Duration) bool {
					now = now.Add(delay)
					waited += delay
					return true
				},
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("waitForRuntimeUpdateIdle() error = %v, want %v", err, tt.wantErr)
			}
			if waited != tt.wantWait {
				t.Fatalf("waited = %s, want %s", waited, tt.wantWait)
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
