package hubclient

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeClaim struct {
	execution            *nativeExecution
	availabilityDeadline time.Time
	source               *NativeConnector
	lease                tracker.NativeLease
	recovery             tracker.NativeRecovery
	deadline             time.Time
}

func (s *Scheduler) ConnectorForProject(project string) (connector.Connector, bool) {
	source, ok := s.nativeProjects[project]
	if !ok {
		return nil, false
	}
	return source, true
}

func (s *Scheduler) ObserveNativeAdmission(project string, current tracker.NativeAdmissionContext) {
	if s.client.runner == nil || s.nativeProjects[project] == nil {
		return
	}
	s.client.runner.routingMu.Lock()
	var revision int64
	if s.client.runner.routing != nil {
		revision = s.client.runner.routing.Revision
	}
	s.client.runner.routingMu.Unlock()
	if revision < 1 {
		return
	}
	current.WorkflowStates = slices.Clone(current.WorkflowStates)
	current.Authors = slices.Clone(current.Authors)
	current.Assignees = slices.Clone(current.Assignees)
	current.LabelInclude = slices.Clone(current.LabelInclude)
	current.LabelExclude = slices.Clone(current.LabelExclude)
	s.mu.Lock()
	s.nativeAdmissions[project] = tracker.NativeAdmissionObservation{Context: current, RunnerRevision: revision}
	s.mu.Unlock()
}

func (s *Scheduler) Heartbeat(ctx context.Context) error {
	if s.client.runner == nil {
		return nil
	}
	var result error
	s.mu.Lock()
	projectOwner := s.projectConfiguration
	s.mu.Unlock()
	for _, source := range s.nativeProjects {
		result = errors.Join(result, s.heartbeatNativeMachine(ctx, source, projectOwner))
	}
	return result
}

func (s *Scheduler) ensureNativeMachine(ctx context.Context, source *NativeConnector) error {
	return s.heartbeatNativeMachine(ctx, source, nil)
}

