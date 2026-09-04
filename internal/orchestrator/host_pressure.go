package orchestrator

import (
	"context"
	"errors"
	"fmt"
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

const pressureRecoveryRatio = 0.8

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
	previous := state.IOPressure
	wasConstrained := previous.CapacityConstrained || previous.DispatchHeld
	observation, ok := o.observePressure(
		ctx,
		o.readIOPressure,
		now,
		o.cfg.IOPressurePollInterval,
		state.IOPressure.ObservedAt,
		func(sample hostpressure.Sample) bool {
			return pressureConstrained(wasConstrained, sample.Full.Avg10, o.cfg.IOPressureFullAvg10Max)
		},
	)
	if !ok {
		refreshIOPressureDuration(&state.IOPressure, now)
		return
	}
	constrainedSince, constrainedForMS := pressureConstraintDuration(
		wasConstrained,
		previous.ConstrainedSince,
		observation.dispatchHeld,
		now,
	)
	state.IOPressure = telemetry.IOPressure{
		Supported:                   observation.supported,
		Some:                        observation.some,
		Full:                        observation.full,
		FullAvg10Max:                o.cfg.IOPressureFullAvg10Max,
		DegradedMaxConcurrentAgents: o.cfg.IOPressureDegradedMaxAgents,
		CapacityConstrained:         observation.dispatchHeld,
		ConstrainedSince:            constrainedSince,
		ConstrainedForMS:            constrainedForMS,
		ObservedAt:                  observation.observedAt,
		LastError:                   observation.lastError,
	}
	refreshIOPressureCapacity(&state.IOPressure, o.cfg.IOPressureDegradedMaxAgents)
}

func (o *Orchestrator) observeCPUPressure(ctx context.Context, state *State, now time.Time) {
	if o == nil || state == nil || o.readCPUPressure == nil || o.cfg.CPUPressureSomeAvg10Max <= 0 {
		return
	}
	previous := state.CPUPressure
	wasConstrained := previous.CapacityConstrained || previous.DispatchHeld
	observation, ok := o.observePressure(
		ctx,
		o.readCPUPressure,
		now,
		o.cfg.CPUPressurePollInterval,
		state.CPUPressure.ObservedAt,
		func(sample hostpressure.Sample) bool {
			return pressureConstrained(wasConstrained, sample.Some.Avg10, o.cfg.CPUPressureSomeAvg10Max)
		},
	)
	if !ok {
		refreshCPUPressureDuration(&state.CPUPressure, now)
		return
	}
	constrainedSince, constrainedForMS := pressureConstraintDuration(
		wasConstrained,
		previous.ConstrainedSince,
		observation.dispatchHeld,
		now,
	)
	state.CPUPressure = telemetry.CPUPressure{
		Supported:                   observation.supported,
		Some:                        observation.some,
		Full:                        observation.full,
		SomeAvg10Max:                o.cfg.CPUPressureSomeAvg10Max,
		DegradedMaxConcurrentAgents: o.cfg.CPUPressureDegradedMaxAgents,
		CapacityConstrained:         observation.dispatchHeld,
		ConstrainedSince:            constrainedSince,
		ConstrainedForMS:            constrainedForMS,
		ObservedAt:                  observation.observedAt,
		LastError:                   observation.lastError,
	}
	refreshCPUPressureCapacity(&state.CPUPressure, o.cfg.CPUPressureDegradedMaxAgents)
}

func pressureConstrained(previous bool, value float64, threshold float64) bool {
	if previous {
		return value > threshold*pressureRecoveryRatio
	}
	return value > threshold
}

