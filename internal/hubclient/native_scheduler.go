package hubclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeClaim struct {
	lifecycle            *sync.Mutex
	lost                 bool
	landingCapacity      int
	execution            *nativeExecution
	availabilityDeadline time.Time
	source               *NativeConnector
	lease                tracker.NativeLease
	recovery             tracker.NativeRecovery
	deadline             time.Time
}

func (s *Scheduler) ConnectorForProject(project string) (connector.Connector, bool) {
	source := s.nativeProject(project)
	if source == nil {
		return nil, false
	}
	return source, true
}

func (s *Scheduler) ObserveNativeAdmission(project string, current tracker.NativeAdmissionContext) {
	if s.client.runner == nil || s.nativeProject(project) == nil {
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
	if s.controlPaused.Load() {
		return nil
	}
	if s.client.runner == nil {
		return nil
	}
	var result error
	if s.refreshProjects != nil {
		result = s.refreshProjects(ctx, s)
	}
	s.mu.Lock()
	projectOwner := s.projectConfiguration
	s.mu.Unlock()
	for _, source := range s.nativeProjectSnapshot() {
		result = errors.Join(result, s.heartbeatNativeMachine(ctx, source, projectOwner))
	}
	return result
}

func (s *Scheduler) ensureNativeMachine(ctx context.Context, source *NativeConnector) error {
	return s.heartbeatNativeMachine(ctx, source, nil)
}

func (s *Scheduler) heartbeatNativeMachine(ctx context.Context, source *NativeConnector, projectOwner func(context.Context, string, *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration) error {
	if s.controlPaused.Load() {
		return nil
	}
	project := source.client.project
	s.mu.Lock()
	if s.nativeHeartbeatMu == nil {
		s.nativeHeartbeatMu = make(map[tracker.ProjectID]*sync.Mutex)
	}
	heartbeat := s.nativeHeartbeatMu[project]
	if heartbeat == nil {
		heartbeat = &sync.Mutex{}
		s.nativeHeartbeatMu[project] = heartbeat
	}
	now := s.now()
	previousConfiguration := s.configHeartbeats[project]
	s.mu.Unlock()
	var ownerView *runnerauth.ProjectConfiguration
	if projectOwner != nil && (previousConfiguration.IsZero() || !now.Before(previousConfiguration.Add(s.heartbeatInterval))) {
		supported, err := source.client.HubFeature(ctx, tracker.NativeProjectConfigurationCapability)
		if err != nil {
			return err
		}
		if supported {
			view := projectOwner(ctx, string(project), nil)
			ownerView = &view
		}
	}
	heartbeat.Lock()
	locked := true
	defer func() {
		if locked {
			heartbeat.Unlock()
		}
	}()
	s.mu.Lock()
	last := s.nativeHeartbeats[project]
	owner := s.updateOwner
	s.mu.Unlock()
	if ownerView == nil && !last.IsZero() && now.Before(last.Add(s.heartbeatInterval)) {
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
	if ownerView != nil {
		view := *ownerView
		if err := s.enrichProjectDiagnostics(ctx, source, &view, update); err != nil {
			return err
		}
		projectConfig = &view
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
		var isolationProblems []runnerauth.Problem
		report, isolationProblems = s.probeIsolation(ctx)
		problems = append(problems, isolationProblems...)
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
	for name, candidate := range s.nativeProjectSnapshot() {
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
	if s.availableCapacity != nil {
		machine.Capacity = max(0, min(machine.Capacity, s.availableCapacity(ctx, machine.Capacity)))
	}
	machine.ProjectConfiguration = projectConfig
	machine.LocalChecks = localChecks
	machine.Admission = admission
	machine.CheckoutRepository = repository
	if s.skills != nil {
		supported, err := source.client.HubFeature(ctx, tracker.NativeRunnerSkillsCapability)
		if err != nil {
			return err
		}
		if supported {
			for name, configured := range s.nativeProjectSnapshot() {
				if configured != source {
					continue
				}
				report := s.skills(name)
				if report != nil {
					machine.Skills = &report
				}
				break
			}
		}
	}
	if err := s.sendNativeMachineHeartbeat(ctx, source, machine, s.client.runner == nil || last.IsZero()); err != nil {
		return err
	}

	s.reconcileReleasedExecutions(ctx, source)
	machine.HostMetrics = nil
	s.mu.Lock()
	s.nativeHeartbeats[project] = s.now()
	if projectConfig != nil {
		if s.configHeartbeats == nil {
			s.configHeartbeats = make(map[tracker.ProjectID]time.Time)
		}
		s.configHeartbeats[project] = s.now()
	}
	s.mu.Unlock()
	heartbeat.Unlock()
	locked = false

	if projectConfig != nil {
		if request := s.client.runner.projectConfigurationRequest(); request != nil && request.ProjectID == string(project) && request.RequestID != projectConfig.RequestID {
			view := projectOwner(ctx, string(project), request)
			if err := s.enrichProjectDiagnostics(ctx, source, &view, update); err != nil {
				return err
			}
			machine.ProjectConfiguration = &view
			heartbeat.Lock()
			err := s.sendNativeMachineHeartbeat(ctx, source, machine, false)
			heartbeat.Unlock()
			if err != nil {
				return err
			}
		}
	}
	if updateSupported {
		if request := s.client.runner.updateRequest(); request != nil {
			if applied := owner(ctx, request); applied != nil && (machine.Update == nil || applied.Revision != machine.Update.Revision || !reflect.DeepEqual(applied.Receipt, machine.Update.Receipt)) {
				s.mu.Lock()
				s.machine.Update = applied
				s.mu.Unlock()
				machine.Update = applied
				heartbeat.Lock()
				err := s.sendNativeMachineHeartbeat(ctx, source, machine, false)
				heartbeat.Unlock()
				if err != nil {
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

func (s *Scheduler) probeIsolation(ctx context.Context) (isolation.Report, []runnerauth.Problem) {
	probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if s.leaseHold != nil {
		release, err := s.leaseHold(probeCtx)
		if err != nil {
			slog.Default().Warn("sprite isolation probe hold unavailable", "error", err)
		} else {
			defer release()
		}
	}
	report, problems := s.isolationReport(probeCtx)
	s.mu.Lock()
	defer s.mu.Unlock()
	probeErr := probeCtx.Err()
	incomplete := slices.ContainsFunc(problems, func(p runnerauth.Problem) bool { return p.Code == "backend_probe_failed" })
	if probeErr != nil || incomplete {
		problem := runnerauth.NewProblem("backend_probe_failed")
		problem.Subject = "backend_isolation"
		problem.Check = "runner isolation probe"
		if probeErr != nil {
			problem.ErrorOutput = probeErr.Error()
		} else {
			for _, p := range problems {
				if p.Code == "backend_probe_failed" {
					problem.ErrorOutput = p.ErrorOutput
					break
				}
			}
		}
		problem.ReportedAt = s.now().UTC()
		if s.lastIsolation != nil {
			report = reuseIncompleteIsolation(report, s.lastIsolation, problems, probeErr != nil)
			s.lastIsolation = report
			problem.CachedCapabilities = true
			problems = slices.DeleteFunc(problems, func(p runnerauth.Problem) bool {
				return p.Code == "backend_probe_failed"
			})
			problem.Message += " Reported capabilities combine completed checks with cached capabilities; the last complete runner isolation probe finished at " + s.lastIsolationAt.UTC().Format(time.RFC3339Nano) + "."
		} else {
			problem.Message += " No completed runner isolation probe is cached; reported capabilities contain only completed backend checks."
		}
		problems = append(problems, problem)
	} else {
		s.lastIsolation = report
		s.lastIsolationAt = s.now().UTC()
	}
	return report, problems
}

func reuseIncompleteIsolation(current, cached isolation.Report, problems []runnerauth.Problem, interrupted bool) isolation.Report {
	report := make(isolation.Report, len(current)+len(cached))
	for backend, tiers := range current {
		report[backend] = slices.Clone(tiers)
	}
	for backend, tiers := range cached {
		_, observed := current[backend]
		if interrupted && !observed {
			report[backend] = []string{}
		}
		for _, tier := range tiers {
			incomplete := interrupted && !observed
			failed := false
			for _, problem := range problems {
				if problem.Subject != backend && (tier != isolation.Sandbox || problem.Subject != backend+"/sandbox") {
					continue
				}
				switch problem.Code {
				case "backend_probe_failed":
					incomplete = true
				case "backend_missing", "tier_unavailable":
					failed = true
				}
			}
			if incomplete && !failed && !slices.Contains(report[backend], tier) {
				report[backend] = append(report[backend], tier)
			}
		}
	}
	return report
}

func (s *Scheduler) reserveNativeClaim() (func(), bool) {
	if s.client != nil && s.client.runner != nil {
		s.client.runner.routingMu.Lock()
		draining := s.client.runner.routing != nil && s.client.runner.routing.Routing.State == "draining"
		s.client.runner.routingMu.Unlock()
		if draining {
			return nil, false
		}
	}
	s.mu.Lock()
	admission := s.claimAdmission
	s.mu.Unlock()
	if admission != nil {
		return admission()
	}
	return func() {}, true
}

func (s *Scheduler) claimNative(ctx context.Context, source *NativeConnector, request tracker.NativeClaim) (tracker.NativeLease, error, bool) {
	release, allowed := s.reserveNativeClaim()
	if !allowed {
		return tracker.NativeLease{}, nil, false
	}
	defer release()
	lease, err := source.client.Claim(ctx, request)
	return lease, err, true
}

func (s *Scheduler) fetchNativeCandidate(ctx context.Context, request orchestrator.SchedulingRequest, source *NativeConnector) ([]connector.Issue, error) {
	s.mu.Lock()
	s.nativeFetches++
	s.mu.Unlock()
	s.syncLeaseHold(ctx)
	defer func() {
		s.mu.Lock()
		s.nativeFetches--
		s.mu.Unlock()
		s.syncLeaseHold(ctx)
	}()
	if err := s.PrepareProject(ctx, request.ProjectID); err != nil {
		return nil, errors.Join(orchestrator.ErrSchedulingUnavailable, err)
	}
	if err := s.ensureNativeMachine(ctx, source); err != nil {
		return nil, schedulingError(err)
	}
	if len(request.DispatchPriorityByState) != 0 || len(request.DispatchPriorityByLabel) != 0 || request.PrioritizeUnblockers {
		if err := source.client.Negotiate(ctx, tracker.NativeDispatchPriorityCapability); err != nil {
			return nil, schedulingError(err)
		}
	}
	if request.LandingCapacity > 0 {
		if err := source.client.Negotiate(ctx, tracker.NativeLandingCapacityCapability); err != nil {
			return nil, schedulingError(err)
		}
	}
	releaseAdmission, allowed := s.reserveNativeClaim()
	if !allowed {
		return nil, nil
	}
	releaseAdmission()

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
		LandingCapacity:         request.LandingCapacity,
		DispatchPriorityByState: request.DispatchPriorityByState,
		DispatchPriorityByLabel: request.DispatchPriorityByLabel,
		PrioritizeUnblockers:    request.PrioritizeUnblockers,
		PolicyID:                request.Policy.ID,
		MachineID:               s.machine.ID, SessionID: session, TTLSeconds: int64(s.leaseTTL / time.Second), ProtocolMajor: 2,
		Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: request.WorkflowStates,
		Authors: request.Filter.Authors, Assignees: request.Filter.Assignees, LabelInclude: request.Filter.LabelInclude, LabelExclude: request.Filter.LabelExclude,
	}
	landingCapacitySupported, err := source.client.HubFeature(ctx, tracker.NativeLandingCapacityCapability)
	if err != nil {
		return nil, schedulingError(err)
	}
	if landingCapacitySupported {
		claimRequest.Capabilities = append(claimRequest.Capabilities, tracker.NativeLandingCapacityCapability)
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
	leases, err := s.reconnectNativeClaims(ctx, request, source, limit)
	if err != nil {
		return nil, schedulingError(err)
	}
	reconnecting := len(leases) > 0
	if reconnecting {
		err = nil
	} else if !source.client.claimHint() {
		return nil, nil
	} else if s.providerReports != nil || request.CandidateReady != nil || request.NativeLandingBatch {
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
			var admitted bool
			lease, err, admitted = s.claimNative(ctx, source, claimRequest)
			if err != nil || !admitted {
				break
			}
			leases = append(leases, lease)
		}
	}
	if errors.Is(err, ErrNoClaimableWork) {
		source.client.claimsExhausted()
		err = nil
	}
	if nativeAdmissionCapacityFull(err) {
		err = nil
	}
	release := func(leases []tracker.NativeLease, cause error) error {
		for _, lease := range leases {
			if reconnecting {
				continue
			}
			releaseErr := source.client.Release(context.WithoutCancel(ctx), lease, "work_item_hydration_failed")
			if releaseErr == nil || claimLost(releaseErr) {
				source.client.client.nativeLeases.CompareAndDelete(source.client.base()+"/"+string(lease.WorkItemID), lease)
			}
			cause = errors.Join(cause, releaseErr)
		}
		return schedulingError(cause)
	}
	if err != nil && (len(leases) == 0 || !nativeBatchIndependentFailure(ctx, err)) {
		return nil, release(leases, err)
	}
	degraded := err
	issues := make([]connector.Issue, 0, len(leases))
	claims := make([]nativeClaim, 0, len(leases))
	for i, lease := range leases {
		recovery, err := source.client.Recovery(ctx, lease.WorkItemID)
		if err == nil && recovery.Issue.LinkedSource != nil && recovery.Issue.LinkedSource.Status != "complete" {
			err = s.intakeNativeSource(ctx, source, lease, recovery.Issue)
			if err == nil {
				recovery, err = source.client.Recovery(ctx, lease.WorkItemID)
			}
		}
		if err != nil {
			if len(claims) == 0 || !nativeBatchIndependentFailure(ctx, err) {
				return nil, release(leases, errors.Join(degraded, err))
			}
			if releaseErr := release(leases[i:], nil); releaseErr != nil {
				return nil, release(leases[:i], errors.Join(degraded, err, releaseErr))
			}
			leases = leases[:i]
			degraded = errors.Join(degraded, err)
			break
		}
		issue := issueFromNative(recovery.Issue)
		issue.AssignedToWorker = true
		issue.IsolationPolicy = lease.IsolationPolicy
		issues = append(issues, issue)
		claims = append(claims, nativeClaim{landingCapacity: request.LandingCapacity, availabilityDeadline: availabilityDeadline, source: source, lease: lease, recovery: recovery, deadline: nativeLeaseDeadline(claimStarted, lease)})
	}
	if err := ctx.Err(); err != nil {
		return nil, release(leases, errors.Join(degraded, err))
	}
	if degraded != nil {
		slog.Default().Warn("native candidate fetch degraded", "project_id", source.client.project, "retained", len(claims), "error", degraded)
	}
	s.mu.Lock()
	for _, claim := range claims {
		issueID := string(claim.recovery.Issue.WorkItemID)
		s.claims[issueID] = nativeTrackerLease(claim.lease)
		s.nativeClaims[issueID] = claim
		s.claimPolicies[issueID] = claimPolicy{project: request.ProjectID, repository: request.Repository, descriptor: request.Policy}
		claim.source.client.client.nativeLeases.Store(claim.source.client.base()+"/"+issueID, claim.lease)
	}
	s.mu.Unlock()
	return issues, nil
}

func nativeBatchIndependentFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, orchestrator.ErrSchedulingUnavailable) || routingDeferred(err) || claimLost(err) {
		return false
	}
	var failure *APIError
	if errors.As(err, &failure) && (failure.Status < http.StatusInternalServerError || failure.Code == "policy_mismatch") {
		return false
	}
	return errors.Is(err, ErrUnavailable)
}

func (s *Scheduler) reconcileReleasedExecutions(ctx context.Context, source *NativeConnector) {
	if s.client.runner == nil || s.releasedStopped == nil {
		return
	}
	file, err := runnerauth.Load(s.client.runner.path)
	if err != nil {
		slog.Default().Warn("released execution reconciliation skipped", "project_id", source.client.project, "error", fmt.Errorf("load runner identity: %w", err))
		return
	}
	s.client.runner.routingMu.Lock()
	var released []runnerauth.ReleasedExecution
	if snapshot := s.client.runner.routing; snapshot != nil && snapshot.RunnerID == file.Identity.RunnerID && snapshot.ClaimState != nil {
		released = slices.Clone(snapshot.ClaimState.ReleasedExecutions)
	}
	s.client.runner.routingMu.Unlock()
	for _, reference := range released {
		if reference.ProjectID != source.client.project {
			continue
		}
		if err := s.reconcileReleasedExecution(ctx, source, file.Identity, reference); err != nil {
			slog.Default().Warn("released execution reconciliation failed", "project_id", source.client.project, "work_item_id", reference.WorkItemID, "attempt_id", reference.AttemptID, "error", err)
		}
	}
}

func (s *Scheduler) reconcileReleasedExecution(ctx context.Context, source *NativeConnector, identity runnerauth.Identity, reference runnerauth.ReleasedExecution) error {
	path, err := nativeItemPath(reference.WorkItemID)
	if err != nil {
		return fmt.Errorf("item path: %w", err)
	}
	var attempt tracker.NativeAttempt
	if err := source.client.client.request(ctx, http.MethodGet, source.client.base()+path+"/attempts/"+reference.AttemptID, nil, &attempt); err != nil {
		return fmt.Errorf("read attempt: %w", err)
	}
	if attempt.AttemptID != reference.AttemptID || attempt.MachineID != identity.MachineID || attempt.RunnerID != identity.RunnerID || attempt.ClaimReleasedAt == nil || attempt.CleanupAcknowledgedAt != nil || attempt.Identity == nil || attempt.Outcome != "" && attempt.Outcome != "interrupted" && attempt.Outcome != "cancelled" || attempt.Runtime == nil || attempt.Runtime.WorkerLaunch == nil {
		return nil
	}
	launch := attempt.Runtime.WorkerLaunch
	if launch.PID <= 0 || launch.GroupID <= 0 || launch.ProcessStart.IsZero() {
		return nil
	}
	s.mu.Lock()
	claim, known := s.nativeClaims[string(reference.WorkItemID)]
	s.mu.Unlock()
	if known && claim.lease.ID == attempt.LeaseID {
		return nil
	}
	observations, err := procgroup.Observe([]procgroup.Identity{{PID: launch.PID, GroupID: launch.GroupID, StartedAt: launch.ProcessStart}})
	if err != nil {
		return fmt.Errorf("observe worker: %w", err)
	}
	if len(observations) != 1 || !observations[0].GroupObserved || observations[0].Alive || observations[0].Stale {
		return nil
	}
	stopped, err := s.releasedStopped(ctx, reference.WorkItemID, attempt)
	if err != nil {
		return fmt.Errorf("check providers stopped: %w", err)
	}
	if !stopped {
		return nil
	}
	if err := cancelNativeReleasedAttempt(ctx, source.client, reference.WorkItemID, attempt); err != nil {
		return fmt.Errorf("acknowledge released attempt: %w", err)
	}
	return nil
}

func (s *Scheduler) reconnectNativeClaims(ctx context.Context, request orchestrator.SchedulingRequest, source *NativeConnector, limit int) ([]tracker.NativeLease, error) {
	if s.client.runner == nil {
		return nil, nil
	}
	s.client.runner.routingMu.Lock()
	var slots []runnerauth.ClaimSlot
	if snapshot := s.client.runner.routing; snapshot != nil && snapshot.ClaimState != nil {
		slots = slices.Clone(snapshot.ClaimState.Slots)
	}
	s.client.runner.routingMu.Unlock()
	file, err := runnerauth.Load(s.client.runner.path)
	if err != nil {
		return nil, err
	}
	pending := false
	s.mu.Lock()
	for _, slot := range slots {
		if slot.RunnerID != file.Identity.RunnerID {
			continue
		}
		known := false
		for _, claim := range s.nativeClaims {
			if claim.lease.ID == slot.ID {
				known = true
				break
			}
		}
		pending = pending || !known
	}
	s.mu.Unlock()
	if !pending {
		return nil, nil
	}
	var owner runnerauth.Runner
	path := "/api/v2/organizations/" + string(file.Identity.OrganizationID) + "/runners/" + file.Identity.RunnerID + "/routing"
	if err := s.client.request(ctx, http.MethodGet, path, nil, &owner); err != nil {
		return nil, err
	}
	var leases []tracker.NativeLease
	for _, held := range owner.Leases {
		if held.ProjectID != source.client.project || held.Policy.ID != request.Policy.ID || len(held.Exclusions) != 0 {
			continue
		}
		s.mu.Lock()
		_, adopted := s.nativeClaims[string(held.WorkItemID)]
		s.mu.Unlock()
		if adopted {
			continue
		}
		recovery, err := source.client.Recovery(ctx, held.WorkItemID)
		if err != nil {
			return nil, err
		}
		for _, attempt := range recovery.Attempts {
			if attempt.LeaseID == held.ID && attempt.Status != "running" && attempt.Status != "interrupted" {
				adopted = true
			}
		}
		if adopted {
			continue
		}
		renewable, err := s.nativeClaimRenewable(ctx, nativeClaim{source: source, lease: tracker.NativeLease{ID: held.ID, WorkItemID: held.WorkItemID, FencingToken: held.FencingToken}, recovery: recovery})
		if err != nil {
			return nil, err
		}
		if !renewable {
			if err := source.client.release(ctx, tracker.NativeLease{ID: held.ID, FencingToken: held.FencingToken}, "cancelled", recovery.Issue.SchedulingEligible != nil); err != nil && !nativeLeaseLost(err) {
				return nil, err
			}
			for _, attempt := range recovery.Attempts {
				if recovery.Issue.SchedulingEligible == nil && attempt.LeaseID == held.ID && attempt.FencingToken == held.FencingToken && (attempt.Status == "running" || attempt.Status == "interrupted") && attempt.Identity != nil {
					if err := cancelNativeReleasedAttempt(ctx, source.client, held.WorkItemID, attempt); err != nil {
						return nil, err
					}
				}
			}
			continue
		}
		lease, err := source.client.Renew(ctx, tracker.NativeLease{ID: held.ID, FencingToken: held.FencingToken}, int64(s.leaseTTL/time.Second))
		if nativeLeaseLost(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
		if len(leases) == limit {
			break
		}
	}
	return leases, nil
}

func nativeAdmissionCapacityFull(err error) bool {
	var failure *APIError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "provider_capacity", "runner_capacity", "host_capacity", "plan_capacity":
		return true
	default:
		return false
	}
}

func (s *Scheduler) renewNativeClaim(ctx context.Context, issueID string, claim nativeClaim, landingCapacity int) (orchestrator.Claimed, error) {
	if claim.lost {
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, orchestrator.ErrSchedulingClaimLost)
	}
	if err := s.checkClaimPolicyApproval(ctx, issueID, claim.lease.PolicyID); err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, err)
	}
	if err := s.ensureNativeMachine(ctx, claim.source); err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, err)
	}
	renewable, err := s.nativeClaimRenewable(ctx, claim)
	if err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, err)
	}
	if !renewable {
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, orchestrator.ErrSchedulingClaimLost)
	}
	renewStarted := s.now()
	lease, err := claim.source.client.renew(ctx, claim.lease, int64(s.leaseTTL/time.Second), landingCapacity)
	if err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, err)
	}
	claim.lease = lease
	claim.deadline = nativeLeaseDeadline(renewStarted, lease)
	s.mu.Lock()
	if current, ok := s.nativeClaims[issueID]; !ok || current.lost || current.lease.FencingToken != lease.FencingToken {
		s.mu.Unlock()
		return orchestrator.Claimed{}, orchestrator.ErrSchedulingClaimLost
	}
	claim.execution = s.nativeClaims[issueID].execution
	claim.availabilityDeadline = s.nativeClaims[issueID].availabilityDeadline
	s.nativeClaims[issueID] = claim
	s.claims[issueID] = nativeTrackerLease(lease)
	s.mu.Unlock()
	if err := s.checkClaimRunner(ctx, issueID, claim.lease.PolicyID); err != nil {
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, err)
	}
	return claimedIssue(connector.Issue{ID: issueID}, nativeTrackerLease(lease)), nil
}