func (s *Scheduler) heartbeatNativeMachine(ctx context.Context, source *NativeConnector, projectOwner func(context.Context, string, *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration) error {
	project := source.client.project
	s.mu.Lock()
	last := s.nativeHeartbeats[project]
	owner := s.updateOwner
	s.mu.Unlock()
	if !last.IsZero() && s.now().Before(last.Add(s.heartbeatInterval)) {
		return nil
	}
	var update *runnerauth.UpdateObservation
	var updateSupported bool
	if owner != nil && s.client.runner != nil {
		supported, err := source.client.HubFeature(ctx, tracker.NativeRunnerUpdateCapability)
		if err != nil {
			return err
		}
		updateSupported = supported
		if supported {
			update = owner(ctx, nil)
		}
	}
	var projectConfig *runnerauth.ProjectConfiguration
	if projectOwner != nil {
		supported, err := source.client.HubFeature(ctx, tracker.NativeProjectConfigurationCapability)
		if err != nil {
			return err
		}
		if supported {
			view := projectOwner(ctx, string(project), nil)
			projectConfig = &view
		}
	}
	var capacityConfig *runnerauth.CapacityConfig
	var capacitySupported bool
	if s.capacityConfiguration != nil && s.client.runner != nil {
		supported, err := source.client.HubFeature(ctx, tracker.NativeRunnerCapacityCapability)
		if err != nil {
			return err
		}
		capacitySupported = supported
		if supported {
			capacityConfig = s.capacityConfiguration(ctx, nil)
		}
	}
	var report isolation.Report
	var problems []runnerauth.Problem
	if s.problems != nil {
		problems = s.problems()
	}
	if s.isolationReport != nil {
		report = s.probeIsolation(ctx)
	}
	if last.IsZero() {
		var required []string
		if s.providerReports != nil {
			required = append(required, tracker.NativeProviderCapacityCapability)
		}
		if err := source.client.Negotiate(ctx, required...); err != nil {
			return err
		}
	}
	var reports []providercapacity.Report
	if s.providerReports != nil {
		var err error
		reports, err = s.providerReports()
		if err != nil {
			return errors.Join(orchestrator.ErrSchedulingUnavailable, err)
		}
		if err := providercapacity.Validate(reports); err != nil {
			return errors.Join(orchestrator.ErrSchedulingUnavailable, err)
		}
	}
	var localChecks *runnerauth.LocalChecks
	var repository *string
	var admission *tracker.NativeAdmissionObservation
	for name, candidate := range s.nativeProjects {
		if candidate != source {
			continue
		}
		s.mu.Lock()
		if current, ok := s.nativeAdmissions[name]; ok {
			admission = &current
		}
		s.mu.Unlock()
		s.mu.Lock()
		checks, ok := s.localChecks[name]
		s.mu.Unlock()
		if ok && s.client.runner != nil {
			localChecks = &checks
		}
		if s.checkoutRepository != nil {
			supported, err := source.client.HubFeature(ctx, tracker.NativeCheckoutRepositoryCapability)
			if err != nil {
				return err
			}
			if supported {
				checkout := s.checkoutRepository(name)
				repository = &checkout
			}
		}
		break
	}
	s.mu.Lock()
	if current := s.nativeHeartbeats[project]; !current.IsZero() && s.now().Before(current.Add(s.heartbeatInterval)) {
		s.mu.Unlock()
		return nil
	}
	s.machine.Update = update
	s.machine.CapacityConfig = capacityConfig
	if capacityConfig != nil {
		s.machine.Capacity = min(capacityConfig.RuntimeLimit, capacityConfig.ClientLimit, capacityConfig.LocalLimit)
	}
	if s.providerReports != nil {
		s.machine.ProviderReports = reports
	}
	if s.isolationReport != nil {
		s.machine.BackendIsolation = report
	}
	s.machine.Problems = problems
	machine := s.machine
	s.mu.Unlock()
	machine.ProjectConfiguration = projectConfig
	machine.LocalChecks = localChecks
	machine.Admission = admission
	machine.CheckoutRepository = repository
	if s.client.runner != nil && !last.IsZero() {
		if err := source.client.HeartbeatMachine(ctx, machine); err != nil {
			return err
		}
	} else {
		if err := source.client.RegisterMachine(ctx, machine); err != nil {
			return err
		}
	}
	if projectConfig != nil {
		if request := s.client.runner.projectConfigurationRequest(); request != nil && request.ProjectID == string(project) && request.RequestID != projectConfig.RequestID {
			view := projectOwner(ctx, string(project), request)
			machine.ProjectConfiguration = &view
			if err := source.client.HeartbeatMachine(ctx, machine); err != nil {
				return err
			}
		}
	}
	if updateSupported {
		if request := s.client.runner.updateRequest(); request != nil && (machine.Update == nil || machine.Update.Receipt == nil || machine.Update.Receipt.Request != *request) {
			if applied := owner(ctx, request); applied != nil {
				s.mu.Lock()
				s.machine.Update = applied
				s.mu.Unlock()
				machine.Update = applied
				if err := source.client.HeartbeatMachine(ctx, machine); err != nil {
					return err
				}
			}
		}
	}
	var appliedCapacity *runnerauth.CapacityConfig
	if capacitySupported {
		if request := s.client.runner.capacityRequest(); request != nil {
			appliedCapacity = s.capacityConfiguration(ctx, request)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if appliedCapacity != nil {
		s.machine.CapacityConfig = appliedCapacity
		s.machine.Capacity = min(appliedCapacity.RuntimeLimit, appliedCapacity.ClientLimit, appliedCapacity.LocalLimit)
	}
	s.nativeHeartbeats[project] = s.now()
	return nil
}

func (s *Scheduler) probeIsolation(ctx context.Context) isolation.Report {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if s.leaseHold != nil {
		release, err := s.leaseHold(probeCtx)
		if err != nil {
			slog.Default().Warn("sprite isolation probe hold unavailable", "error", err)
		} else {
			defer release()
		}
	}
	return s.isolationReport(probeCtx)
}

func (s *Scheduler) fetchNativeCandidate(ctx context.Context, request orchestrator.SchedulingRequest, source *NativeConnector) ([]connector.Issue, error) {
	if err := s.PrepareProject(ctx, request.ProjectID); err != nil {
		return nil, errors.Join(orchestrator.ErrSchedulingUnavailable, err)
	}
	if len(request.DispatchPriorityByState) != 0 || len(request.DispatchPriorityByLabel) != 0 || request.PrioritizeUnblockers {
		if err := source.client.Negotiate(ctx, tracker.NativeDispatchPriorityCapability); err != nil {
			return nil, schedulingError(err)
		}
	}
	if err := s.ensureNativeMachine(ctx, source); err != nil {
		return nil, schedulingError(err)
	}
	if s.client.runner != nil {
		s.client.runner.routingMu.Lock()
		draining := s.client.runner.routing != nil && s.client.runner.routing.Routing.State == "draining"
		s.client.runner.routingMu.Unlock()
		if draining {
			return nil, nil
		}
	}
	session, err := s.sessionID()
	if err != nil {
		return nil, err
	}
	claimStarted := s.now()
	var availabilityDeadline time.Time
	if s.client.runner != nil {
		availability, err := s.client.runner.availability()
		if err != nil {
			return nil, err
		}
		availabilityDeadline, err = availability.Deadline(claimStarted)
		if err != nil {
			return nil, err
		}
	}
	claimRequest := tracker.NativeClaim{
		DispatchPriorityByState: request.DispatchPriorityByState,
		DispatchPriorityByLabel: request.DispatchPriorityByLabel,
		PrioritizeUnblockers:    request.PrioritizeUnblockers,
		PolicyID:                request.Policy.ID,
		MachineID:               s.machine.ID, SessionID: session, TTLSeconds: int64(s.leaseTTL / time.Second), ProtocolMajor: 2,
		Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: request.WorkflowStates,
		Authors: request.Filter.Authors, Assignees: request.Filter.Assignees, LabelInclude: request.Filter.LabelInclude, LabelExclude: request.Filter.LabelExclude,
	}
	s.mu.Lock()
	machineCapacity := s.machine.Capacity
	limit := min(max(1, request.AdmissionLimit), machineCapacity)
	s.mu.Unlock()
	if request.NativeLandingBatch && machineCapacity > 0 {
		limit = max(limit, request.CandidateLimit)
	}
	if request.CandidateLimit > 0 {
		limit = min(limit, request.CandidateLimit)
	}
	var leases []tracker.NativeLease
	if s.providerReports != nil || request.CandidateReady != nil || request.NativeLandingBatch {
		leases, err = s.claimPreviewCandidates(ctx, request, source, claimRequest, limit)
	} else {
		for len(leases) < limit {
			if len(leases) > 0 {
				claimRequest.SessionID, err = s.sessionID()
				if err != nil {
					break
				}
			}
			var lease tracker.NativeLease
			lease, err = source.client.Claim(ctx, claimRequest)
			if err != nil {
				break
			}
			leases = append(leases, lease)
		}
	}
	if errors.Is(err, ErrNoClaimableWork) || nativeAdmissionCapacityFull(err) {
		err = nil
	}
	release := func(cause error) error {
		for _, lease := range leases {
			cause = errors.Join(cause, source.client.Release(context.WithoutCancel(ctx), lease, "work_item_hydration_failed"))
		}
		return schedulingError(cause)
	}
	if err != nil {
		return nil, release(err)
	}
	issues := make([]connector.Issue, 0, len(leases))
	claims := make([]nativeClaim, 0, len(leases))
	for _, lease := range leases {
		recovery, err := source.client.Recovery(ctx, lease.WorkItemID)
		if err == nil && recovery.Issue.LinkedSource != nil && recovery.Issue.LinkedSource.Status != "complete" {
			err = s.intakeNativeSource(ctx, source, lease, recovery.Issue)
			if err == nil {
				recovery, err = source.client.Recovery(ctx, lease.WorkItemID)
			}
		}
		if err != nil {
			return nil, release(err)
		}
		issue := issueFromNative(recovery.Issue)
		issue.AssignedToWorker = true
		issue.IsolationPolicy = lease.IsolationPolicy
		issues = append(issues, issue)
		claims = append(claims, nativeClaim{availabilityDeadline: availabilityDeadline, source: source, lease: lease, recovery: recovery, deadline: nativeLeaseDeadline(claimStarted, lease)})
	}
	s.mu.Lock()
	for _, claim := range claims {
		issueID := string(claim.recovery.Issue.WorkItemID)
		s.claims[issueID] = nativeTrackerLease(claim.lease)
		s.nativeClaims[issueID] = claim
		s.claimPolicies[issueID] = claimPolicy{project: request.ProjectID, repository: request.Repository, descriptor: request.Policy}
	}
	s.mu.Unlock()
	s.syncLeaseHold(ctx)
	return issues, nil
}

func nativeAdmissionCapacityFull(err error) bool {
	var failure *APIError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "provider_capacity", "runner_capacity", "host_capacity":
		return true
	default:
		return false
	}
}

func (s *Scheduler) renewNativeClaim(ctx context.Context, issueID string, claim nativeClaim) (orchestrator.Claimed, error) {
	if err := s.checkClaimPolicy(ctx, issueID, claim.lease.PolicyID); err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(issueID, claim.lease.FencingToken, err)
	}
	if err := s.ensureNativeMachine(ctx, claim.source); err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(issueID, claim.lease.FencingToken, err)
	}
	renewStarted := s.now()
	lease, err := claim.source.client.Renew(ctx, claim.lease, int64(s.leaseTTL/time.Second))
	if err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(issueID, claim.lease.FencingToken, err)
	}
	claim.lease = lease
	claim.deadline = nativeLeaseDeadline(renewStarted, lease)
	s.mu.Lock()
	if current, ok := s.nativeClaims[issueID]; !ok || current.lease.FencingToken != lease.FencingToken {
		s.mu.Unlock()
		return orchestrator.Claimed{}, orchestrator.ErrSchedulingClaimLost
	}
	claim.execution = s.nativeClaims[issueID].execution
	s.nativeClaims[issueID] = claim
	s.claims[issueID] = nativeTrackerLease(lease)
	s.mu.Unlock()
	return claimedIssue(connector.Issue{ID: issueID}, nativeTrackerLease(lease)), nil
}

