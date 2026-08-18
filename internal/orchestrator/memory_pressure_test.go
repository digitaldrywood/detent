package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hostmemory"
)

func TestObserveMemoryPressureControlsAdmission(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		avg60         float64
		readErr       error
		wantSupported bool
		wantHeld      bool
	}{
		{name: "below threshold admits", avg60: 9.99, wantSupported: true},
		{name: "at threshold admits", avg60: 10, wantSupported: true},
		{name: "above threshold holds", avg60: 10.01, wantSupported: true, wantHeld: true},
		{name: "unsupported platform admits", readErr: hostmemory.ErrUnsupported},
		{name: "read failure admits", readErr: errors.New("pressure unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 18, 17, 0, 0, 0, time.UTC)
			orch := &Orchestrator{
				cfg: Config{MemoryPressureSomeAvg60Max: 10, MemoryPressurePollInterval: time.Second},
				readMemoryPressure: func(context.Context) (hostmemory.Sample, error) {
					return hostmemory.Sample{Some: hostmemory.Pressure{Avg60: tt.avg60}, ObservedAt: now}, tt.readErr
				},
				now: func() time.Time { return now },
			}
			state := newState(normalizeConfig(orch.cfg))
			orch.observeMemoryPressure(t.Context(), &state, now)
			if state.MemoryPressure.Supported != tt.wantSupported || state.MemoryPressure.DispatchHeld != tt.wantHeld {
				t.Fatalf("MemoryPressure = %#v, want supported %v held %v", state.MemoryPressure, tt.wantSupported, tt.wantHeld)
			}
			if tt.wantSupported && state.MemoryPressure.Some.Avg60 != tt.avg60 {
				t.Fatalf("Some.Avg60 = %v, want %v", state.MemoryPressure.Some.Avg60, tt.avg60)
			}
		})
	}
}

func TestObserveMemoryPressureResumesAfterPressureFalls(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 17, 0, 0, 0, time.UTC)
	avg60 := 11.0
	orch := &Orchestrator{
		cfg: Config{MemoryPressureSomeAvg60Max: 10, MemoryPressurePollInterval: time.Second},
		readMemoryPressure: func(context.Context) (hostmemory.Sample, error) {
			return hostmemory.Sample{Some: hostmemory.Pressure{Avg60: avg60}}, nil
		},
		now: func() time.Time { return now },
	}
	state := newState(normalizeConfig(orch.cfg))
	orch.observeMemoryPressure(t.Context(), &state, now)
	if !state.MemoryPressure.DispatchHeld {
		t.Fatal("DispatchHeld = false above threshold")
	}
	outcome := orch.dispatchIssueWithAdmission(
		t.Context(),
		&state,
		connector.Issue{ID: "issue-1899"},
		0,
		now,
		"",
		false,
		nil,
	)
	if outcome.dispatched || outcome.reason != dispatchIssueFailureMemoryPressure {
		t.Fatalf("dispatch outcome = %#v, want memory pressure hold", outcome)
	}
	avg60 = 9
	now = now.Add(time.Second)
	orch.observeMemoryPressure(t.Context(), &state, now)
	if state.MemoryPressure.DispatchHeld {
		t.Fatal("DispatchHeld = true after pressure fell below threshold")
	}
}
