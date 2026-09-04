package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hostpressure"
)

func TestRecordedBuildHostPressureHoldsAdmission(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	orch := &Orchestrator{
		cfg: Config{
			MemoryPressureSomeAvg60Max: 10,
			IOPressureFullAvg10Max:     5,
			CPUPressureSomeAvg10Max:    80,
		},
		readMemoryPressure: func(context.Context) (hostpressure.Sample, error) {
			return hostpressure.Sample{ObservedAt: now}, nil
		},
		readIOPressure: func(context.Context) (hostpressure.Sample, error) {
			return hostpressure.Sample{
				Some:       hostpressure.Pressure{Avg10: 78.81},
				Full:       hostpressure.Pressure{Avg10: 63.64},
				ObservedAt: now,
			}, nil
		},
		readCPUPressure: func(context.Context) (hostpressure.Sample, error) {
			return hostpressure.Sample{Some: hostpressure.Pressure{Avg10: 94}, ObservedAt: now}, nil
		},
		now: func() time.Time { return now },
	}
	state := newState(normalizeConfig(orch.cfg))

	outcome := orch.dispatchIssueWithAdmission(
		t.Context(),
		&state,
		connector.Issue{ID: "digitaldrywood/detent#2085"},
		0,
		now,
		"",
		false,
		nil,
	)

	if outcome.dispatched || outcome.reason != dispatchIssueFailureIOPressure {
		t.Fatalf("dispatch outcome = %#v, want IO pressure hold", outcome)
	}
	if state.MemoryPressure.DispatchHeld {
		t.Fatal("memory pressure held dispatch at zero PSI")
	}
	if !state.IOPressure.DispatchHeld || !state.CPUPressure.DispatchHeld {
		t.Fatalf("host pressure = IO %#v CPU %#v, want both held", state.IOPressure, state.CPUPressure)
	}
}

func TestObserveIOAndCPUPressureControlsAdmission(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		configure     func(*Orchestrator, float64, error)
		observed      func(State) (bool, bool, float64, string)
		value         float64
		readErr       error
		wantSupported bool
		wantHeld      bool
		wantError     string
	}{
		{
			name: "IO below threshold admits",
			configure: func(orch *Orchestrator, value float64, err error) {
				orch.cfg.IOPressureFullAvg10Max = 5
				orch.readIOPressure = pressureReader(now, hostpressure.Sample{Full: hostpressure.Pressure{Avg10: value}}, err)
			},
			observed:      observedIOPressure,
			value:         4.99,
			wantSupported: true,
		},
		{
			name: "IO at threshold admits",
			configure: func(orch *Orchestrator, value float64, err error) {
				orch.cfg.IOPressureFullAvg10Max = 5
				orch.readIOPressure = pressureReader(now, hostpressure.Sample{Full: hostpressure.Pressure{Avg10: value}}, err)
			},
			observed:      observedIOPressure,
			value:         5,
			wantSupported: true,
		},
		{
			name: "IO above threshold holds",
			configure: func(orch *Orchestrator, value float64, err error) {
				orch.cfg.IOPressureFullAvg10Max = 5
				orch.readIOPressure = pressureReader(now, hostpressure.Sample{Full: hostpressure.Pressure{Avg10: value}}, err)
			},
			observed:      observedIOPressure,
			value:         5.01,
			wantSupported: true,
			wantHeld:      true,
		},
		{
			name: "CPU above threshold holds",
			configure: func(orch *Orchestrator, value float64, err error) {
				orch.cfg.CPUPressureSomeAvg10Max = 80
				orch.readCPUPressure = pressureReader(now, hostpressure.Sample{Some: hostpressure.Pressure{Avg10: value}}, err)
			},
			observed:      observedCPUPressure,
			value:         80.01,
			wantSupported: true,
			wantHeld:      true,
		},
		{
			name: "unsupported CPU admits",
			configure: func(orch *Orchestrator, value float64, err error) {
				orch.cfg.CPUPressureSomeAvg10Max = 80
				orch.readCPUPressure = pressureReader(now, hostpressure.Sample{Some: hostpressure.Pressure{Avg10: value}}, err)
			},
			observed: observedCPUPressure,
			readErr:  hostpressure.ErrUnsupported,
		},
		{
			name: "IO read failure admits and reports error",
			configure: func(orch *Orchestrator, value float64, err error) {
				orch.cfg.IOPressureFullAvg10Max = 5
				orch.readIOPressure = pressureReader(now, hostpressure.Sample{Full: hostpressure.Pressure{Avg10: value}}, err)
			},
			observed:  observedIOPressure,
			readErr:   errors.New("pressure unavailable"),
			wantError: "pressure unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			orch := &Orchestrator{now: func() time.Time { return now }}
			tt.configure(orch, tt.value, tt.readErr)
			state := newState(normalizeConfig(orch.cfg))

			orch.observeHostPressure(t.Context(), &state, now)

			supported, held, value, lastError := tt.observed(state)
			if supported != tt.wantSupported || held != tt.wantHeld || value != tt.value || lastError != tt.wantError {
				t.Fatalf("pressure = supported %t held %t value %.2f error %q", supported, held, value, lastError)
			}
		})
	}
}

func TestDispatchRefreshesHostPressureAtAdmission(t *testing.T) {
	t.Parallel()

	tickStartedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	observedAt := tickStartedAt.Add(500 * time.Millisecond)
	admissionTime := tickStartedAt.Add(2 * time.Second)
	reads := 0
	orch := &Orchestrator{
		cfg: Config{IOPressureFullAvg10Max: 5, IOPressurePollInterval: time.Second},
		readIOPressure: func(context.Context) (hostpressure.Sample, error) {
			reads++
			return hostpressure.Sample{Full: hostpressure.Pressure{Avg10: 63.64}, ObservedAt: admissionTime}, nil
		},
		now: func() time.Time { return admissionTime },
	}
	state := newState(normalizeConfig(orch.cfg))
	state.IOPressure.ObservedAt = observedAt
	state.CPUPressure.DispatchHeld = true

	outcome := orch.dispatchIssueWithAdmission(
		t.Context(),
		&state,
		connector.Issue{ID: "digitaldrywood/detent#2085"},
		0,
		tickStartedAt,
		"",
		false,
		nil,
	)

	if reads != 1 || outcome.reason != dispatchIssueFailureIOPressure {
		t.Fatalf("reads = %d outcome = %#v, want fresh IO pressure hold", reads, outcome)
	}
}

func pressureReader(now time.Time, sample hostpressure.Sample, err error) func(context.Context) (hostpressure.Sample, error) {
	return func(context.Context) (hostpressure.Sample, error) {
		sample.ObservedAt = now
		return sample, err
	}
}

func observedIOPressure(state State) (bool, bool, float64, string) {
	return state.IOPressure.Supported, state.IOPressure.DispatchHeld, state.IOPressure.Full.Avg10, state.IOPressure.LastError
}

func observedCPUPressure(state State) (bool, bool, float64, string) {
	return state.CPUPressure.Supported, state.CPUPressure.DispatchHeld, state.CPUPressure.Some.Avg10, state.CPUPressure.LastError
}
