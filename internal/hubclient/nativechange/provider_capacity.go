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

func (e *nativeExecution) validateProviderStart(identity tracker.NativeExecutionIdentity, backendIdentity string) error {
	err := provider.ValidateStart(e.ProviderCapacity(), identity, backendIdentity, e.scheduler.providerReports, e.unavailable)
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
	return e.setPhaseProvider(ctx, requirement, 0, "")
}

func (e *nativeExecution) BindLandingAuditProvider(ctx context.Context, requirement providercapacity.Requirement, predecessor int64, backendIdentity string) error {
	return e.setPhaseProvider(ctx, requirement, predecessor, backendIdentity)
}

func (e *nativeExecution) setPhaseProvider(ctx context.Context, requirement providercapacity.Requirement, predecessor int64, backendIdentity string) error {
	if requirement.Role == runner.RoleSecurityAudit && backendIdentity == "" {
		return e.unavailable(errors.New("landing security audit requires the local backend identity"))
	}
	if e.scheduler.providerReports == nil && e.ProviderCapacity() == nil {
		if requirement.Role == runner.RoleSecurityAudit {
			return e.unavailable(errors.New("landing security audit requires a provider reservation"))
		}
		return nil
	}
	if err := e.VerifyPhaseAuthority(ctx); err != nil {
		return e.unavailable(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	identity := tracker.NativeExecutionIdentity{Role: requirement.Role, Backend: requirement.Backend, Model: requirement.Model}
	var expected providercapacity.Requirement
	if active := e.ProviderCapacity(); active != nil {
		expected = active.Requirement
		if expected == requirement && requirement.Role != runner.RoleSecurityAudit {
			return e.validateProviderStart(identity, backendIdentity)
		}
	}
	if err := e.flushObservations(ctx); err != nil {
		return e.unavailable(err)
	}
	client, lease := e.claim.source.NativeClient(), e.claim.lease
	ttl := int64(e.scheduler.host.LeaseDuration() / time.Second)
	current, err := client.SetLeaseProviderPhase(ctx, lease, ttl, tracker.NativeProviderPhaseMutation{Expected: expected, Requirement: requirement, Sequence: e.data.Sequence, AuditPredecessor: predecessor})
	if err != nil {
		return e.unavailable(e.executionError(ctx, err))
	}
	if current.ID != lease.ID || current.FencingToken != lease.FencingToken || current.ProviderReservation == nil || current.ProviderReservation.Requirement != requirement {
		return e.unavailable(errors.New("Hub returned a different phase provider reservation"))
	}
	e.phaseReservation.Store(current.ProviderReservation)
	return e.validateProviderStart(identity, backendIdentity)
}
