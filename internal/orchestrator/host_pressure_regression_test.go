package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hostpressure"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/telemetry"
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

func TestRuntimePressureThresholdReloadReevaluatesCachedSample(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 20, 30, 0, 0, time.UTC)
	tests := []struct {
		name             string
		configureInitial func(*Config, *Orchestrator)
		configureReload  func(*Config)
		constrained      func(State) bool
		threshold        func(State) float64
		constraintSince  func(State) time.Time
		wantThreshold    float64
	}{
		{
			name: "memory",
			configureInitial: func(cfg *Config, orch *Orchestrator) {
				cfg.MemoryPressureSomeAvg60Max = 10
				cfg.MemoryPressurePollInterval = time.Second
				orch.readMemoryPressure = pressureReader(now, hostpressure.Sample{Some: hostpressure.Pressure{Avg60: 11}}, nil)
			},
			configureReload: func(cfg *Config) {
				cfg.MemoryPressureSomeAvg60Max = 12
				cfg.MemoryPressurePollInterval = time.Hour
			},
			constrained: func(state State) bool {
				return state.MemoryPressure.DispatchHeld
			},
			threshold: func(state State) float64 {
				return state.MemoryPressure.SomeAvg60Max
			},
			constraintSince: func(State) time.Time {
				return time.Time{}
			},
			wantThreshold: 12,
		},
		{
			name: "IO",
			configureInitial: func(cfg *Config, orch *Orchestrator) {
				cfg.IOPressureFullAvg10Max = 5
				cfg.IOPressurePollInterval = time.Second
				orch.readIOPressure = pressureReader(now, hostpressure.Sample{Full: hostpressure.Pressure{Avg10: 6}}, nil)
			},
			configureReload: func(cfg *Config) {
				cfg.IOPressureFullAvg10Max = 7
				cfg.IOPressurePollInterval = time.Hour
			},
			constrained: func(state State) bool {
				return state.IOPressure.CapacityConstrained || state.IOPressure.DispatchHeld
			},
			threshold: func(state State) float64 {
				return state.IOPressure.FullAvg10Max
			},
			constraintSince: func(state State) time.Time {
				return state.IOPressure.ConstrainedSince
			},
			wantThreshold: 7,
		},
		{
			name: "CPU",
			configureInitial: func(cfg *Config, orch *Orchestrator) {
				cfg.CPUPressureSomeAvg10Max = 80
				cfg.CPUPressurePollInterval = time.Second
				orch.readCPUPressure = pressureReader(now, hostpressure.Sample{Some: hostpressure.Pressure{Avg10: 90}}, nil)
			},
			configureReload: func(cfg *Config) {
				cfg.CPUPressureSomeAvg10Max = 100
				cfg.CPUPressurePollInterval = time.Hour
			},
			constrained: func(state State) bool {
				return state.CPUPressure.CapacityConstrained || state.CPUPressure.DispatchHeld
			},
			threshold: func(state State) float64 {
				return state.CPUPressure.SomeAvg10Max
			},
			constraintSince: func(state State) time.Time {
				return state.CPUPressure.ConstrainedSince
			},
			wantThreshold: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{PollInterval: time.Hour, MaxConcurrentAgents: 1}
			orch := Orchestrator{now: func() time.Time { return now }}
			tt.configureInitial(&cfg, &orch)
			cfg = normalizeConfig(cfg)
			orch.cfg = cfg
			orch.supervisor = newTestSupervisor(t, FakeRunner{}, cfg)
			state := newState(cfg)
			orch.observeHostPressure(t.Context(), &state, now)
			if !tt.constrained(state) {
				t.Fatalf("initial pressure = %#v, want constrained", state)
			}

			reloaded := cfg
			tt.configureReload(&reloaded)
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			orch.applyRuntimeUpdate(&state, RuntimeUpdate{Config: reloaded}, ticker)

			if tt.constrained(state) {
				t.Fatalf("reloaded pressure = %#v, want cached sample admitted", state)
			}
			if got := tt.threshold(state); got != tt.wantThreshold {
				t.Fatalf("threshold = %v, want %v", got, tt.wantThreshold)
			}
			if since := tt.constraintSince(state); !since.IsZero() {
				t.Fatalf("ConstrainedSince = %s, want zero", since)
			}
		})
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

