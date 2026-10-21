package runner

import (
	"context"
	"testing"
	"time"
)

func TestStartupDispatchPacerRampsInitialStarts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		config     StartupDispatchPacerConfig
		waits      int
		wantDelays []time.Duration
	}{
		{
			name: "spaces the configured startup width",
			config: StartupDispatchPacerConfig{
				MaxStartsPerSecond: 2,
				Jitter:             100 * time.Millisecond,
				RampStarts:         4,
			},
			waits:      6,
			wantDelays: []time.Duration{525 * time.Millisecond, 525 * time.Millisecond, 525 * time.Millisecond},
		},
		{
			name: "single startup slot needs no delay",
			config: StartupDispatchPacerConfig{
				MaxStartsPerSecond: 1,
				Jitter:             time.Second,
				RampStarts:         1,
			},
			waits: 3,
		},
		{
			name: "rate alone spaces starts",
			config: StartupDispatchPacerConfig{
				MaxStartsPerSecond: 4,
				RampStarts:         3,
			},
			waits:      3,
			wantDelays: []time.Duration{250 * time.Millisecond, 250 * time.Millisecond},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var delays []time.Duration
			tt.config.Sleep = func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			}
			tt.config.RandomJitter = func(time.Duration) time.Duration { return 25 * time.Millisecond }
			pacer := NewStartupDispatchPacer(tt.config)
			for range tt.waits {
				if err := pacer.Wait(t.Context()); err != nil {
					t.Fatalf("Wait() error = %v", err)
				}
			}
			if len(delays) != len(tt.wantDelays) {
				t.Fatalf("delays = %v, want %v", delays, tt.wantDelays)
			}
			for index := range delays {
				if delays[index] != tt.wantDelays[index] {
					t.Fatalf("delays = %v, want %v", delays, tt.wantDelays)
				}
			}
		})
	}
}

func TestStartupDispatchPacerTracksHostScopedStartupAndActiveCounts(t *testing.T) {
	t.Parallel()

	pacer := NewStartupDispatchPacer(StartupDispatchPacerConfig{})
	first := pacer.BeginStartup("worker-a")
	second := pacer.BeginStartup("worker-a")
	other := pacer.BeginStartup("worker-b")

	if got := second.Snapshot(); got.concurrentStartups != 2 || got.activeWorkers != 2 {
		t.Fatalf("worker-a snapshot = %#v, want 2 startups and 2 active workers", got)
	}
	if got := other.Snapshot(); got.concurrentStartups != 1 || got.activeWorkers != 1 {
		t.Fatalf("worker-b snapshot = %#v, want host-isolated counts", got)
	}
	first.Ready()
	if got := second.Snapshot(); got.concurrentStartups != 1 || got.activeWorkers != 2 {
		t.Fatalf("worker-a ready snapshot = %#v, want 1 startup and 2 active workers", got)
	}
	first.Finish()
	second.Finish()
	other.Finish()
	if len(pacer.startingByHost) != 0 || len(pacer.activeByHost) != 0 {
		t.Fatalf("finished census = startups %#v active %#v, want empty", pacer.startingByHost, pacer.activeByHost)
	}
}

func TestStartupDispatchPacerCensusDoesNotWaitForPacingSleep(t *testing.T) {
	t.Parallel()

	sleeping := make(chan struct{})
	release := make(chan struct{})
	pacer := NewStartupDispatchPacer(StartupDispatchPacerConfig{
		MaxStartsPerSecond: 1,
		RampStarts:         2,
		Sleep: func(context.Context, time.Duration) error {
			close(sleeping)
			<-release
			return nil
		},
	})
	if err := pacer.Wait(t.Context()); err != nil {
		t.Fatalf("first Wait() error = %v", err)
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- pacer.Wait(t.Context())
	}()
	<-sleeping

	observation := pacer.BeginStartup("worker-a")
	snapshotDone := make(chan startupHostSnapshot, 1)
	go func() {
		snapshotDone <- observation.Snapshot()
	}()
	select {
	case got := <-snapshotDone:
		if got.concurrentStartups != 1 || got.activeWorkers != 1 {
			t.Fatalf("snapshot = %#v, want 1 startup and 1 active worker", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Snapshot() waited for pacing sleep")
	}

	close(release)
	if err := <-waitDone; err != nil {
		t.Fatalf("second Wait() error = %v", err)
	}
	observation.Finish()
}

func TestSupervisorPacesNormalDispatchBeforeRunningWorker(t *testing.T) {
	t.Parallel()

	var order []string
	supervisor, err := NewSupervisor(orderRecordingBackend{order: &order}, SupervisorConfig{
		DispatchPacer: orderRecordingPacer{order: &order},
	})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	completion := supervisor.Run(t.Context(), RunRequest{})
	if completion.Err != nil {
		t.Fatalf("Run() error = %v", completion.Err)
	}
	if len(order) != 2 || order[0] != "pace" || order[1] != "run" {
		t.Fatalf("order = %v, want [pace run]", order)
	}
}

type orderRecordingPacer struct {
	order *[]string
}

func (p orderRecordingPacer) Wait(context.Context) error {
	*p.order = append(*p.order, "pace")
	return nil
}

type orderRecordingBackend struct {
	order *[]string
}

func (b orderRecordingBackend) Run(context.Context, RunRequest) (RunResult, error) {
	*b.order = append(*b.order, "run")
	return RunResult{}, nil
}