func nativeLeaseDeadline(started time.Time, lease tracker.NativeLease) time.Time {
	if lease.ServerTime.IsZero() {
		return started
	}
	return started.Add(lease.ExpiresAt.Sub(lease.ServerTime))
}

func (s *Scheduler) nativeClaimError(issueID string, token tracker.FencingToken, err error) error {
	var apiErr *APIError
	lostAuthority := errors.As(err, &apiErr) && apiErr != nil &&
		(apiErr.Code == "policy_mismatch" || apiErr.Code == "selector_no_match")
	if nativeAuthorityLost(err) || lostAuthority {
		s.mu.Lock()
		if current, ok := s.nativeClaims[issueID]; ok && current.lease.FencingToken == token {
			delete(s.claims, issueID)
			delete(s.nativeClaims, issueID)
			delete(s.claimPolicies, issueID)
		}
		s.mu.Unlock()
		return errors.Join(orchestrator.ErrSchedulingClaimLost, err)
	}
	return err
}

func nativeTrackerLease(lease tracker.NativeLease) tracker.Lease {
	return tracker.Lease{LeaseSummary: tracker.LeaseSummary{PolicyID: lease.PolicyID, ID: lease.ID, FencingToken: lease.FencingToken,
		Machine: tracker.MachineSummary{ID: lease.MachineID}, SessionID: lease.SessionID, AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt}}
}

// NativeClient returns the hub client the scheduler built for a configured
// native project. The workspace lane claims through the same client the issue
// lane does, so the two share one connection pool and one registered machine
// rather than the runner opening a second identity for the same hub.
func (s *Scheduler) NativeClient(project string) (*NativeClient, bool) {
	if s == nil {
		return nil, false
	}
	source, ok := s.nativeProjects[project]
	if !ok || source == nil || source.client == nil {
		return nil, false
	}
	return source.client, true
}

// MachineID is the machine identity this scheduler registers and claims under.
// The workspace lane claims under the same one: decisions section 18.1 keys a
// workspace's owner tuple on the machine, and a retained worktree may only be
// served by the machine that produced it.
func (s *Scheduler) MachineID() tracker.MachineID {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.machine.ID
}

func nativeTransportUnavailable(err error) bool {
	if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= http.StatusInternalServerError
	}
	return errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

func nativeAuthorityLost(err error) bool {
	var apiErr *APIError
	return nativeLeaseLost(err) || errors.As(err, &apiErr) && apiErr != nil &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound)
}
