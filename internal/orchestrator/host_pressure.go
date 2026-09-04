package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hostpressure"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type pressureObservation struct {
	supported    bool
	some         telemetry.PressureAverages
	full         telemetry.PressureAverages
	dispatchHeld bool
	observedAt   time.Time
	lastError    string
}

func (o *Orchestrator) observeHostPressure(ctx context.Context, state *State, now time.Time) {
	if o == nil || state == nil {
		return
	}
	if now.IsZero() {
		now = o.clockNow()
	}
	o.observeMemoryPressure(ctx, state, now)
	o.observeIOPressure(ctx, state, now)
	o.observeCPUPressure(ctx, state, now)
}

func (o *Orchestrator) observeMemoryPressure(ctx context.Context, state *State, now time.Time) {
	if o == nil || state == nil || o.readMemoryPressure == nil || o.cfg.MemoryPressureSomeAvg60Max <= 0 {
		return
	}
	observation, ok := o.observePressure(
		ctx,
		o.readMemoryPressure,
		now,
		o.cfg.MemoryPressurePollInterval,
		state.MemoryPressure.ObservedAt,
		func(sample hostpressure.Sample) bool {
			return sample.Some.Avg60 > o.cfg.MemoryPressureSomeAvg60Max
		},
	)
	if !ok {
		return
	}
	state.MemoryPressure = telemetry.MemoryPressure{
		Supported:    observation.supported,
		Some:         observation.some,
		Full:         observation.full,
		SomeAvg60Max: o.cfg.MemoryPressureSomeAvg60Max,
		DispatchHeld: observation.dispatchHeld,
		ObservedAt:   observation.observedAt,
		LastError:    observation.lastError,
	}
}

func (o *Orchestrator) observeIOPressure(ctx context.Context, state *State, now time.Time) {
	if o == nil || state == nil || o.readIOPressure == nil || o.cfg.IOPressureFullAvg10Max <= 0 {
		return
	}
	observation, ok := o.observePressure(
		ctx,
		o.readIOPressure,
		now,
		o.cfg.IOPressurePollInterval,
		state.IOPressure.ObservedAt,
		func(sample hostpressure.Sample) bool {
			return sample.Full.Avg10 > o.cfg.IOPressureFullAvg10Max
		},
	)
	if !ok {
		return
	}
	state.IOPressure = telemetry.IOPressure{
		Supported:    observation.supported,
		Some:         observation.some,
		Full:         observation.full,
		FullAvg10Max: o.cfg.IOPressureFullAvg10Max,
		DispatchHeld: observation.dispatchHeld,
		ObservedAt:   observation.observedAt,
		LastError:    observation.lastError,
	}
}

func (o *Orchestrator) observeCPUPressure(ctx context.Context, state *State, now time.Time) {
	if o == nil || state == nil || o.readCPUPressure == nil || o.cfg.CPUPressureSomeAvg10Max <= 0 {
		return
	}
	observation, ok := o.observePressure(
		ctx,
		o.readCPUPressure,
		now,
		o.cfg.CPUPressurePollInterval,
		state.CPUPressure.ObservedAt,
		func(sample hostpressure.Sample) bool {
			return sample.Some.Avg10 > o.cfg.CPUPressureSomeAvg10Max
		},
	)
	if !ok {
		return
	}
	state.CPUPressure = telemetry.CPUPressure{
		Supported:    observation.supported,
		Some:         observation.some,
		Full:         observation.full,
		SomeAvg10Max: o.cfg.CPUPressureSomeAvg10Max,
		DispatchHeld: observation.dispatchHeld,
		ObservedAt:   observation.observedAt,
		LastError:    observation.lastError,
	}
}

func (o *Orchestrator) observePressure(
	ctx context.Context,
	read func(context.Context) (hostpressure.Sample, error),
	now time.Time,
	pollInterval time.Duration,
	previousObservedAt time.Time,
	holdsDispatch func(hostpressure.Sample) bool,
) (pressureObservation, bool) {
	if now.IsZero() {
		now = o.clockNow()
	}
	if pollInterval > 0 && !previousObservedAt.IsZero() && now.Sub(previousObservedAt) < pollInterval {
		return pressureObservation{}, false
	}
	observation := pressureObservation{observedAt: now.UTC()}
	sample, err := read(ctx)
	if err != nil {
		if !errors.Is(err, hostpressure.ErrUnsupported) {
			observation.lastError = err.Error()
		}
		return observation, true
	}
	if !sample.ObservedAt.IsZero() {
		observation.observedAt = sample.ObservedAt.UTC()
	}
	observation.supported = true
	observation.some = pressureAverages(sample.Some)
	observation.full = pressureAverages(sample.Full)
	observation.dispatchHeld = holdsDispatch(sample)
	return observation, true
}

func pressureAverages(pressure hostpressure.Pressure) telemetry.PressureAverages {
	return telemetry.PressureAverages{
		Avg10:  pressure.Avg10,
		Avg60:  pressure.Avg60,
		Avg300: pressure.Avg300,
		Total:  pressure.Total,
	}
}
