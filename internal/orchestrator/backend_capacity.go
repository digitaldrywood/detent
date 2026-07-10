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
)

type BackendOutage struct {
	Scope          backendcapacity.Scope
	Kind           string
	Reason         string
	DetectedAt     time.Time
	LastObservedAt time.Time
	ResetAt        time.Time
	ResumeAt       time.Time
	ProbeIssueID   string
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
	outage := o.registerBackendOutage(state, capacityErr, event.CompletedAt)
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

func (o *Orchestrator) registerBackendOutage(state *State, capacityErr *backendcapacity.Error, observedAt time.Time) BackendOutage {
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
	existing.ProbeIssueID = ""
	existing.ResetAt = time.Time{}
	if capacityErr.Details.ResetAt != nil {
		existing.ResetAt = capacityErr.Details.ResetAt.UTC()
	}
	existing.ResumeAt = backendCapacityResumeAt(existing.ResetAt, observedAt)
	state.BackendOutages[key] = existing
	delete(state.BackendRecoveries, key)
	return existing
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
		message += " — resuming at " + outage.ResumeAt.UTC().Format(time.RFC3339)
	}
	return message
}

func (o *Orchestrator) scheduleBackendCapacityRetry(state *State, running Running, outage BackendOutage) {
	issue := cloneIssue(running.Issue)
	state.Retry[issue.ID] = Retry{
		Issue:      issue,
		Attempt:    running.Attempt,
		DueAt:      outage.ResumeAt,
		Error:      backendCapacityStatusMessage(outage),
		WorkerHost: running.WorkerHost,
	}
	claim, ok := state.Claimed[issue.ID]
	if !ok {
		claim.ClaimedAt = outage.LastObservedAt
	}
	claim.Issue = issue
	state.Claimed[issue.ID] = claim
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
	if strings.TrimSpace(outage.ProbeIssueID) != "" || now.Before(outage.ResumeAt) {
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
	if strings.TrimSpace(outage.ProbeIssueID) != "" || now.Before(outage.ResumeAt) {
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
		outage := o.registerBackendOutage(state, event.CapacityErr, event.CompletedAt)
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

func matchingBackendOutage(outages map[string]BackendOutage, scope backendcapacity.Scope) (string, BackendOutage, bool) {
	for key, outage := range outages {
		if outage.Scope.Matches(scope) {
			return key, outage, true
		}
	}
	return "", BackendOutage{}, false
}

func markBackendCapacityProbe(state *State, key string, issueID string) {
	if key == "" {
		return
	}
	outage, ok := state.BackendOutages[key]
	if !ok {
		return
	}
	outage.ProbeIssueID = strings.TrimSpace(issueID)
	state.BackendOutages[key] = outage
}

func (o *Orchestrator) recoverBackendCapacity(state *State, running Running, recoveredAt time.Time) {
	if !running.CapacityProbe {
		return
	}
	key, outage, ok := matchingBackendOutage(state.BackendOutages, running.CapacityScope)
	if !ok {
		return
	}
	delete(state.BackendOutages, key)
	if state.BackendRecoveries == nil {
		state.BackendRecoveries = map[string]BackendRecovery{}
	}
	state.BackendRecoveries[key] = BackendRecovery{Outage: outage, RecoveredAt: recoveredAt}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      recoveredAt,
		Event:   "backend_capacity_recovered",
		Message: "backend " + outage.Scope.BackendID + " capacity recovered",
	})
	if o.logger != nil {
		o.logger.Info(
			"backend capacity recovered",
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"detected_at", outage.DetectedAt,
			"recovered_at", recoveredAt,
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
	outage.ResumeAt = failedAt.Add(backendCapacityProbeDelay)
	state.BackendOutages[key] = outage
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      failedAt,
		Event:   "backend_capacity_probe_deferred",
		Message: "backend " + outage.Scope.BackendID + " capacity probe failed; retrying at " + outage.ResumeAt.Format(time.RFC3339),
	})
	if o.logger != nil {
		o.logger.Warn(
			"backend capacity probe deferred",
			"backend_id", outage.Scope.BackendID,
			"backend_kind", outage.Scope.BackendKind,
			"provider", outage.Scope.Provider,
			"retry_at", outage.ResumeAt,
			"error", probeErr,
		)
	}
}

func backendOutagesCapacitySnapshot(outages map[string]BackendOutage) []map[string]any {
	rows := make([]map[string]any, 0, len(outages))
	for _, key := range sortedKeys(outages) {
		outage := outages[key]
		rows = append(rows, map[string]any{
			"backend_id":       outage.Scope.BackendID,
			"backend_kind":     outage.Scope.BackendKind,
			"provider":         outage.Scope.Provider,
			"kind":             outage.Kind,
			"reason":           outage.Reason,
			"detected_at":      outage.DetectedAt,
			"last_observed_at": outage.LastObservedAt,
			"reset_at":         outage.ResetAt,
			"resume_at":        outage.ResumeAt,
			"probe_issue_id":   outage.ProbeIssueID,
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
			outage = o.registerBackendOutage(state, capacityErr, now)
			key, _, _ = matchingBackendOutage(state.BackendOutages, capacityErr.Scope)
			active = true
		}
		if active && now.Before(outage.ResumeAt) {
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
		if ok && capacityErr != nil {
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
