package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	backendCapacityResetJitter = 5 * time.Second
	backendCapacityProbeDelay  = 5 * time.Minute
	backendCapacityProbeMax    = time.Hour
)

type BackendOutage struct {
	Scope                backendcapacity.Scope
	Kind                 string
	Reason               string
	Trigger              string
	DetectedAt           time.Time
	LastObservedAt       time.Time
	ResetAt              time.Time
	ResumeAt             time.Time
	NextProbeAt          time.Time
	LastProbeAt          time.Time
	LastProbeResult      string
	LastProbeDetail      string
	ProbeAttempts        int
	ProbeIssueID         string
	QuotaReadUnsupported bool
}

type BackendRecovery struct {
	Outage           BackendOutage
	RecoveredAt      time.Time
	SuppressedIssues map[string]string
}

type validatorCapacityEvent struct {
	Scope         backendcapacity.Scope
	CapacityErr   *backendcapacity.Error
	ProbeErr      error
	CapacityProbe bool
	CompletedAt   time.Time
}

func (o *Orchestrator) handleBackendCapacityError(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	capacityErr *backendcapacity.Error,
) {
	running = o.restoreBackendCapacityIssueState(ctx, state, running, event.CompletedAt)
	outage := o.registerBackendOutage(state, capacityErr, event.CompletedAt, running.CapacityProbe)
	attemptCompleted := o.completeDurableWorkAttemptWithMetadata(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		backendcapacity.ErrorClass,
		event.Err.Error(),
		"waiting",
		backendCapacityStatusMessage(outage),
		nil,
	)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		o.releaseClaim(state, running.Issue.ID)
		return
	}
	var parked bool
	running.Issue, _, parked = o.demoteTerminalAttemptRetry(
		ctx,
		state,
		running.Issue,
		running.WorkProductPushed,
		workAttemptErrorRunner,
		attemptCompleted,
		running.Mode,
		running.DiffStats,
		event.CompletedAt,
	)
	if parked {
		return
	}
	o.scheduleBackendCapacityRetry(state, running, outage)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "backend_capacity_paused",
		Message: backendCapacityStatusMessage(outage),
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, "backend_capacity_paused", "backend capacity paused", o.runningLifecycleCorrelation(running.Issue, running),
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"capacity_kind", outage.Kind,
			"capacity_reason", outage.Reason,
			"capacity_trigger", outage.Trigger,
			"reset_at", outage.ResetAt,
			"resume_at", outage.ResumeAt,
			"error", event.Err,
		)
	}
}

func (o *Orchestrator) registerBackendOutage(
	state *State,
	capacityErr *backendcapacity.Error,
	observedAt time.Time,
	capacityProbe bool,
) BackendOutage {
	if capacityErr == nil || capacityErr.Details.Type == backendcapacity.ErrorTypeTransientOverload || o.cfg.subscriptionBilling() && capacityErr.Details.Type == backendcapacity.ErrorTypeUsageLimit {
		return BackendOutage{}
	}
	if observedAt.IsZero() {
		observedAt = o.clockNow()
	}
	if state.BackendOutages == nil {
		state.BackendOutages = map[string]BackendOutage{}
	}
	scope := capacityErr.Scope.Normalize()
	key, existing, ok := matchingBackendOutage(state.BackendOutages, scope)
	if !ok {
		key = scope.Key()
		existing = BackendOutage{Scope: scope, DetectedAt: observedAt}
	}
	existing.Scope = scope
	existing.Kind = strings.TrimSpace(capacityErr.Details.Kind)
	existing.Reason = strings.TrimSpace(capacityErr.Details.Reason)
	existing.Trigger = strings.TrimSpace(capacityErr.Details.Trigger)
	existing.LastObservedAt = observedAt
	existing.ResetAt = time.Time{}
	if capacityErr.Details.ResetAt != nil {
		existing.ResetAt = capacityErr.Details.ResetAt.UTC()
	}
	existing.ResumeAt = backendCapacityResumeAt(existing.ResetAt, observedAt)
	if capacityProbe {
		existing.ProbeIssueID = ""
		if existing.ProbeAttempts == 0 {
			existing.ProbeAttempts = 1
		}
		existing.LastProbeAt = observedAt
		existing.LastProbeResult = "capacity_exhausted"
		existing.LastProbeDetail = existing.Reason
	}
	if strings.TrimSpace(existing.ProbeIssueID) == "" {
		delay := backendCapacityProbeDelay
		if backendCapacityProviderOutage(existing) {
			delay = backendCapacityProbeDelayForAttempt(existing.ProbeAttempts)
			existing.ResumeAt = observedAt.Add(delay)
		}
		existing.NextProbeAt = backendCapacityBoundedProbeAt(existing.ResumeAt, observedAt.Add(delay), observedAt)
		if existing.QuotaReadUnsupported && existing.NextProbeAt.Before(existing.ResumeAt) {
			existing.NextProbeAt = existing.ResumeAt
		}
	}
	state.BackendOutages[key] = existing
	delete(state.BackendRecoveries, key)
	return existing
}