func TestIOAndCPUPressureUseHysteresis(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		threshold float64
		floor     int
		configure func(*Orchestrator, *float64)
		observed  func(State) (bool, bool, int, time.Time, int64)
	}{
		{
			name:      "IO",
			threshold: 5,
			floor:     1,
			configure: func(orch *Orchestrator, value *float64) {
				orch.cfg.IOPressureFullAvg10Max = 5
				orch.cfg.IOPressureDegradedMaxAgents = 1
				orch.cfg.IOPressurePollInterval = time.Second
				orch.readIOPressure = func(context.Context) (hostpressure.Sample, error) {
					return hostpressure.Sample{Full: hostpressure.Pressure{Avg10: *value}}, nil
				}
			},
			observed: func(state State) (bool, bool, int, time.Time, int64) {
				pressure := state.IOPressure
				return pressure.CapacityConstrained, pressure.DispatchHeld, pressure.EffectiveMaxConcurrentAgents, pressure.ConstrainedSince, pressure.ConstrainedForMS
			},
		},
		{
			name:      "CPU",
			threshold: 80,
			floor:     2,
			configure: func(orch *Orchestrator, value *float64) {
				orch.cfg.CPUPressureSomeAvg10Max = 80
				orch.cfg.CPUPressureDegradedMaxAgents = 2
				orch.cfg.CPUPressurePollInterval = time.Second
				orch.readCPUPressure = func(context.Context) (hostpressure.Sample, error) {
					return hostpressure.Sample{Some: hostpressure.Pressure{Avg10: *value}}, nil
				}
			},
			observed: func(state State) (bool, bool, int, time.Time, int64) {
				pressure := state.CPUPressure
				return pressure.CapacityConstrained, pressure.DispatchHeld, pressure.EffectiveMaxConcurrentAgents, pressure.ConstrainedSince, pressure.ConstrainedForMS
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			value := tt.threshold + 1
			orch := &Orchestrator{now: func() time.Time { return now }}
			tt.configure(orch, &value)
			state := newState(normalizeConfig(orch.cfg))

			orch.observeHostPressure(t.Context(), &state, now)
			constrained, held, effective, since, constrainedForMS := tt.observed(state)
			if !constrained || held || effective != tt.floor || !since.Equal(now) || constrainedForMS != 0 {
				t.Fatalf("triggered pressure = constrained %t held %t effective %d since %s duration %d", constrained, held, effective, since, constrainedForMS)
			}

			values := []struct {
				name            string
				value           float64
				wantConstrained bool
				wantDurationMS  int64
			}{
				{name: "at trigger threshold stays constrained", value: tt.threshold, wantConstrained: true, wantDurationMS: 1000},
				{name: "above recovery threshold stays constrained", value: tt.threshold*pressureRecoveryRatio + 0.01, wantConstrained: true, wantDurationMS: 2000},
				{name: "at recovery threshold resumes", value: tt.threshold * pressureRecoveryRatio},
			}
			for _, sample := range values {
				now = now.Add(time.Second)
				value = sample.value
				orch.observeHostPressure(t.Context(), &state, now)
				constrained, held, effective, since, constrainedForMS = tt.observed(state)
				if constrained != sample.wantConstrained || held {
					t.Fatalf("%s: constrained %t held %t, want constrained %t held false", sample.name, constrained, held, sample.wantConstrained)
				}
				if sample.wantConstrained {
					if effective != tt.floor || !since.Equal(now.Add(-time.Duration(sample.wantDurationMS)*time.Millisecond)) || constrainedForMS != sample.wantDurationMS {
						t.Fatalf("%s: effective %d since %s duration %d", sample.name, effective, since, constrainedForMS)
					}
				} else if effective != 0 || !since.IsZero() || constrainedForMS != 0 {
					t.Fatalf("%s: cleared pressure = effective %d since %s duration %d", sample.name, effective, since, constrainedForMS)
				}
			}
		})
	}
}

