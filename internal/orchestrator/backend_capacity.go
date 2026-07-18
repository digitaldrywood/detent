package orchestrator

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
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
	Scope           backendcapacity.Scope
	Kind            string
	Reason          string
	DetectedAt      time.Time
	LastObservedAt  time.Time
	ResetAt         time.Time
	ResumeAt        time.Time
	NextProbeAt     time.Time
	LastProbeAt     time.Time
	LastProbeResult string
	LastProbeDetail string
	ProbeAttempts   int
	ProbeIssueID    string
}

type BackendRecovery struct {
	Outage      BackendOutage
	RecoveredAt time.Time
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
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		backendcapacity.ErrorClass,
		event.Err.Error(),
		"waiting",
		backendCapacityStatusMessage(outage),
	)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		o.releaseClaim(state, running.Issue.ID)
		return
	}
	running.Issue, _ = o.demoteTerminalAttemptRetry(ctx, state, running.Issue, running.WorkProductPushed, event.CompletedAt)
	o.scheduleBackendCapacityRetry(state, running, outage)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "backend_capacity_paused",
		Message: backendCapacityStatusMessage(outage),
	})
	if o.logger != nil {
		o.logger.Warn(
			"backend capacity paused",
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"reset_at", outage.ResetAt,
			"resume_at", outage.ResumeAt,
			"issue_id", running.Issue.ID,
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
	if capacityErr == nil || capacityErr.Details.Type == backendcapacity.ErrorTypeTransientOverload {
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
		delay := backendCapacityProbeDelayForAttempt(existing.ProbeAttempts)
		existing.NextProbeAt = backendCapacityBoundedProbeAt(existing.ResumeAt, observedAt.Add(delay), observedAt)
	}
	state.BackendOutages[key] = existing
	delete(state.BackendRecoveries, key)
	return existing
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
	message := "backend " + backend + " at usage limit"
	if !outage.ResumeAt.IsZero() {
		message += " — provider-recorded resume at " + outage.ResumeAt.UTC().Format(time.RFC3339)
	}
	if !outage.NextProbeAt.IsZero() {
		message += "; next probe at " + outage.NextProbeAt.UTC().Format(time.RFC3339)
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
	if !ok {
		return scope, "", false
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
	if !ok {
		return scope, "", false
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
			o.logger.Warn(
				"validator backend capacity paused",
				"backend_id", outage.Scope.BackendID,
				"provider", outage.Scope.Provider,
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
		o.logger.Info(
			"backend capacity probe started",
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"issue_id", strings.TrimSpace(issueID),
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
	if !ok {
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
	if o.capacityStatus == nil || rateLimits == nil || !running.CapacityScope.Hosted() {
		return
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, running.CapacityScope)
	if !ok {
		return
	}
	status, ok := o.capacityStatus.BackendCapacityStatus(running.CapacityScope, rateLimits)
	if !ok {
		return
	}
	if observedAt.IsZero() {
		observedAt = o.clockNow()
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
	o.activateDispatchRecovery(
		state,
		dispatchRecoveryBackendCapacity,
		backendCapacityStatusMessage(outage),
		recoveredAt,
		"",
	)
	releaseBackendCapacityRetries(state, outage.Scope, recoveredAt)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      recoveredAt,
		Event:   "backend_capacity_recovered",
		Message: "backend " + outage.Scope.BackendID + " capacity recovered via " + source,
	})
	if o.logger != nil {
		o.logger.Info(
			"backend capacity recovered",
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
		o.logger.Warn(
			"backend capacity probe deferred",
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

func (o *Orchestrator) clearBackendCapacity(state *State, scopeFilter string, clearedAt time.Time) []BackendOutage {
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
		o.activateDispatchRecovery(
			state,
			dispatchRecoveryBackendCapacity,
			backendCapacityStatusMessage(outage),
			clearedAt,
			"",
		)
		releaseBackendCapacityRetries(state, outage.Scope, clearedAt)
		cleared = append(cleared, outage)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      clearedAt,
			Event:   "backend_capacity_operator_cleared",
			Message: "operator cleared backend " + outage.Scope.BackendID + " capacity outage",
		})
		if o.logger != nil {
			o.logger.Info(
				"operator cleared backend capacity outage",
				"backend_id", outage.Scope.BackendID,
				"backend_kind", outage.Scope.BackendKind,
				"provider", outage.Scope.Provider,
				"cleared_at", clearedAt,
			)
		}
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

func backendCapacityRecoveryTarget(issue connector.Issue) string {
	if issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) == "open" {
		return autoPromoteReworkState
	}
	return "Todo"
}

func IsLegacyFailureBreakerComment(body string) bool {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "Detent stopped retrying this worker after ") {
		return false
	}
	return strings.Contains(body, " consecutive instant failures with the same backend error.") ||
		strings.Contains(body, " consecutive failed attempts.")
}

func (o *Orchestrator) recoverBackendCapacityBlockedIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	if o.capacityController == nil {
		return nil
	}
	transitioned := map[string]struct{}{}
	consumedRecoveries := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, []string{blockedStatusState}) {
		capacityErr, hydratedIssue, ok := o.classifyBlockedCapacityIssue(ctx, state, issue, now)
		if !ok {
			continue
		}
		key, outage, active := matchingBackendOutage(state.BackendOutages, capacityErr.Scope)
		recoveryKey, recovery, recovered := matchingBackendRecovery(state.BackendRecoveries, capacityErr.Scope)
		if !active && !recovered {
			outage = o.registerBackendOutage(state, capacityErr, now, false)
			key, _, _ = matchingBackendOutage(state.BackendOutages, capacityErr.Scope)
			active = true
		}
		if active {
			continue
		}
		if !o.applyBackendCapacityBlockedRecovery(ctx, state, hydratedIssue, outage, recovery, now) {
			continue
		}
		transitioned[hydratedIssue.ID] = struct{}{}
		if recovered {
			consumedRecoveries[recoveryKey] = struct{}{}
		}
		if active && key != "" {
			state.BackendOutages[key] = outage
		}
	}
	for key := range consumedRecoveries {
		delete(state.BackendRecoveries, key)
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func (o *Orchestrator) classifyBlockedCapacityIssue(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	now time.Time,
) (*backendcapacity.Error, connector.Issue, bool) {
	issue = cloneIssue(issue)
	if len(issue.Comments) == 0 {
		reader, ok := o.connector.(connector.IssueCommentReader)
		if !ok {
			return nil, issue, false
		}
		comments, err := reader.FetchIssueComments(ctx, issue)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("capacity blocked comment hydration failed", "issue_id", issue.ID, "error", err)
			}
			return nil, issue, false
		}
		issue.Comments = comments
	}
	request := runpkg.RunRequest{
		Issue:           issue,
		Mode:            runpkg.RunModeImplement,
		SelectorContext: o.selectorContext(),
	}
	for index := len(issue.Comments) - 1; index >= 0; index-- {
		body := strings.TrimSpace(issue.Comments[index].Body)
		if !IsLegacyFailureBreakerComment(body) {
			continue
		}
		capacityErr, ok := o.capacityController.ClassifyCapacityError(request, errors.New(body), state.RateLimits, now)
		if ok && capacityErr != nil && capacityErr.Details.Type != backendcapacity.ErrorTypeTransientOverload {
			return capacityErr, issue, true
		}
	}
	return nil, issue, false
}

func matchingBackendRecovery(
	recoveries map[string]BackendRecovery,
	scope backendcapacity.Scope,
) (string, BackendRecovery, bool) {
	for key, recovery := range recoveries {
		if recovery.Outage.Scope.Matches(scope) {
			return key, recovery, true
		}
	}
	return "", BackendRecovery{}, false
}

func (o *Orchestrator) applyBackendCapacityBlockedRecovery(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	outage BackendOutage,
	recovery BackendRecovery,
	now time.Time,
) bool {
	targetState := backendCapacityRecoveryTarget(issue)
	if err := o.updateIssueState(ctx, state, issue, targetState, now, "backend_capacity_recovered"); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"backend capacity blocked recovery failed",
				"issue_id", issue.ID,
				"target_state", targetState,
				"error", err,
			)
		}
		return false
	}
	if outage.Scope.BackendID == "" {
		outage = recovery.Outage
	}
	if o.connector != nil {
		body := backendCapacityRecoveryComment(issue, targetState, outage, recovery, now)
		if err := o.connector.CreateComment(ctx, issue.ID, body); err != nil && o.logger != nil {
			o.logger.Warn("backend capacity recovery comment failed", "issue_id", issue.ID, "error", err)
		}
	}
	delete(state.Blocked, issue.ID)
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.InstantFailures, issue.ID)
	delete(state.RepeatedFailures, issue.ID)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "backend_capacity_blocked_issue_recovered",
		Message: "recovered " + issueLabel(issue) + " from Blocked to " + targetState,
	})
	return true
}

func backendCapacityRecoveryComment(
	issue connector.Issue,
	targetState string,
	outage BackendOutage,
	recovery BackendRecovery,
	now time.Time,
) string {
	recoveredAt := recovery.RecoveredAt
	if recoveredAt.IsZero() {
		recoveredAt = now
	}
	var b strings.Builder
	b.WriteString("Detent recovered this issue after provider capacity returned.")
	b.WriteString("\n\n- reason: backend_capacity_recovered")
	b.WriteString("\n- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- backend_id: ")
	b.WriteString(outage.Scope.BackendID)
	if outage.Scope.Provider != "" {
		b.WriteString("\n- provider: ")
		b.WriteString(outage.Scope.Provider)
	}
	if !outage.DetectedAt.IsZero() {
		b.WriteString("\n- outage_started_at: ")
		b.WriteString(outage.DetectedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n- recovered_at: ")
	b.WriteString(recoveredAt.UTC().Format(time.RFC3339))
	b.WriteString("\n- target_state: ")
	b.WriteString(targetState)
	return b.String()
}