func backendCapacityProviderOutage(outage BackendOutage) bool {
	return outage.Kind == "http_404" || (len(outage.Kind) == len("http_500") && strings.HasPrefix(outage.Kind, "http_5"))
}

func (o *Orchestrator) subscriptionQuotaOutage(outage BackendOutage) bool {
	switch outage.Scope.BackendKind {
	case workflowconfig.AgentBackendCodex, workflowconfig.AgentBackendClaudeCode, workflowconfig.AgentBackendPiAgent, workflowconfig.AgentBackendOpenCode, workflowconfig.AgentBackendAntigravity:
		return o.cfg.subscriptionBilling() && outage.Scope.Hosted() && !backendCapacityProviderOutage(outage)
	default:
		return false
	}
}

func backendCapacityCanary(outage BackendOutage) bool {
	return backendCapacityProviderOutage(outage) || outage.QuotaReadUnsupported
}

func (o *Orchestrator) refreshBackendCapacity(ctx context.Context, state *State, now time.Time) {
	if o.cfg.subscriptionBilling() {
		clearedQuota := false
		for key, outage := range state.BackendOutages {
			if o.subscriptionQuotaOutage(outage) {
				clearedQuota = true
				delete(state.BackendOutages, key)
				delete(state.DispatchRecoveries, dispatchRecoveryBackendCapacity+":"+outage.Scope.Key())
				releaseBackendCapacityRetries(state, outage.Scope, now)
			}
		}
		for key, recovery := range state.BackendRecoveries {
			if o.subscriptionQuotaOutage(recovery.Outage) {
				clearedQuota = true
				delete(state.BackendRecoveries, key)
				delete(state.DispatchRecoveries, dispatchRecoveryBackendCapacity+":"+recovery.Outage.Scope.Key())
				releaseBackendCapacityRetries(state, recovery.Outage.Scope, now)
			}
		}
		if clearedQuota {
			if _, active := activeProviderCapacityOutage(state.BackendOutages); !active {
				key, outage, held := githubLookupBackoff(state.BackendOutages)
				if held && outage.Trigger == githubLookupTriggerProvider {
					delete(state.BackendOutages, key)
					delete(state.DispatchRecoveries, dispatchRecoveryGitHubLookup)
				}
			}
		}
		return
	}
	if o.capacityReader == nil {
		return
	}
	for _, key := range sortedKeys(state.BackendOutages) {
		outage := state.BackendOutages[key]
		if outage.Scope.BackendKind != workflowconfig.AgentBackendCodex && outage.Scope.BackendKind != workflowconfig.AgentBackendClaudeCode && outage.Scope.BackendKind != workflowconfig.AgentBackendPiAgent && outage.Scope.BackendKind != workflowconfig.AgentBackendOpenCode && outage.Scope.BackendKind != workflowconfig.AgentBackendAntigravity {
			continue
		}
		if backendCapacityProviderOutage(outage) || !outage.Scope.Hosted() || outage.ProbeIssueID != "" ||
			(!outage.QuotaReadUnsupported && now.Before(outage.NextProbeAt)) ||
			(!outage.LastProbeAt.IsZero() && now.Before(outage.LastProbeAt.Add(backendCapacityProbeDelay))) {
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		observation, err := o.capacityReader.ReadBackendCapacity(readCtx, outage.Scope, now)
		cancel()
		outage.LastProbeAt = now
		outage.QuotaReadUnsupported = errors.Is(err, runpkg.ErrCapacityReadUnsupported)
		if outage.QuotaReadUnsupported {
			if outage.NextProbeAt.Before(outage.ResumeAt) {
				outage.NextProbeAt = outage.ResumeAt
			}
		} else {
			outage.NextProbeAt = now.Add(backendCapacityProbeDelay)
		}
		outage.ProbeIssueID = ""
		outage.ProbeAttempts++
		outage.LastProbeResult = "status_unknown"
		if err == nil {
			outage.LastProbeDetail = observation.Status.Detail
			if observation.Status.Available && !observation.Status.Exhausted {
				outage.LastProbeResult = "status_available"
				o.completeBackendCapacityRecovery(state, key, outage, now, "live_status")
				continue
			}
			if observation.Status.Exhausted {
				outage.LastProbeResult = "status_exhausted"
			}
		} else {
			outage.LastProbeDetail = "provider capacity read unavailable"
		}
		state.BackendOutages[key] = outage
	}
}

func backendCapacityProbeDelayForAttempt(attempt int) time.Duration {
	delay := backendCapacityProbeDelay
	for range max(attempt, 0) {
		if delay >= backendCapacityProbeMax/2 {
			return backendCapacityProbeMax
		}
		delay *= 2
	}
	return min(delay, backendCapacityProbeMax)
}

func backendCapacityBoundedProbeAt(resumeAt time.Time, probeAt time.Time, now time.Time) time.Time {
	if resumeAt.After(now) && resumeAt.Before(probeAt) {
		return resumeAt
	}
	return probeAt
}

func backendCapacityResumeAt(resetAt time.Time, now time.Time) time.Time {
	if resetAt.IsZero() {
		return now.Add(backendCapacityProbeDelay)
	}
	resumeAt := resetAt.Add(backendCapacityResetJitter)
	minimum := now.Add(backendCapacityResetJitter)
	if resumeAt.Before(minimum) {
		return minimum
	}
	return resumeAt
}

func backendCapacityStatusMessage(outage BackendOutage) string {
	backend := strings.TrimSpace(outage.Scope.BackendID)
	if backend == "" {
		backend = strings.TrimSpace(outage.Scope.BackendKind)
	}
	if backend == "" {
		backend = "agent backend"
	}
	reason := strings.TrimSpace(outage.Reason)
	message := "backend " + backend + " at usage limit"
	if reason != "" {
		message = "backend " + backend + ": " + reason
	}
	if !outage.ResumeAt.IsZero() {
		message += ", resumes ~" + outage.ResumeAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	if !outage.NextProbeAt.IsZero() {
		message += "; next probe ~" + outage.NextProbeAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	return message
}

func (o *Orchestrator) scheduleBackendCapacityRetry(state *State, running Running, outage BackendOutage) {
	issue := cloneIssue(running.Issue)
	dueAt := outage.NextProbeAt
	if dueAt.IsZero() {
		dueAt = outage.ResumeAt
	}
	state.Retry[issue.ID] = Retry{
		Issue:         issue,
		Attempt:       running.Attempt,
		DueAt:         dueAt,
		Error:         backendCapacityStatusMessage(outage),
		WorkerHost:    running.WorkerHost,
		CapacityScope: outage.Scope,
	}
}

func (o *Orchestrator) restoreBackendCapacityIssueState(
	ctx context.Context,
	state *State,
	running Running,
	now time.Time,
) Running {
	source := strings.TrimSpace(running.DispatchSourceState)
	target := strings.TrimSpace(running.DispatchTargetState)
	if source == "" || target == "" || normalizeState(running.Issue.State) != normalizeState(target) {
		return running
	}
	issue := cloneIssue(running.Issue)
	if err := o.updateIssueState(ctx, state, issue, source, now, "backend_capacity_pause"); err != nil {
		if o.logger != nil {
			o.logger.Warn("backend capacity state restore failed", "issue_id", issue.ID, "source_state", source, "error", err)
		}
		return running
	}
	issue.State = source
	running.Issue = issue
	return running
}

func (o *Orchestrator) backendCapacityDispatch(
	state *State,
	request runpkg.RunRequest,
	now time.Time,
) (backendcapacity.Scope, string, bool) {
	scope, ok := o.backendCapacityScope(request)
	if !ok {
		return backendcapacity.Scope{}, "", false
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, scope)
	if !ok || o.subscriptionQuotaOutage(outage) {
		return scope, "", false
	}
	if !backendCapacityCanary(outage) {
		return scope, "", true
	}
	probeAt := outage.NextProbeAt
	if probeAt.IsZero() {
		probeAt = outage.ResumeAt
	}
	if strings.TrimSpace(outage.ProbeIssueID) != "" || (!probeAt.IsZero() && now.Before(probeAt)) {
		return scope, "", true
	}
	return scope, key, false
}

func (o *Orchestrator) backendCapacityScope(request runpkg.RunRequest) (backendcapacity.Scope, bool) {
	if o.capacityController != nil {
		return o.capacityController.CapacityScope(request)
	}
	return backendcapacity.Scope{}, false
}

func (o *Orchestrator) validatorCapacityDispatch(
	state *State,
	issue connector.Issue,
	now time.Time,
) (backendcapacity.Scope, string, bool) {
	if state == nil || o.validatorCapacity == nil {
		return backendcapacity.Scope{}, "", false
	}
	scope, ok := o.validatorCapacity.ValidatorCapacityScope(runpkg.ValidatorRequest{
		Issue:           issue,
		StartedAt:       now,
		SelectorContext: o.selectorContext(),
	})
	if !ok {
		return backendcapacity.Scope{}, "", false
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, scope)
	if !ok || o.subscriptionQuotaOutage(outage) {
		return scope, "", false
	}
	if !backendCapacityCanary(outage) {
		return scope, "", true
	}
	probeAt := outage.NextProbeAt
	if probeAt.IsZero() {
		probeAt = outage.ResumeAt
	}
	if strings.TrimSpace(outage.ProbeIssueID) != "" || (!probeAt.IsZero() && now.Before(probeAt)) {
		return scope, "", true
	}
	return scope, key, false
}

func (o *Orchestrator) publishValidatorCapacityEvent(ctx context.Context, event validatorCapacityEvent) {
	if o.validatorCapacityEvents == nil {
		return
	}
	select {
	case o.validatorCapacityEvents <- event:
	case <-ctx.Done():
	case <-o.done:
	}
}

func (o *Orchestrator) handleValidatorCapacityEvent(state *State, event validatorCapacityEvent) {
	if event.CapacityErr != nil {
		if o.cfg.subscriptionBilling() && event.CapacityErr.Details.Type == backendcapacity.ErrorTypeUsageLimit {
			return
		}
		if event.CapacityErr.Details.Type == backendcapacity.ErrorTypeTransientOverload {
			if event.CapacityProbe {
				releaseBackendCapacityProbe(state, Running{CapacityScope: event.Scope, CapacityProbe: true})
			}
			return
		}
		outage := o.registerBackendOutage(state, event.CapacityErr, event.CompletedAt, event.CapacityProbe)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      event.CompletedAt,
			Event:   "backend_capacity_paused",
			Message: backendCapacityStatusMessage(outage),
		})
		if o.logger != nil {
			telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, "backend_capacity_paused", "validator backend capacity paused", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
				"backend_id", outage.Scope.BackendID,
				"provider", outage.Scope.Provider,
				"capacity_kind", outage.Kind,
				"capacity_reason", outage.Reason,
				"capacity_trigger", outage.Trigger,
				"resume_at", outage.ResumeAt,
				"error", event.CapacityErr,
			)
		}
		return
	}
	if event.CapacityProbe {
		running := Running{CapacityScope: event.Scope, CapacityProbe: true}
		if event.ProbeErr != nil {
			o.deferBackendCapacityProbe(state, running, event.CompletedAt, event.ProbeErr)
			return
		}
		o.recoverBackendCapacity(state, running, event.CompletedAt)
	}
}

