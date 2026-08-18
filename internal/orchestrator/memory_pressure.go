package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmemory"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) observeMemoryPressure(ctx context.Context, state *State, now time.Time) {
	if o == nil || state == nil || o.readMemoryPressure == nil || o.cfg.MemoryPressureSomeAvg60Max <= 0 {
		return
	}
	if now.IsZero() {
		now = o.clockNow()
	}
	pollInterval := o.cfg.MemoryPressurePollInterval
	if pollInterval > 0 && !state.MemoryPressure.ObservedAt.IsZero() && now.Sub(state.MemoryPressure.ObservedAt) < pollInterval {
		return
	}
	pressure := telemetry.MemoryPressure{
		SomeAvg60Max: o.cfg.MemoryPressureSomeAvg60Max,
		ObservedAt:   now.UTC(),
	}
	sample, err := o.readMemoryPressure(ctx)
	if err != nil {
		if !errors.Is(err, hostmemory.ErrUnsupported) {
			pressure.LastError = err.Error()
		}
		state.MemoryPressure = pressure
		return
	}
	if !sample.ObservedAt.IsZero() {
		pressure.ObservedAt = sample.ObservedAt.UTC()
	}
	pressure.Supported = true
	pressure.Some = pressureAverages(sample.Some)
	pressure.Full = pressureAverages(sample.Full)
	pressure.DispatchHeld = sample.Some.Avg60 > pressure.SomeAvg60Max
	state.MemoryPressure = pressure
}

func pressureAverages(pressure hostmemory.Pressure) telemetry.PressureAverages {
	return telemetry.PressureAverages{
		Avg10:  pressure.Avg10,
		Avg60:  pressure.Avg60,
		Avg300: pressure.Avg300,
		Total:  pressure.Total,
	}
}
