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