func releaseBackendCapacityProbe(state *State, running Running) {
	if state == nil || !running.CapacityProbe {
		return
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, running.CapacityScope)
	if !ok {
		return
	}
	outage.ProbeIssueID = ""
	state.BackendOutages[key] = outage
}

func matchingBackendOutage(outages map[string]BackendOutage, scope backendcapacity.Scope) (string, BackendOutage, bool) {
	for key, outage := range outages {
		if outage.Scope.Matches(scope) {
			return key, outage, true
		}
	}
	return "", BackendOutage{}, false
}

func (o *Orchestrator) markBackendCapacityProbe(state *State, key string, issueID string, startedAt time.Time) {
	if key == "" {
		return
	}
	outage, ok := state.BackendOutages[key]
	if !ok {
		return
	}
	outage.ProbeIssueID = strings.TrimSpace(issueID)
	outage.NextProbeAt = time.Time{}
	outage.LastProbeAt = startedAt
	outage.LastProbeResult = "in_progress"
	outage.LastProbeDetail = "canary dispatch started"
	outage.ProbeAttempts++
	state.BackendOutages[key] = outage
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      startedAt,
		Event:   "backend_capacity_probe_started",
		Message: "backend " + outage.Scope.BackendID + " capacity probe started with " + strings.TrimSpace(issueID),
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelInfo, telemetry.LifecycleSafetyControl, "backend_capacity_probe_started", "backend capacity probe started", telemetry.LifecycleCorrelation{
			ProjectID: o.cfg.Project.ID,
			IssueID:   issueID,
		},
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"probe_attempt", outage.ProbeAttempts,
			"provider_resume_at", outage.ResumeAt,
		)
	}
}