func TestSustainedIOPressureUsesConfiguredAdmissionFloor(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	project := scheduler.ProjectCandidate{ID: "detent"}
	gate := scheduler.NewGlobalDispatchGate(
		scheduler.NewRoundRobin(scheduler.Config{Capacity: 4}),
		project,
	)
	orch := &Orchestrator{
		cfg: Config{
			Project:                     project,
			IOPressureFullAvg10Max:      5,
			IOPressureDegradedMaxAgents: 1,
			IOPressurePollInterval:      time.Second,
		},
		globalDispatchGate: gate,
		readIOPressure: func(context.Context) (hostpressure.Sample, error) {
			return hostpressure.Sample{Full: hostpressure.Pressure{Avg10: 63.64}}, nil
		},
		now: func() time.Time { return now },
	}
	state := newState(normalizeConfig(orch.cfg))
	orch.observeHostPressure(t.Context(), &state, now)
	constraint, constrained := activeHostPressureConstraint(&state)
	if !constrained || constraint.capacity != 1 || state.IOPressure.DispatchHeld {
		t.Fatalf("IO pressure = %#v, constraint = %#v, %t", state.IOPressure, constraint, constrained)
	}

	first, acquired, decision := orch.acquireGlobalDispatchSlot(
		t.Context(),
		connector.Issue{ID: "issue-1", State: "Todo"},
		"",
		now,
		constraint.capacity,
	)
	if !acquired {
		t.Fatalf("first pressure-limited acquisition = false: %#v", decision)
	}
	defer orch.releaseGlobalDispatchSlot(first)

	_, acquired, decision = orch.acquireGlobalDispatchSlot(
		t.Context(),
		connector.Issue{ID: "issue-2", State: "Todo"},
		"",
		now,
		constraint.capacity,
	)
	if acquired || decision.Reason != scheduler.DispatchGateReasonPressureCapacityFull || decision.PressureCapacity != 1 || decision.PressureUsed != 1 || decision.PressureAvailable != 0 {
		t.Fatalf("second pressure-limited acquisition = %t, %#v", acquired, decision)
	}
	if snapshot := gate.PoolSnapshot(); snapshot.Used != 1 {
		t.Fatalf("running work was preempted: %#v", snapshot)
	}
}

func TestActiveHostPressureConstraintUsesLowestFloor(t *testing.T) {
	t.Parallel()

	ioSince := time.Date(2026, 9, 4, 11, 55, 0, 0, time.UTC)
	cpuSince := ioSince.Add(time.Minute)
	tests := []struct {
		name       string
		state      State
		wantReason string
		wantFloor  int
		wantSince  time.Time
	}{
		{
			name: "IO only",
			state: State{IOPressure: telemetry.IOPressure{
				CapacityConstrained: true, EffectiveMaxConcurrentAgents: 2, ConstrainedSince: ioSince,
			}},
			wantReason: dispatchIssueFailureIOPressure,
			wantFloor:  2,
			wantSince:  ioSince,
		},
		{
			name: "CPU lower",
			state: State{
				IOPressure: telemetry.IOPressure{
					CapacityConstrained: true, EffectiveMaxConcurrentAgents: 2, ConstrainedSince: ioSince,
				},
				CPUPressure: telemetry.CPUPressure{
					CapacityConstrained: true, EffectiveMaxConcurrentAgents: 1, ConstrainedSince: cpuSince,
				},
			},
			wantReason: dispatchIssueFailureCPUPressure,
			wantFloor:  1,
			wantSince:  cpuSince,
		},
		{
			name: "IO hard stop wins",
			state: State{
				IOPressure: telemetry.IOPressure{
					CapacityConstrained: true, ConstrainedSince: ioSince,
				},
				CPUPressure: telemetry.CPUPressure{
					CapacityConstrained: true, EffectiveMaxConcurrentAgents: 1, ConstrainedSince: cpuSince,
				},
			},
			wantReason: dispatchIssueFailureIOPressure,
			wantSince:  ioSince,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			constraint, ok := activeHostPressureConstraint(&tt.state)
			if !ok || constraint.reason != tt.wantReason || constraint.capacity != tt.wantFloor || !constraint.constrainedAt.Equal(tt.wantSince) {
				t.Fatalf("activeHostPressureConstraint() = %#v, %t", constraint, ok)
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