func (s *Scheduler) nativeClaimRenewable(ctx context.Context, claim nativeClaim) (bool, error) {
	var observation *tracker.NativeRuntimeObservation
	localWorker := false
	landed := false
	if execution := claim.execution; execution != nil {
		localWorker = execution.hasWorkers()
		execution.cancelMu.Lock()
		controller := execution.cancel != nil
		execution.cancelMu.Unlock()
		execution.diagnosticMu.Lock()
		pending := execution.diagnostic.OperationPending
		execution.diagnosticMu.Unlock()
		if pending || !execution.mu.TryLock() {
			return true, nil
		}
		pending = execution.pending != nil || execution.data.Outcome != ""
		landed = execution.landed
		if execution.data.Runtime != nil {
			runtime := *execution.data.Runtime
			observation = &runtime
		}
		// The controller owns bounded finalization after the turn ends. Its
		// turn-end heartbeat cannot expire a live publication or review.
		outage := !execution.completionOutageDeadline.IsZero()
		if observation != nil && observation.Phase == "completed" && (controller || localWorker) && !outage {
			execution.mu.Unlock()
			return true, nil
		}
		if outage && s.now().Before(execution.completionOutageDeadline) && execution.preparedOutcome != "" {
			execution.mu.Unlock()
			return true, nil
		}
		if outage && !s.now().Before(execution.completionOutageDeadline) && pending {
			execution.mu.Unlock()
			return false, nil
		}
		if !pending && observation != nil && observation.Phase == "completed" &&
			(observation.HeartbeatAt.IsZero() || !s.now().Before(observation.HeartbeatAt.Add(s.leaseTTL))) {
			execution.RetainCompletion()
			execution.preparedOutcome = "failed"
			execution.preparedDisposition = nil
			execution.preparedFailure = execution.finalizationFailure(errors.New("completed worker runtime expired before finalization"))
			if outage {
				execution.preparedFailure.ErrorClass = "publication"
				execution.preparedFailure.Error = "Hub unavailable during completion finalization"
				failure := execution.preparedFailure.Public()
				execution.preparedFailure = &failure
			}
			err := execution.finish(ctx, "failed")
			finished := execution.data.Outcome != ""
			execution.mu.Unlock()
			if finished {
				execution.refreshReasons(ctx, true)
			}
			return false, err
		}
		prepared := execution.preparedOutcome != ""
		execution.mu.Unlock()
		if pending || prepared {
			return true, nil
		}
	}
	if observation == nil {
		for _, attempt := range claim.recovery.Attempts {
			if attempt.LeaseID == claim.lease.ID && attempt.FencingToken == claim.lease.FencingToken {
				observation = attempt.Runtime
				break
			}
		}
	}
	issue, err := claim.source.client.Issue(ctx, claim.lease.WorkItemID)
	if err != nil {
		return false, err
	}
	landed = landed || observation != nil && observation.Landing != nil && observation.Landing.Landed
	if issue.Terminal && !issue.Archived && !landed && claim.execution != nil {
		if !claim.execution.mu.TryLock() {
			return true, nil
		}
		landed = claim.execution.landed
		claim.execution.mu.Unlock()
	}
	if !issue.Terminal || issue.Archived || !landed {
		if issue.SchedulingEligible != nil {
			if !*issue.SchedulingEligible {
				return false, nil
			}
		} else {
			if issue.Terminal || issue.Archived || !issue.DependencyReady {
				return false, nil
			}
			project, err := claim.source.client.Project(ctx)
			if err != nil {
				return false, err
			}
			dispatchable := false
			for _, state := range project.States {
				if strings.EqualFold(state.Name, issue.State) {
					dispatchable = state.Dispatchable && !state.Terminal && !state.OperatorOnly
					break
				}
			}
			if !dispatchable {
				return false, nil
			}
		}
	}
	if observation == nil || observation.LocalAttemptID == 0 || observation.Generation == 0 {
		return true, nil
	}
	if observation.Phase == "completed" {
		return false, nil
	}
	if localWorker && observation.WorkerLaunch != nil {
		launch := observation.WorkerLaunch
		alive, err := procgroup.Alive(procgroup.Identity{PID: launch.PID, GroupID: launch.GroupID, StartedAt: launch.ProcessStart})
		if err != nil {
			return false, errors.Join(ErrUnavailable, err)
		}
		if alive {
			if err := claim.execution.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{HeartbeatAt: s.now().UTC()}); err != nil {
				return false, err
			}
		}
		return alive, nil
	}
	return !observation.HeartbeatAt.IsZero() && s.now().Sub(observation.HeartbeatAt) < s.leaseTTL, nil
}