func (o *Orchestrator) recoverBackendCapacity(state *State, running Running, recoveredAt time.Time) {
	if !running.CapacityProbe {
		return
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, running.CapacityScope)
	if !ok || !backendCapacityCanary(outage) {
		return
	}
	outage.LastProbeAt = recoveredAt
	outage.LastProbeResult = "capacity_available"
	outage.LastProbeDetail = "canary dispatch reached the provider"
	outage.NextProbeAt = time.Time{}
	o.completeBackendCapacityRecovery(state, key, outage, recoveredAt, "canary")
}

func (o *Orchestrator) recoverBackendCapacityFromStatus(
	state *State,
	running Running,
	rateLimits *telemetry.RateLimits,
	observedAt time.Time,
) {
	if o.cfg.subscriptionBilling() || o.capacityStatus == nil || rateLimits == nil || !running.CapacityScope.Hosted() {
		return
	}
	status, ok := o.capacityStatus.BackendCapacityStatus(running.CapacityScope, rateLimits)
	if !ok {
		return
	}
	if observedAt.IsZero() {
		observedAt = o.clockNow()
	}
	key, outage, active := matchingBackendOutage(state.BackendOutages, running.CapacityScope)
	if active && backendCapacityProviderOutage(outage) {
		return
	}
	if status.Exhausted {
		wasActive := active
		details := status.Details
		if details.Type == "" {
			details.Type = backendcapacity.ErrorTypeUsageLimit
		}
		if strings.TrimSpace(details.Kind) == "" {
			details.Kind = "subscription_window_exhausted"
		}
		if strings.TrimSpace(details.Reason) == "" {
			details.Reason = "subscription window exhausted"
		}
		detail := strings.TrimSpace(status.Detail)
		if detail == "" {
			detail = details.Reason
		}
		capacityErr, classified := backendcapacity.As(backendcapacity.NewError(
			running.CapacityScope,
			details,
			errors.New(detail),
		))
		if !classified {
			return
		}
		o.registerBackendOutage(state, capacityErr, observedAt, running.CapacityProbe)
		key, outage, active = matchingBackendOutage(state.BackendOutages, running.CapacityScope)
		if !active {
			return
		}
		outage.LastProbeAt = observedAt
		outage.LastProbeResult = "status_exhausted"
		outage.LastProbeDetail = strings.TrimSpace(status.Detail)
		state.BackendOutages[key] = outage
		if !wasActive {
			recordStateEvent(state, telemetry.ActivityEvent{
				At:      observedAt,
				Event:   "backend_capacity_paused",
				Message: backendCapacityStatusMessage(outage),
			})
			telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, "backend_capacity_paused", "backend capacity paused from provider status", o.runningLifecycleCorrelation(running.Issue, running),
				"backend_id", outage.Scope.BackendID,
				"backend_kind", outage.Scope.BackendKind,
				"provider", outage.Scope.Provider,
				"reset_at", outage.ResetAt,
				"resume_at", outage.ResumeAt,
			)
		}
		return
	}
	if !active {
		return
	}
	outage.LastProbeAt = observedAt
	outage.LastProbeDetail = strings.TrimSpace(status.Detail)
	if !status.Available {
		outage.LastProbeResult = "status_exhausted"
		state.BackendOutages[key] = outage
		return
	}
	outage.LastProbeResult = "status_available"
	outage.NextProbeAt = time.Time{}
	o.completeBackendCapacityRecovery(state, key, outage, observedAt, "live_status")
}

