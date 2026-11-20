package nativechange

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/provider"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) validateProviderStart(identity tracker.NativeExecutionIdentity) error {
	err := provider.ValidateStart(e.claim.lease.ProviderReservation, identity, e.scheduler.providerReports, e.unavailable)
	if err != nil {
		e.HostOperation("native.provider_start", e.scheduler.now(), err, false)
	}
	return err
}

func (e *nativeExecution) ProviderCapacity() *providercapacity.Reservation {
	return e.claim.lease.ProviderReservation
}

func (e *nativeExecution) SetPhaseProvider(ctx context.Context, expected, requirement providercapacity.Requirement) error {
	if e.claim.lease.ProviderReservation == nil {
		return nil
	}
	if err := e.VerifyPhaseAuthority(ctx); err != nil {
		return err
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
	for {
		current, err := client.SetLeaseProviderPhase(ctx, lease, ttl, tracker.NativeProviderPhaseMutation{Expected: expected, Requirement: requirement, Sequence: sequence})
		if err == nil {
			if current.ID != lease.ID || current.FencingToken != lease.FencingToken ||
				(current.ProviderReservation == nil) != (requirement == (providercapacity.Requirement{})) ||
				current.ProviderReservation != nil && current.ProviderReservation.Requirement != requirement {
				return errors.New("Hub returned a different phase provider reservation")
			}
			if current.ProviderReservation == nil {
				return nil
			}
			identity := tracker.NativeExecutionIdentity{Role: requirement.Role, Backend: requirement.Backend, Model: requirement.Model}
			return provider.ValidateStart(current.ProviderReservation, identity, e.scheduler.providerReports, e.unavailable)
		}
		var refusal *APIError
		if !errors.As(err, &refusal) || refusal.Code != "provider_capacity" {
			return e.executionError(ctx, err)
		}
		timer := time.NewTimer(runner.JitteredFailureRetryDelay(time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}