func nativeLeaseDeadline(started time.Time, lease tracker.NativeLease) time.Time {
	if lease.ServerTime.IsZero() {
		return started
	}
	return started.Add(lease.ExpiresAt.Sub(lease.ServerTime))
}

func (s *Scheduler) nativeClaimError(ctx context.Context, issueID string, token tracker.FencingToken, err error) error {
	var apiErr *APIError
	lostAuthority := errors.As(err, &apiErr) && apiErr != nil &&
		(apiErr.Code == "policy_mismatch" || apiErr.Code == "selector_no_match")
	if nativeAuthorityLost(err) || lostAuthority {
		if s.invalidateNativeClaim(ctx, issueID, token, err) {
			return errors.Join(orchestrator.ErrSchedulingClaimLost, runner.ErrExecutionAuthorityUnavailable, err)
		}
		return errors.Join(orchestrator.ErrSchedulingClaimLost, err)
	}
	return err
}

func (s *Scheduler) invalidateNativeClaim(ctx context.Context, issueID string, token tracker.FencingToken, cause error) bool {
	s.mu.Lock()
	if current, ok := s.nativeClaims[issueID]; ok && current.lease.FencingToken == token {
		current.lost = true
		s.nativeClaims[issueID] = current
		if execution := current.execution; execution != nil {
			pending := execution.hasWorkers() || execution.completionOwner.Load()
			execution.cancelMu.Lock()
			pending = pending || execution.cancel != nil
			execution.cancelMu.Unlock()
			if pending {
				s.mu.Unlock()
				execution.cancelExecution(errors.Join(runner.ErrExecutionAuthorityUnavailable, cause))
				return true
			}
		}
		s.mu.Unlock()
		if err := current.source.client.release(ctx, current.lease, "cancelled", true); err != nil && !nativeLeaseLost(err) {
			slog.Default().Warn("native claim release unavailable", "work_item_id", issueID, "error", err)
			return true
		}
		s.mu.Lock()
		if successor, ok := s.nativeClaims[issueID]; !ok || successor.lease.ID != current.lease.ID || successor.lease.FencingToken != token {
			s.mu.Unlock()
			return false
		}
		current.source.client.slotFreed()
		delete(s.claims, issueID)
		delete(s.nativeClaims, issueID)
		delete(s.claimPolicies, issueID)
	}
	s.mu.Unlock()
	return false
}

