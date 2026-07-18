package cli

import (
	"context"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/project"
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