func pressureConstraintDuration(previous bool, since time.Time, constrained bool, now time.Time) (time.Time, int64) {
	if !constrained {
		return time.Time{}, 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if !previous || since.IsZero() {
		since = now
	}
	return since, max(now.Sub(since).Milliseconds(), 0)
}

func refreshIOPressureCapacity(pressure *telemetry.IOPressure, degraded int) {
	if pressure == nil {
		return
	}
	pressure.DegradedMaxConcurrentAgents = max(degraded, 0)
	pressure.EffectiveMaxConcurrentAgents = 0
	pressure.DispatchHeld = false
	if pressure.CapacityConstrained {
		pressure.EffectiveMaxConcurrentAgents = pressure.DegradedMaxConcurrentAgents
		pressure.DispatchHeld = pressure.EffectiveMaxConcurrentAgents == 0
	}
}

func refreshCPUPressureCapacity(pressure *telemetry.CPUPressure, degraded int) {
	if pressure == nil {
		return
	}
	pressure.DegradedMaxConcurrentAgents = max(degraded, 0)
	pressure.EffectiveMaxConcurrentAgents = 0
	pressure.DispatchHeld = false
	if pressure.CapacityConstrained {
		pressure.EffectiveMaxConcurrentAgents = pressure.DegradedMaxConcurrentAgents
		pressure.DispatchHeld = pressure.EffectiveMaxConcurrentAgents == 0
	}
}

func refreshIOPressureDuration(pressure *telemetry.IOPressure, now time.Time) {
	if pressure == nil || !pressure.CapacityConstrained || pressure.ConstrainedSince.IsZero() {
		return
	}
	pressure.ConstrainedForMS = max(now.UTC().Sub(pressure.ConstrainedSince).Milliseconds(), 0)
}

func refreshCPUPressureDuration(pressure *telemetry.CPUPressure, now time.Time) {
	if pressure == nil || !pressure.CapacityConstrained || pressure.ConstrainedSince.IsZero() {
		return
	}
	pressure.ConstrainedForMS = max(now.UTC().Sub(pressure.ConstrainedSince).Milliseconds(), 0)
}

type hostPressureConstraint struct {
	resource       string
	reason         string
	capacity       int
	constrainedAt  time.Time
	constrainedFor time.Duration
}

func activeHostPressureConstraint(state *State) (hostPressureConstraint, bool) {
	if state == nil {
		return hostPressureConstraint{}, false
	}
	constraints := make([]hostPressureConstraint, 0, 2)
	if pressure := state.IOPressure; pressure.CapacityConstrained {
		constraints = append(constraints, hostPressureConstraint{
			resource:       "I/O",
			reason:         dispatchIssueFailureIOPressure,
			capacity:       pressure.EffectiveMaxConcurrentAgents,
			constrainedAt:  pressure.ConstrainedSince,
			constrainedFor: time.Duration(pressure.ConstrainedForMS) * time.Millisecond,
		})
	}
	if pressure := state.CPUPressure; pressure.CapacityConstrained {
		constraints = append(constraints, hostPressureConstraint{
			resource:       "CPU",
			reason:         dispatchIssueFailureCPUPressure,
			capacity:       pressure.EffectiveMaxConcurrentAgents,
			constrainedAt:  pressure.ConstrainedSince,
			constrainedFor: time.Duration(pressure.ConstrainedForMS) * time.Millisecond,
		})
	}
	if len(constraints) == 0 {
		return hostPressureConstraint{}, false
	}
	selected := constraints[0]
	for _, constraint := range constraints[1:] {
		if constraint.capacity < selected.capacity {
			selected = constraint
		}
	}
	return selected, true
}

func hostPressureWaitReason(state *State, reason string) string {
	constraint, ok := activeHostPressureConstraint(state)
	if !ok {
		return schedulerDecisionWaitReason(reason)
	}
	if reason == dispatchIssueFailureIOPressure && state.IOPressure.CapacityConstrained {
		constraint = hostPressureConstraint{
			resource:       "I/O",
			reason:         reason,
			capacity:       state.IOPressure.EffectiveMaxConcurrentAgents,
			constrainedAt:  state.IOPressure.ConstrainedSince,
			constrainedFor: time.Duration(state.IOPressure.ConstrainedForMS) * time.Millisecond,
		}
	}
	if reason == dispatchIssueFailureCPUPressure && state.CPUPressure.CapacityConstrained {
		constraint = hostPressureConstraint{
			resource:       "CPU",
			reason:         reason,
			capacity:       state.CPUPressure.EffectiveMaxConcurrentAgents,
			constrainedAt:  state.CPUPressure.ConstrainedSince,
			constrainedFor: time.Duration(state.CPUPressure.ConstrainedForMS) * time.Millisecond,
		}
	}
	duration := constraint.constrainedFor.Round(time.Second).String()
	if constraint.capacity == 0 {
		return constraint.resource + " pressure has held dispatch for " + duration
	}
	agentLabel := "agent"
	if constraint.capacity != 1 {
		agentLabel = "agents"
	}
	return fmt.Sprintf(
		"%s pressure has limited admission to %d concurrent %s for %s",
		constraint.resource,
		constraint.capacity,
		agentLabel,
		duration,
	)
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