func (s *Scheduler) ForgetClaim(issueID string) {
	s.mu.Lock()
	claim, ok := s.nativeClaims[issueID]
	s.mu.Unlock()
	if ok {
		s.invalidateNativeClaim(context.Background(), issueID, claim.lease.FencingToken, orchestrator.ErrSchedulingClaimLost)
	}
}

func (s *Scheduler) cleanupNativeClaim(ctx context.Context, issueID string, token tracker.FencingToken) {
	s.mu.Lock()
	claim, ok := s.nativeClaims[issueID]
	s.mu.Unlock()
	if ok && claim.lost && claim.lease.FencingToken == token {
		s.invalidateNativeClaim(ctx, issueID, token, orchestrator.ErrSchedulingClaimLost)
	}
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
	source := s.nativeProject(project)
	if source == nil || source.client == nil {
		return nil, false
	}
	return source.client, true
}

// MachineID is the machine identity this scheduler registers and claims under.
// The workspace lane claims under the same one: a
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
	return errors.Is(err, orchestrator.ErrSchedulingClaimLost) || nativeLeaseLost(err) || errors.As(err, &apiErr) && apiErr != nil &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound)
}

func (s *Scheduler) CommitIdentity() workspace.CommitIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return workspace.CommitIdentity{Name: "Detent runner", Email: string(s.machine.ID) + "@runners.detent.invalid"}
}

func (s *Scheduler) ReportCommitPolicy(project string, report workspace.CommitReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localChecks == nil {
		s.localChecks = make(map[string]runnerauth.LocalChecks)
	}
	checks := s.localChecks[project]
	checks.CommitName = report.Identity.Name
	checks.CommitEmail = report.Identity.Email
	checks.CommitSigning = report.Signing
	s.localChecks[project] = checks
}
