package nativechange

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/provider"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
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
	for delay := time.Second; ; delay = min(30*time.Second, 2*delay) {
		err := e.setPhaseProvider(ctx, requirement)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		var refusal *APIError
		if requirement.Role != runner.RoleSecurityAudit || !errors.As(err, &refusal) || refusal.Code != "provider_capacity" {
			return e.unavailable(err)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))):
		}
	}
}

func (e *nativeExecution) setPhaseProvider(ctx context.Context, requirement providercapacity.Requirement) error {
	if err := e.VerifyPhaseAuthority(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	identity := tracker.NativeExecutionIdentity{Role: requirement.Role, Backend: requirement.Backend, Model: requirement.Model}
	var expected providercapacity.Requirement
	if active := e.ProviderCapacity(); active != nil {
		expected = active.Requirement
		if expected == requirement {
			return e.validateProviderStart(identity)
		}
	}
	if err := e.flushObservations(ctx); err != nil {
		return err
	}
	client, lease := e.claim.source.NativeClient(), e.claim.lease
	ttl := int64(e.scheduler.host.LeaseDuration() / time.Second)
	current, err := client.SetLeaseProviderPhase(ctx, lease, ttl, tracker.NativeProviderPhaseMutation{Expected: expected, Requirement: requirement, Sequence: e.data.Sequence})
	if err != nil {
		return e.executionError(ctx, err)
	}
	if current.ID != lease.ID || current.FencingToken != lease.FencingToken || current.ProviderReservation == nil || current.ProviderReservation.Requirement != requirement {
		return errors.New("Hub returned a different phase provider reservation")
	}
	e.phaseReservation.Store(current.ProviderReservation)
	return e.validateProviderStart(identity)
}
