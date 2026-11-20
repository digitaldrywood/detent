package nativechange

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/provider"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) validateProviderStart(identity tracker.NativeExecutionIdentity) error {
	err := provider.ValidateStart(e.ProviderCapacity(), identity, e.scheduler.providerReports, e.unavailable)
	if err != nil {
		e.HostOperation("native.provider_start", e.scheduler.now(), err, false)
	}
	return err
}

func (e *nativeExecution) ProviderCapacity() *providercapacity.Reservation {
	if reservation := e.phaseReservation.Load(); reservation != nil {
		return reservation
	}
	return e.claim.lease.ProviderReservation
}

func (e *nativeExecution) SetPhaseProvider(ctx context.Context, requirement providercapacity.Requirement) error {
	if e.scheduler.providerReports == nil && e.ProviderCapacity() == nil {
		return nil
	}
	if err := e.VerifyPhaseAuthority(ctx); err != nil {
		return err
	}
	identity := tracker.NativeExecutionIdentity{Role: requirement.Role, Backend: requirement.Backend, Model: requirement.Model}
	var expected providercapacity.Requirement
	if active := e.ProviderCapacity(); active != nil {
		expected = active.Requirement
		if expected == requirement {
			return e.validateProviderStart(identity)
		}
	}
	e.mu.Lock()
	err := e.flush(ctx)
	sequence := e.data.Sequence
	e.mu.Unlock()
	if err != nil {
		return err
	}
	client, lease := e.claim.source.NativeClient(), e.claim.lease
	ttl := int64(e.scheduler.host.LeaseDuration() / time.Second)
	current, err := client.SetLeaseProviderPhase(ctx, lease, ttl, tracker.NativeProviderPhaseMutation{Expected: expected, Requirement: requirement, Sequence: sequence})
	if err != nil {
		return e.executionError(ctx, err)
	}
	if current.ID != lease.ID || current.FencingToken != lease.FencingToken || current.ProviderReservation == nil || current.ProviderReservation.Requirement != requirement {
		return errors.New("Hub returned a different phase provider reservation")
	}
	e.phaseReservation.Store(current.ProviderReservation)
	return e.validateProviderStart(identity)
}