func (o *Orchestrator) completeBackendCapacityRecovery(
	state *State,
	key string,
	outage BackendOutage,
	recoveredAt time.Time,
	source string,
) {
	delete(state.BackendOutages, key)
	if state.BackendRecoveries == nil {
		state.BackendRecoveries = map[string]BackendRecovery{}
	}
	state.BackendRecoveries[key] = BackendRecovery{Outage: outage, RecoveredAt: recoveredAt}
	o.activateBackendDispatchRecovery(state, outage, recoveredAt)
	releaseBackendCapacityRetries(state, outage.Scope, recoveredAt)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      recoveredAt,
		Event:   "backend_capacity_recovered",
		Message: "backend " + outage.Scope.BackendID + " capacity recovered via " + source,
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelInfo, telemetry.LifecycleSafetyControl, "backend_capacity_recovered", "backend capacity recovered", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"detected_at", outage.DetectedAt,
			"recovered_at", recoveredAt,
			"source", source,
		)
	}
}

func (o *Orchestrator) deferBackendCapacityProbe(state *State, running Running, failedAt time.Time, probeErr error) {
	if !running.CapacityProbe {
		return
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, running.CapacityScope)
	if !ok {
		return
	}
	if failedAt.IsZero() {
		failedAt = o.clockNow()
	}
	outage.ProbeIssueID = ""
	if outage.ProbeAttempts == 0 {
		outage.ProbeAttempts = 1
	}
	outage.LastProbeAt = failedAt
	outage.LastProbeResult = "failed"
	outage.LastProbeDetail = strings.TrimSpace(probeErr.Error())
	delay := backendCapacityProbeDelayForAttempt(outage.ProbeAttempts)
	outage.NextProbeAt = backendCapacityBoundedProbeAt(outage.ResumeAt, failedAt.Add(delay), failedAt)
	state.BackendOutages[key] = outage
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      failedAt,
		Event:   "backend_capacity_probe_deferred",
		Message: "backend " + outage.Scope.BackendID + " capacity probe failed; retrying at " + outage.NextProbeAt.Format(time.RFC3339),
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, "backend_capacity_probe_deferred", "backend capacity probe deferred", o.runningLifecycleCorrelation(running.Issue, running),
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"retry_at", outage.NextProbeAt,
			"provider_resume_at", outage.ResumeAt,
			"error", probeErr,
		)
	}
}

