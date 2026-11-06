package dispatch

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) Now() time.Time               { return s.now() }
func (s *Scheduler) LeaseDuration() time.Duration { return s.leaseTTL }
func (s *Scheduler) Transport() *api.Client       { return s.client }
func (s *Scheduler) ExecutionSource(project string) *api.NativeConnector {
	return s.nativeProject(project)
}
func (s *Scheduler) ExecutionClaim(item string) (ExecutionClaim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.nativeClaims[item]
	return ExecutionClaim{Lost: c.lost, Source: c.source, Lease: c.lease, Recovery: c.recovery, LandingCapacity: c.landingCapacity, Deadline: c.deadline, Execution: c.execution}, ok
}
func (s *Scheduler) ValidateExecutionPolicy(ctx context.Context, item, policyID string) error {
	return s.checkClaimPolicy(ctx, item, policyID)
}
func (s *Scheduler) PinnedExecutionPolicy(item string) policy.Descriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimPolicies[item].descriptor
}
func (s *Scheduler) ExecutionError(ctx context.Context, item string, token tracker.FencingToken, err error) error {
	return s.nativeClaimError(ctx, item, token, err)
}
func (s *Scheduler) CleanupExecution(ctx context.Context, item string, token tracker.FencingToken) {
	s.cleanupNativeClaim(ctx, item, token)
}
func (s *Scheduler) RenewExecution(ctx context.Context, lease tracker.NativeLease, capacity int) error {
	s.mu.Lock()
	claim, ok := s.nativeClaims[string(lease.WorkItemID)]
	s.mu.Unlock()
	if !ok || claim.lease.FencingToken != lease.FencingToken {
		return runner.ErrExecutionAuthorityUnavailable
	}
	_, err := s.renewNativeClaim(ctx, string(lease.WorkItemID), claim, capacity)
	return err
}
func (s *Scheduler) RemainingExecution(lease tracker.NativeLease) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.nativeClaims[string(lease.WorkItemID)]
	if !ok || claim.lease.FencingToken != lease.FencingToken {
		return 0
	}
	return claim.deadline.Sub(s.now()) - min(5*time.Second, s.leaseTTL/10)
}
func (s *Scheduler) ExecutionAvailability(lease tracker.NativeLease) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.nativeClaims[string(lease.WorkItemID)]
	if !ok || claim.lease.FencingToken != lease.FencingToken {
		return time.Time{}
	}
	return claim.availabilityDeadline
}
func (s *Scheduler) SetExecutionAvailability(lease tracker.NativeLease, deadline time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.nativeClaims[string(lease.WorkItemID)]
	if ok && claim.lease.FencingToken == lease.FencingToken {
		claim.availabilityDeadline = deadline
		s.nativeClaims[string(lease.WorkItemID)] = claim
	}
}
func (s *Scheduler) ProviderObservations() func() ([]providercapacity.Report, error) {
	return s.providerReports
}
func (s *Scheduler) RegisterExecution(ctx context.Context, request orchestrator.SchedulingRequest, issue connector.Issue, view ExecutionClaim, execution Execution) error {
	s.mu.Lock()
	if _, ok := s.nativeClaims[issue.ID]; ok {
		s.mu.Unlock()
		return runner.ErrExecutionAuthorityUnavailable
	}
	s.nativeClaims[issue.ID] = nativeClaim{source: view.Source, lease: view.Lease, recovery: view.Recovery, deadline: view.Deadline, execution: execution}
	s.claims[issue.ID] = nativeTrackerLease(view.Lease)
	s.claimPolicies[issue.ID] = claimPolicy{project: request.ProjectID, repository: request.Repository, descriptor: request.Policy}
	s.mu.Unlock()
	s.client.RememberLease(view.Source.NativeClient().Scope(), tracker.NativeWorkItemID(issue.ID), view.Lease)
	s.syncLeaseHold(ctx)
	return nil
}

func (s *Scheduler) ActiveExecutions() []ExecutionClaim {
	s.mu.Lock()
	defer s.mu.Unlock()
	claims := make([]ExecutionClaim, 0, len(s.nativeClaims))
	for _, c := range s.nativeClaims {
		claims = append(claims, ExecutionClaim{Lost: c.lost, Source: c.source, Lease: c.lease, Recovery: c.recovery, LandingCapacity: c.landingCapacity, Deadline: c.deadline, Execution: c.execution})
	}
	return claims
}