func releaseBackendCapacityRetries(state *State, scope backendcapacity.Scope, releasedAt time.Time) {
	for issueID, retry := range state.Retry {
		if !retry.CapacityScope.Matches(scope) {
			continue
		}
		retry.DueAt = releasedAt
		state.Retry[issueID] = retry
	}
}

func (o *Orchestrator) clearBackendCapacity(state *State, scopeFilter string, clearedAt time.Time, immediate ...bool) []BackendOutage {
	full := len(immediate) > 0 && immediate[0]
	if clearedAt.IsZero() {
		clearedAt = o.clockNow()
	}
	cleared := []BackendOutage{}
	for _, key := range sortedKeys(state.BackendOutages) {
		outage := state.BackendOutages[key]
		if !backendCapacityScopeMatchesFilter(outage.Scope, scopeFilter) {
			continue
		}
		outage.LastProbeResult = "operator_cleared"
		outage.LastProbeDetail = "operator cleared the recorded outage"
		outage.NextProbeAt = time.Time{}
		delete(state.BackendOutages, key)
		if state.BackendRecoveries == nil {
			state.BackendRecoveries = map[string]BackendRecovery{}
		}
		state.BackendRecoveries[key] = BackendRecovery{Outage: outage, RecoveredAt: clearedAt}
		if !full {
			o.activateBackendDispatchRecovery(state, outage, clearedAt)
		}
		releaseBackendCapacityRetries(state, outage.Scope, clearedAt)
		cleared = append(cleared, outage)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      clearedAt,
			Event:   "backend_capacity_operator_cleared",
			Message: "operator cleared backend " + outage.Scope.BackendID + " capacity outage",
		})
		if o.logger != nil {
			telemetry.LogLifecycleMessage(o.logger, slog.LevelInfo, telemetry.LifecycleSafetyControl, "backend_capacity_operator_cleared", "operator cleared backend capacity outage", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
				"backend_id", outage.Scope.BackendID,
				"backend_kind", outage.Scope.BackendKind,
				"provider", outage.Scope.Provider,
				"cleared_at", clearedAt,
			)
		}
	}
	if full {
		for key, recovery := range state.DispatchRecoveries {
			if recovery.Kind == dispatchRecoveryBackendCapacity && backendCapacityScopeMatchesFilter(recovery.BackendScope, scopeFilter) {
				delete(state.DispatchRecoveries, key)
			}
		}
	}
	if o.logger != nil {
		mode := "ramping"
		if full {
			mode = "immediate"
		}
		o.logger.Info("backend capacity clear applied", "scope", scopeFilter, "recovery_requested", mode, "recovery_applied", mode, "cleared", len(cleared))
	}
	return cleared
}

func backendCapacityScopeMatchesFilter(scope backendcapacity.Scope, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	scope = scope.Normalize()
	return strings.EqualFold(filter, scope.BackendID) ||
		strings.EqualFold(filter, scope.BackendKind) ||
		strings.EqualFold(filter, scope.Provider) ||
		strings.EqualFold(filter, scope.BackendID+"/"+scope.Provider)
}

func backendOutagesCapacitySnapshot(outages map[string]BackendOutage) []map[string]any {
	rows := make([]map[string]any, 0, len(outages))
	for _, key := range sortedKeys(outages) {
		outage := outages[key]
		rows = append(rows, map[string]any{
			"backend_id":        outage.Scope.BackendID,
			"backend_kind":      outage.Scope.BackendKind,
			"provider":          outage.Scope.Provider,
			"kind":              outage.Kind,
			"reason":            outage.Reason,
			"trigger":           outage.Trigger,
			"detected_at":       outage.DetectedAt,
			"last_observed_at":  outage.LastObservedAt,
			"reset_at":          outage.ResetAt,
			"resume_at":         outage.ResumeAt,
			"next_probe_at":     outage.NextProbeAt,
			"last_probe_at":     outage.LastProbeAt,
			"last_probe_result": outage.LastProbeResult,
			"last_probe_detail": outage.LastProbeDetail,
			"probe_attempts":    outage.ProbeAttempts,
			"probe_issue_id":    outage.ProbeIssueID,
		})
	}
	return rows
}

func cloneBackendRecoveries(recoveries map[string]BackendRecovery) map[string]BackendRecovery {
	cloned := make(map[string]BackendRecovery, len(recoveries))
	for key, recovery := range recoveries {
		if recovery.SuppressedIssues != nil {
			recovery.SuppressedIssues = make(map[string]string, len(recovery.SuppressedIssues))
			for issueID, reason := range recoveries[key].SuppressedIssues {
				recovery.SuppressedIssues[issueID] = reason
			}
		}
		cloned[key] = recovery
	}
	return cloned
}
