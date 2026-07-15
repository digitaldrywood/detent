package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	mergeWorkerRequiredChecksMissingReason = "required_checks_missing_after_head_update"
	mergeWorkerFastPathNotReadyReason      = "merge_fast_path_head_not_ready"
	mergeWorkerCITriggerLabelFailedReason  = "ci_trigger_label_reapply_failed"
)

func (o *Orchestrator) handleRunUpdate(state *State, event runUpdate) {
	running, ok := state.Running[event.issueID]
	if !ok {
		return
	}

	if event.usage.SessionID != "" {
		running.SessionID = event.usage.SessionID
	}
	if event.usage.DetentSessionID > 0 {
		running.DetentSessionID = event.usage.DetentSessionID
	}
	if !event.usage.RuntimeIdentity.IsZero() {
		running.RuntimeIdentity = running.RuntimeIdentity.Merge(event.usage.RuntimeIdentity)
	}
	if event.usage.TurnCount > 0 {
		running.TurnCount = event.usage.TurnCount
	}
	if !event.usage.LastEventAt.IsZero() {
		running.LastEventAt = event.usage.LastEventAt
	}
	if event.usage.LastEvent != "" {
		running.LastEvent = event.usage.LastEvent
	}
	if event.usage.LastMessage != "" {
		running.LastMessage = event.usage.LastMessage
		running.LastMessageTruncation = runtimeoutput.CloneTruncation(event.usage.LastMessageTruncation)
	}
	if len(event.usage.RecentEvents) > 0 {
		running.RecentEvents = cloneActivityEvents(event.usage.RecentEvents)
	}
	if event.usage.ProcessIdentity != "" {
		running.ProcessIdentity = event.usage.ProcessIdentity
	}
	if event.usage.WorkspacePath != "" {
		running.WorkspacePath = event.usage.WorkspacePath
	}
	if diffStatsPresent(event.usage.DiffStats) {
		running.DiffStats = event.usage.DiffStats
	}
	running.Tokens = event.usage.Tokens
	state.Running[event.issueID] = running
	if event.usage.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.usage.RateLimits)
		o.recoverBackendCapacityFromStatus(state, running, event.usage.RateLimits, event.usage.LastEventAt)
	}
	if o.workAttempts != nil && running.WorkAttemptID > 0 {
		now := event.usage.LastEventAt
		if now.IsZero() {
			now = time.Now()
		}
		heartbeat := o.runningWorkAttemptHeartbeat(state, running, now)
		if err := o.workAttempts.RecordWorkAttemptHeartbeat(context.Background(), heartbeat); err != nil {
			if o.logger != nil {
				o.logger.Warn("work attempt usage heartbeat failed", "attempt_id", running.WorkAttemptID, "issue_id", event.issueID, "error", err)
			}
		} else {
			o.applyWorkAttemptHeartbeatSnapshot(state, running.WorkAttemptID, heartbeat, event.usage.LastMessageTruncation)
		}
	}
}

func (o *Orchestrator) handleRunResult(ctx context.Context, state *State, event runpkg.Completion) {
	running, ok := state.Running[event.IssueID]
	if !ok {
		return
	}
	if o.retrospector != nil {
		defer o.retrospector.Trigger("completion")
	}
	if o.handleOperatorStopCompletion(ctx, state, event, running) {
		return
	}
	o.releaseGlobalDispatchSlot(running.globalSlot)
	o.logWorkerLifecycle(running.Issue, "worker_capacity_released",
		"attempt", running.Attempt,
		"worker_host", strings.TrimSpace(running.WorkerHost),
		"reason", "run_completed",
	)
	running.globalSlot = scheduler.Slot{}
	if !event.Result.RuntimeIdentity.IsZero() {
		running.RuntimeIdentity = running.RuntimeIdentity.Merge(event.Result.RuntimeIdentity)
	}
	if event.Result.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	}
	if running.cancel != nil {
		running.cancel()
	}
	delete(state.Running, event.IssueID)
	if o.handleGitHubRESTCapacityCompletion(ctx, state, event, running) {
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalFailure, event.Err, "github_rest_capacity", errorString(event.Err))
		return
	}
	if capacityErr, ok := backendcapacity.As(event.Err); ok {
		if capacityErr.Details.Type == backendcapacity.ErrorTypeTransientOverload {
			o.handleTransientOverload(ctx, state, event, running, capacityErr)
			return
		}
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalFailure, event.Err, "backend_capacity", errorString(event.Err))
		o.handleBackendCapacityError(ctx, state, event, running, capacityErr)
		return
	}
	if event.Err == nil || event.Result.TurnStarted {
		o.recoverBackendCapacity(state, running, event.CompletedAt)
	} else {
		o.deferBackendCapacityProbe(state, running, event.CompletedAt, event.Err)
	}

	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
		tokens := event.Result.Tokens
		if tokens == (TokenTotals{}) {
			tokens = running.Tokens
		}
		if diffStatsPresent(event.Result.DiffStats) {
			running.DiffStats = event.Result.DiffStats
		}
		o.logWorkerLifecycle(running.Issue, "worker_"+workerOutcome(event.Err, event.Result.FinalState),
			"attempt", running.Attempt,
			"worker_host", strings.TrimSpace(running.WorkerHost),
			"final_state", strings.TrimSpace(running.Issue.State),
		)
		o.completeTerminalRunning(context.Background(), state, event.IssueID, running, terminalCompletedAt(running.Issue, o.cfg.TerminalStates, event.CompletedAt), tokens)
		return
	}

	if event.Err != nil {
		o.logWorkerLifecycle(running.Issue, "worker_"+workerOutcome(event.Err, event.Result.FinalState),
			"attempt", running.Attempt,
			"worker_host", strings.TrimSpace(running.WorkerHost),
			"retry_attempt", event.RetryAttempt,
			"retry_delay_seconds", int64(event.RetryDelay/time.Second),
			"error", event.Err,
		)
		spendProgress := spendProgressDecision{}
		if !mergeWorkerIssue(running.Issue) {
			evidenceWarning := ""
			if implementProgressLinkedPullRequest(running.Issue) || event.Result.PullRequestUpdated {
				running.Issue, evidenceWarning = o.refreshSpendProgressIssue(ctx, running.Issue)
			}
			accepted, acceptedReason := dispatchAcceptedStateChange(running)
			spendProgress = o.evaluateSpendProgress(ctx, running, event.CompletedAt, accepted, acceptedReason)
			if evidenceWarning != "" {
				spendProgress.Warning = evidenceWarning
				spendProgress.Block = false
			}
		}
		terminalState := terminalStateForRun(event.Err, event.Result.FinalState)
		errorClass := workAttemptErrorRunner
		errorMessage := event.Err.Error()
		phase := "failed"
		statusMessage := "worker failed"
		if spendProgress.Block && !errors.Is(event.Err, runpkg.ErrSessionTokenCeilingExceeded) {
			terminalState = store.WorkAttemptTerminalNoProgress
			errorClass = spendProgressReason
			errorMessage = fmt.Sprintf("spent %s since the last accepted state change; configured limit %s", budget.FormatUSD(spendProgress.Spend.CostUSD), budget.FormatUSD(spendProgress.LimitUSD))
			phase = "no_progress"
			statusMessage = "spend-since-progress circuit breaker tripped"
		}
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, terminalState, event.Err, errorClass, errorMessage)
		o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, terminalState, errorClass, errorMessage, phase, statusMessage, spendProgressMetadata(spendProgress))
		attempt := event.RetryAttempt
		if attempt < 1 {
			attempt = nextAttempt(running.Attempt)
		}
		if o.tripTokenCeilingCircuitBreaker(ctx, state, event, running, attempt) {
			return
		}
		if spendProgress.Block && o.blockSpendProgress(ctx, state, running.Issue, spendProgress, event.CompletedAt) {
			return
		}
		if mergeWorkerIssue(running.Issue) {
			o.logMergeWorkerFailure(running.Issue, "runner_failed", event.Err)
			o.recordMergeFailed(state, running.Issue, event.CompletedAt, "runner_failed", event.Err)
		}
		if mergeWorkerIssue(running.Issue) && attempt > maxMergeWorkerRunnerFailures {
			if o.blockExhaustedMergeWorker(ctx, state, running, event.CompletedAt, attempt, event.Err) {
				return
			}
		}
		if o.tripInstantFailureCircuitBreaker(ctx, state, event, running, attempt) {
			return
		}
		if o.tripRepeatedFailureCircuitBreaker(ctx, state, event, running, attempt) {
			return
		}
		delay := event.RetryDelay
		if delay <= 0 {
			delay = o.retryDelay(attempt, false)
		}
		o.scheduleRetryAfter(
			state,
			running.Issue,
			attempt,
			event.CompletedAt,
			delay,
			event.Err.Error(),
			running.WorkerHost,
		)
		return
	}

	// Every path below is a completion without a worker error: reset both
	// failure circuit breakers here so plan and merge-worker completions do
	// not carry stale counts into the next attempt cycle.
	delete(state.InstantFailures, event.IssueID)
	delete(state.RepeatedFailures, event.IssueID)

	if event.Request.Mode == runpkg.RunModePlan {
		o.logWorkerLifecycle(running.Issue, "worker_"+workerOutcome(event.Err, event.Result.FinalState),
			"attempt", running.Attempt,
			"worker_host", strings.TrimSpace(running.WorkerHost),
			"mode", strings.TrimSpace(event.Request.Mode),
			"final_state", strings.TrimSpace(event.Result.FinalState),
		)
		o.completePlanRunning(ctx, state, event, running)
		return
	}

	if mergeWorkerIssue(running.Issue) {
		if o.completeLatestTerminalMergeWorkerResult(ctx, state, event, running) {
			return
		}
		if state.Draining {
			releaseProjectFailureBreakerCanary(state, event.IssueID)
			o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalCancelled, "draining", "worker stopped during drain", "cancelled", "worker stopped during drain")
			o.cleanupDrainedRun(ctx, state, event.IssueID)
			return
		}
		o.handleIncompleteMergeWorkerResult(ctx, state, event, running)
		return
	}
	if o.completeRedundantGateWaitRun(ctx, state, event, running) {
		releaseProjectFailureBreakerCanary(state, event.IssueID)
		return
	}

	finalState := event.Result.FinalState
	if finalState == "" {
		finalState = FinalStateCompleted
	}
	o.logWorkerLifecycle(running.Issue, "worker_"+workerOutcome(nil, finalState),
		"attempt", running.Attempt,
		"worker_host", strings.TrimSpace(running.WorkerHost),
		"mode", strings.TrimSpace(event.Request.Mode),
		"final_state", strings.TrimSpace(finalState),
	)
	terminalState := terminalStateForRun(nil, finalState)
	errorClass := ""
	errorMessage := ""
	if terminalState == store.WorkAttemptTerminalFailure {
		errorClass = "runner_final_state"
		errorMessage = finalState
	}
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
	}
	progress := o.evaluateImplementCompletionProgress(ctx, running, finalState, event.Result.PullRequestUpdated)
	running.Issue = progress.Issue
	accepted, acceptedReason := implementAcceptedStateChange(running, progress)
	spendProgress := o.evaluateSpendProgress(ctx, running, event.CompletedAt, accepted, acceptedReason)
	if progress.Warning != "" && strings.HasPrefix(progress.Reason, "pull_request_hydrat") {
		spendProgress.Warning = progress.Warning
		spendProgress.Block = false
	}
	if terminalState != store.WorkAttemptTerminalSuccess {
		progress.Outcome = terminalState
	}
	phase := "completed"
	statusMessage := "worker completed"
	if terminalState == store.WorkAttemptTerminalSuccess && progress.Outcome == store.WorkAttemptTerminalNoProgress {
		terminalState = store.WorkAttemptTerminalNoProgress
		phase = "no_progress"
		statusMessage = "worker completed without PR progress"
	}
	if spendProgress.Block {
		terminalState = store.WorkAttemptTerminalNoProgress
		progress.Outcome = store.WorkAttemptTerminalNoProgress
		errorClass = spendProgressReason
		errorMessage = fmt.Sprintf("spent %s since the last accepted state change; configured limit %s", budget.FormatUSD(spendProgress.Spend.CostUSD), budget.FormatUSD(spendProgress.LimitUSD))
		phase = "no_progress"
		statusMessage = "spend-since-progress circuit breaker tripped"
	}
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, terminalState, nil, errorClass, errorMessage)
	o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, terminalState, errorClass, errorMessage, phase, statusMessage, mergeWorkAttemptMetadata(implementCompletionProgressMetadata(progress), spendProgressMetadata(spendProgress)))

	state.Completed[event.IssueID] = Completed{
		Issue:           cloneIssue(running.Issue),
		SessionID:       running.SessionID,
		StartedAt:       running.StartedAt,
		CompletedAt:     event.CompletedAt,
		FinalState:      finalState,
		Tokens:          event.Result.Tokens,
		RuntimeIdentity: running.RuntimeIdentity,
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, event.Result.Tokens)
	if event.Result.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	}
	if diffStatsPresent(event.Result.DiffStats) {
		state.DiffStats[event.IssueID] = event.Result.DiffStats
	}
	if event.Result.BudgetRefusal != nil && !o.cfg.subscriptionBilling() {
		refusal := *event.Result.BudgetRefusal
		refusal.Issue = cloneIssue(running.Issue)
		state.BudgetRefusals[event.IssueID] = refusal
		o.commentBudgetRefusal(ctx, event.IssueID, refusal)
	}

	if state.Draining {
		o.cleanupDrainedRun(ctx, state, event.IssueID)
		return
	}
	if terminalState == store.WorkAttemptTerminalSuccess &&
		autoPromoteActiveGatePendingIssue(running.Issue, state, o.cfg, o.cfg.AutoPromote) {
		o.finishCompletedGateWaitRun(ctx, state, running.Issue)
		return
	}
	if spendProgress.Block && o.blockSpendProgress(ctx, state, running.Issue, spendProgress, event.CompletedAt) {
		return
	}
	if terminalState == store.WorkAttemptTerminalNoProgress && progress.Block {
		if o.blockImplementProgress(ctx, state, progress, event.CompletedAt) {
			return
		}
	}
	if event.Result.BudgetRefusal != nil && !o.cfg.subscriptionBilling() && event.Result.BudgetRefusal.Code == string(budget.ReasonPerIssueMaxUSD) {
		if err := o.abandonClaim(ctx, event.IssueID); err != nil && o.logger != nil {
			o.logger.Warn("per-issue budget hold claim release failed", "issue_id", event.IssueID, "error", err)
		}
		delete(state.Claimed, event.IssueID)
		delete(state.Retry, event.IssueID)
		delete(state.PriorAttempts, event.IssueID)
		return
	}
	o.scheduleRetry(state, running.Issue, 1, event.CompletedAt, "", true, running.WorkerHost)
}

func (o *Orchestrator) completeRedundantGateWaitRun(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	if !autoPromoteActiveGateTrackedIssue(running.Issue, o.cfg, o.cfg.AutoPromote) {
		return false
	}
	attempt, ok, err := o.latestSuccessfulGateWaitAttempt(ctx, running.Issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"gate-wait completion history lookup failed",
				"issue_id", running.Issue.ID,
				"identifier", running.Issue.Identifier,
				"error", err,
			)
		}
		return false
	}
	if !ok {
		return false
	}

	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalSuperseded,
		"awaiting_gate",
		"completed gate-wait work already has a successful attempt",
		"superseded",
		"ignored redundant gate-wait dispatch",
	)
	state.Completed[event.IssueID] = completedFromGateWaitAttempt(running.Issue, attempt)
	tokens := event.Result.Tokens
	if tokens == (TokenTotals{}) {
		tokens = running.Tokens
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, tokens)
	if event.Result.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	}
	if diffStatsPresent(event.Result.DiffStats) {
		state.DiffStats[event.IssueID] = event.Result.DiffStats
	}
	o.finishCompletedGateWaitRun(ctx, state, running.Issue)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "gate_wait_dispatch_superseded",
		Message: "ignored redundant dispatch for completed gate-wait " + issueLabel(running.Issue),
	})
	return true
}

func (o *Orchestrator) finishCompletedGateWaitRun(ctx context.Context, state *State, issue connector.Issue) {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("release completed gate-wait claim failed", "issue_id", issueID, "error", err)
	}
	o.releaseClaim(state, issueID)
}

func (o *Orchestrator) commentBudgetRefusal(ctx context.Context, issueID string, refusal BudgetRefusal) {
	if o.connector == nil {
		return
	}
	body := strings.TrimSpace(refusal.Comment)
	if body == "" {
		body = strings.TrimSpace(refusal.Message)
	}
	if body == "" {
		return
	}
	if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
		o.logger.Warn(
			"budget refusal comment failed",
			"issue_id", issueID,
			"identifier", refusal.Issue.Identifier,
			"code", refusal.Code,
			"error", err,
		)
	}
}

func (o *Orchestrator) tripInstantFailureCircuitBreaker(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	attempt int,
) bool {
	if state == nil || event.Err == nil || !instantFailureDuration(running, event) {
		delete(state.InstantFailures, event.IssueID)
		return false
	}
	if state.InstantFailures == nil {
		state.InstantFailures = map[string]InstantFailure{}
	}
	key := instantFailureErrorKey(event.Err)
	displayError := o.operatorText(key)
	if displayError == "" {
		displayError = o.operatorText(event.Err.Error())
	}
	failure := state.InstantFailures[event.IssueID]
	failureKey := failure.errorKey
	if failureKey == "" {
		failureKey = failure.Error
	}
	if failureKey != key {
		failure = InstantFailure{
			Issue:          cloneIssue(running.Issue),
			Error:          displayError,
			errorKey:       key,
			FirstFailureAt: event.CompletedAt,
		}
	}
	failure.Count++
	failure.Issue = cloneIssue(running.Issue)
	failure.LastFailureAt = event.CompletedAt
	state.InstantFailures[event.IssueID] = failure
	if failure.Count < instantFailureThreshold {
		return false
	}

	o.parkInstantFailure(ctx, state, event, running, failure, attempt)
	return true
}

func instantFailureDuration(running Running, event runpkg.Completion) bool {
	if !running.StartedAt.IsZero() && !event.CompletedAt.IsZero() {
		duration := event.CompletedAt.Sub(running.StartedAt)
		return duration >= 0 && duration < instantFailureMaxDuration
	}
	if event.Result.Tokens.RuntimeSeconds > 0 {
		return event.Result.Tokens.RuntimeSeconds < instantFailureMaxDuration.Seconds()
	}
	return false
}

func instantFailureErrorKey(err error) string {
	var carrier interface {
		BackendErrorBody() string
	}
	if errors.As(err, &carrier) {
		if body := strings.TrimSpace(carrier.BackendErrorBody()); body != "" {
			return body
		}
	}
	return strings.TrimSpace(err.Error())
}

func (o *Orchestrator) parkInstantFailure(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	failure InstantFailure,
	attempt int,
) {
	targetState := o.instantFailureParkState()
	issue := cloneIssue(running.Issue)
	if targetState != "" {
		if err := o.updateIssueState(ctx, state, issue, targetState, event.CompletedAt, "instant_fail_circuit_breaker"); err != nil {
			if o.logger != nil {
				o.logger.Error(
					"instant fail circuit breaker state transition failed",
					"issue_id", issue.ID,
					"identifier", issue.Identifier,
					"target_state", targetState,
					"error", err,
				)
			}
		} else {
			issue.State = targetState
		}
	}
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issue.ID, instantFailureComment(issue, event.Err, failure, attempt, targetState, o.cfg.OutputTruncationMaxBytes)); err != nil && o.logger != nil {
			o.logger.Error("instant fail circuit breaker comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issue.ID); err != nil && o.logger != nil {
		o.logger.Error("instant fail circuit breaker claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	delete(state.RepeatedFailures, issue.ID)
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issue.ID] = Blocked{
		Issue:          issue,
		Reason:         instantFailureBlockedReasonPrefix + failure.Error,
		RecoveryReason: "fix the pinned agent model or backend configuration, then move the issue back to Todo or Rework",
		RecoveryTarget: "Todo",
		BlockedAt:      event.CompletedAt,
		Source:         BlockedSourceProjectStatus,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "worker_instant_fail_circuit_breaker_tripped",
		Message: "parked " + issueLabel(issue) + " after repeated instant worker failures: " + failure.Error,
	})
	if o.logger != nil {
		attrs := []any{
			"event", "worker_instant_fail_circuit_breaker_tripped",
			"issue_id", issue.ID,
			"issue_identifier", issue.Identifier,
			"attempt", attempt,
			"instant_failures", failure.Count,
			"target_state", targetState,
			"error", event.Err,
		}
		if body := o.operatorText(instantFailureErrorKey(event.Err)); body != "" {
			attrs = append(attrs, "backend_error_body", body)
		}
		var carrier interface {
			BackendErrorMessage() string
		}
		if errors.As(event.Err, &carrier) {
			if message := o.operatorText(carrier.BackendErrorMessage()); message != "" {
				attrs = append(attrs, "backend_error_message", message)
			}
		}
		o.logger.Error("worker instant fail circuit breaker tripped", attrs...)
	}
}

func (o *Orchestrator) instantFailureParkState() string {
	return blockedStatusState
}

func (o *Orchestrator) tripTokenCeilingCircuitBreaker(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	attempt int,
) bool {
	var ceilingErr *runpkg.SessionTokenCeilingError
	if state == nil || !errors.As(event.Err, &ceilingErr) || ceilingErr == nil {
		return false
	}
	failure := o.recordRepeatedFailure(state, event, running)
	targetState := o.instantFailureParkState()
	issue := cloneIssue(running.Issue)
	if targetState != "" {
		if err := o.updateIssueState(ctx, state, issue, targetState, event.CompletedAt, "token_ceiling_circuit_breaker"); err != nil {
			if o.logger != nil {
				o.logger.Error(
					"token ceiling circuit breaker state transition failed",
					"issue_id", issue.ID,
					"identifier", issue.Identifier,
					"target_state", targetState,
					"error", err,
				)
			}
		} else {
			issue.State = targetState
		}
	}
	if o.connector != nil {
		comment := tokenCeilingFailureComment(issue, ceilingErr, running.Attempt, attempt, targetState)
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Error("token ceiling circuit breaker comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issue.ID); err != nil && o.logger != nil {
		o.logger.Error("token ceiling circuit breaker claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	delete(state.InstantFailures, issue.ID)
	delete(state.RepeatedFailures, issue.ID)
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	reason := tokenCeilingFailureReason(ceilingErr)
	state.Blocked[issue.ID] = Blocked{
		Issue:          issue,
		Reason:         reason,
		RecoveryReason: string(BlockedRecoveryReasonHumanBlocker),
		RecoveryTarget: "Rework",
		BlockedAt:      event.CompletedAt,
		Source:         BlockedSourceProjectStatus,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "worker_token_ceiling_circuit_breaker_tripped",
		Message: "parked " + issueLabel(issue) + " after a session token ceiling failure: " + reason,
	})
	if o.logger != nil {
		o.logger.Error(
			"worker token ceiling circuit breaker tripped",
			"event", "worker_token_ceiling_circuit_breaker_tripped",
			"issue_id", issue.ID,
			"issue_identifier", issue.Identifier,
			"failed_attempt", running.Attempt,
			"prevented_retry_attempt", attempt,
			"repeated_failures", failure.Count,
			"target_state", targetState,
			"observed_total_tokens", ceilingErr.TotalTokens,
			"ceiling_tokens", ceilingErr.CeilingTokens,
			"ceiling_source", ceilingErr.Source,
			"error", event.Err,
		)
	}
	return true
}

func tokenCeilingFailureReason(ceilingErr *runpkg.SessionTokenCeilingError) string {
	source := strings.TrimSpace(ceilingErr.Source)
	if source == "" {
		source = "unknown"
	}
	return fmt.Sprintf(
		"%sobserved %d tokens above the %d %s ceiling",
		tokenCeilingBlockedReasonPrefix,
		ceilingErr.TotalTokens,
		ceilingErr.CeilingTokens,
		source,
	)
}

func tokenCeilingFailureComment(
	issue connector.Issue,
	ceilingErr *runpkg.SessionTokenCeilingError,
	failedAttempt int,
	preventedRetryAttempt int,
	targetState string,
) string {
	var b strings.Builder
	b.WriteString("Detent stopped retrying this worker because the session exceeded its configured token ceiling.")
	if targetState = strings.TrimSpace(targetState); targetState != "" {
		b.WriteString("\n\nIssue parked in `")
		b.WriteString(targetState)
		b.WriteString("` for a human decision.")
	}
	b.WriteString("\n\n- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- failed_attempt: ")
	b.WriteString(strconv.Itoa(failedAttempt))
	b.WriteString("\n- prevented_retry_attempt: ")
	b.WriteString(strconv.Itoa(preventedRetryAttempt))
	b.WriteString("\n- observed_total_tokens: ")
	b.WriteString(strconv.FormatInt(ceilingErr.TotalTokens, 10))
	b.WriteString("\n- ceiling_tokens: ")
	b.WriteString(strconv.FormatInt(ceilingErr.CeilingTokens, 10))
	b.WriteString("\n- ceiling_source: ")
	b.WriteString(strings.TrimSpace(ceilingErr.Source))
	b.WriteString("\n\nChoose one recovery before moving the issue to Rework: split the issue into narrower work, apply the label configured by `agent.max_session_token_override_label` for a deliberate per-issue bypass, or raise `agent.max_session_tokens` (and `agent.max_session_context_multiplier` when it is the active guard).")
	return b.String()
}

// tripRepeatedFailureCircuitBreaker parks an issue after too many consecutive
// worker failures of any duration. The instant-failure breaker only counts
// sub-instantFailureMaxDuration failures with identical error text, which lets
// a long-running failure — one that spends minutes of paid agent time per
// attempt, like a session token ceiling hit — retry forever. This breaker
// counts every worker failure and resets only on a successful completion.
func (o *Orchestrator) tripRepeatedFailureCircuitBreaker(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	attempt int,
) bool {
	if state == nil || event.Err == nil {
		return false
	}
	failure := o.recordRepeatedFailure(state, event, running)
	if failure.Count < repeatedFailureThreshold {
		return false
	}

	o.parkRepeatedFailure(ctx, state, event, running, failure, attempt)
	return true
}

func (o *Orchestrator) recordRepeatedFailure(state *State, event runpkg.Completion, running Running) RepeatedFailure {
	if state.RepeatedFailures == nil {
		state.RepeatedFailures = map[string]RepeatedFailure{}
	}
	failure := state.RepeatedFailures[event.IssueID]
	if failure.Count == 0 {
		failure.FirstFailureAt = event.CompletedAt
	}
	failure.Count++
	failure.Issue = cloneIssue(running.Issue)
	failure.Error = o.operatorText(event.Err.Error())
	failure.LastFailureAt = event.CompletedAt
	state.RepeatedFailures[event.IssueID] = failure
	return failure
}

func (o *Orchestrator) parkRepeatedFailure(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	failure RepeatedFailure,
	attempt int,
) {
	targetState := o.instantFailureParkState()
	issue := cloneIssue(running.Issue)
	if targetState != "" {
		if err := o.updateIssueState(ctx, state, issue, targetState, event.CompletedAt, "repeated_failure_circuit_breaker"); err != nil {
			if o.logger != nil {
				o.logger.Error(
					"repeated failure circuit breaker state transition failed",
					"issue_id", issue.ID,
					"identifier", issue.Identifier,
					"target_state", targetState,
					"error", err,
				)
			}
		} else {
			issue.State = targetState
		}
	}
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issue.ID, repeatedFailureComment(issue, event.Err, failure, attempt, targetState, o.cfg.OutputTruncationMaxBytes)); err != nil && o.logger != nil {
			o.logger.Error("repeated failure circuit breaker comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issue.ID); err != nil && o.logger != nil {
		o.logger.Error("repeated failure circuit breaker claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	delete(state.InstantFailures, issue.ID)
	delete(state.RepeatedFailures, issue.ID)
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issue.ID] = Blocked{
		Issue:          issue,
		Reason:         repeatedFailureBlockedReasonPrefix + failure.Error,
		RecoveryReason: "fix the workflow or agent configuration causing every attempt to fail, then move the issue back to Todo or Rework",
		RecoveryTarget: "Todo",
		BlockedAt:      event.CompletedAt,
		Source:         BlockedSourceProjectStatus,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "worker_repeated_failure_circuit_breaker_tripped",
		Message: "parked " + issueLabel(issue) + " after repeated worker failures: " + failure.Error,
	})
	if o.logger != nil {
		o.logger.Error("worker repeated failure circuit breaker tripped",
			"event", "worker_repeated_failure_circuit_breaker_tripped",
			"issue_id", issue.ID,
			"issue_identifier", issue.Identifier,
			"attempt", attempt,
			"repeated_failures", failure.Count,
			"first_failure_at", failure.FirstFailureAt,
			"target_state", targetState,
			"error", event.Err,
		)
	}
}

func repeatedFailureComment(issue connector.Issue, err error, failure RepeatedFailure, attempt int, targetState string, maxBytes int) string {
	var b strings.Builder
	b.WriteString("Detent stopped retrying this worker after ")
	b.WriteString(strconv.Itoa(failure.Count))
	b.WriteString(" consecutive failed attempts. Each attempt ran to failure and spent real agent time, so retrying without a change is waste.")
	if targetState = strings.TrimSpace(targetState); targetState != "" {
		b.WriteString("\n\nIssue parked in `")
		b.WriteString(targetState)
		b.WriteString("`.")
	}
	b.WriteString("\n\n- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- latest_attempt: ")
	b.WriteString(strconv.Itoa(attempt))
	b.WriteString("\n- first_failure_at: ")
	b.WriteString(failure.FirstFailureAt.UTC().Format(time.RFC3339))
	b.WriteString("\n- last error:\n\n```text\n")
	b.WriteString(runtimeoutput.Truncate(strings.TrimSpace(err.Error()), maxBytes).Value)
	b.WriteString("\n```")
	b.WriteString("\n\nFix the workflow or agent configuration causing every attempt to fail, then move the issue back to Todo or Rework.")
	return b.String()
}

func instantFailureComment(issue connector.Issue, err error, failure InstantFailure, attempt int, targetState string, maxBytes int) string {
	var b strings.Builder
	b.WriteString("Detent stopped retrying this worker after ")
	b.WriteString(strconv.Itoa(failure.Count))
	b.WriteString(" consecutive instant failures with the same backend error.")
	if targetState = strings.TrimSpace(targetState); targetState != "" {
		b.WriteString("\n\nIssue parked in `")
		b.WriteString(targetState)
		b.WriteString("`.")
	}
	b.WriteString("\n\n- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- latest_attempt: ")
	b.WriteString(strconv.Itoa(attempt))
	b.WriteString("\n- failure_window_seconds: ")
	b.WriteString(strconv.FormatInt(int64(instantFailureMaxDuration/time.Second), 10))
	b.WriteString("\n- error:\n\n```text\n")
	errorText := runtimeoutput.Truncate(strings.TrimSpace(err.Error()), maxBytes).Value
	b.WriteString(errorText)
	b.WriteString("\n```")
	if body := runtimeoutput.Truncate(instantFailureErrorKey(err), maxBytes).Value; body != "" && body != errorText {
		b.WriteString("\n\n- backend_error_body:\n\n```json\n")
		b.WriteString(body)
		b.WriteString("\n```")
	}
	b.WriteString("\n\nFix the pinned agent model or backend configuration, then move the issue back to Todo or Rework.")
	return b.String()
}

func (o *Orchestrator) operatorText(value string) string {
	return runtimeoutput.Truncate(strings.TrimSpace(value), o.cfg.OutputTruncationMaxBytes).Value
}

func (o *Orchestrator) completeLatestTerminalMergeWorkerResult(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	issueID := strings.TrimSpace(event.IssueID)
	if issueID == "" || o.connector == nil {
		return false
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge_worker_terminal_state_refresh_failed",
				"issue_id", issueID,
				"identifier", running.Issue.Identifier,
				"error", err,
			)
		}
		return false
	}
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) != issueID {
			continue
		}
		issue = mergeIssueTrackerFields(running.Issue, issue)
		if !workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
			return o.completeProgrammaticMergeWorkerResult(ctx, state, event, running, issue)
		}
		tokens := event.Result.Tokens
		if tokens == (TokenTotals{}) {
			tokens = running.Tokens
		}
		if diffStatsPresent(event.Result.DiffStats) {
			running.DiffStats = event.Result.DiffStats
		}
		running.Issue = issue
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
		o.completeTerminalRunning(ctx, state, issueID, running, terminalCompletedAt(issue, o.cfg.TerminalStates, event.CompletedAt), tokens)
		if event.Result.RateLimits != nil {
			state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
		}
		return true
	}
	return false
}

func (o *Orchestrator) completeProgrammaticMergeWorkerResult(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	issue connector.Issue,
) bool {
	if state != nil && state.Draining {
		return false
	}
	if !mergeWorkerTurnSucceeded(event) {
		return false
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		return false
	}
	refreshedIssue, err := hydrator.HydratePullRequest(ctx, issue)
	if err != nil {
		running.Issue = issue
		o.failProgrammaticMergeWorkerResult(ctx, state, event, running, "programmatic_merge_pr_refresh_failed", err)
		return true
	}
	issue = refreshedIssue
	if !mergeWorkerProgrammaticMergeReady(issue) {
		if pullRequestHydrationBlocksProgress(issue.PullRequest) {
			o.waitForMergeWorkerPullRequestHydration(ctx, state, event, running, issue)
			return true
		}
		if missingChecks := mergeWorkerMissingRequiredChecks(issue); len(missingChecks) > 0 {
			reapplied, err := o.reapplyMergeWorkerCITriggerLabel(ctx, issue, missingChecks)
			if err != nil {
				running.Issue = issue
				o.failProgrammaticMergeWorkerResult(ctx, state, event, running, mergeWorkerCITriggerLabelFailedReason, err)
				return true
			}
			if reapplied || mergeWorkerMissingRequiredChecksPropagating(issue, running.Attempt) {
				o.waitForMergeWorkerRequiredCheckPropagation(ctx, state, event, running, issue)
				return true
			}
			o.reworkMergeWorkerResult(ctx, state, event, running, issue, mergeWorkerRequiredChecksMissingReason, missingChecks)
			return true
		}
		if mergeFastPathResult(event) && mergeWorkerProgrammaticMergeWaiting(issue) {
			o.waitForMergeWorkerCurrentHeadCI(ctx, state, event, running, issue)
			return true
		}
		if mergeFastPathResult(event) {
			o.reworkMergeWorkerResult(ctx, state, event, running, issue, mergeWorkerFastPathNotReadyReason, nil)
			return true
		}
		return false
	}
	merger, ok := o.connector.(connector.PullRequestMerger)
	if !ok {
		return false
	}
	issueID := strings.TrimSpace(event.IssueID)
	repository := pullRequestRepository(issue)
	number := pullRequestNumber(issue)
	headSHA := strings.TrimSpace(issue.PullRequest.HeadSHA)
	if err := merger.MergePullRequest(ctx, repository, number, headSHA, o.cfg.MergeMethod); err != nil {
		running.Issue = issue
		o.failProgrammaticMergeWorkerResult(ctx, state, event, running, "programmatic_merge_failed", err)
		return true
	}

	targetState := doneStateName(o.cfg.TerminalStates)
	mergedIssue := cloneIssue(issue)
	if mergedIssue.PullRequest != nil {
		mergedIssue.PullRequest.State = "MERGED"
		activityAt := event.CompletedAt.UTC()
		mergedIssue.PullRequest.ActivityAt = &activityAt
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, mergedIssue, targetState, event.CompletedAt, "merge_worker_programmatic_merge"); err != nil {
		running.Issue = mergedIssue
		o.failProgrammaticMergeWorkerResult(ctx, state, event, running, "programmatic_merge_state_update_failed", err)
		return true
	}

	updatedAt := event.CompletedAt.UTC()
	mergedIssue.State = targetState
	mergedIssue.UpdatedAt = &updatedAt
	mergedIssue.StageUpdatedAt = &updatedAt
	mergeTimingIssue := running.Issue
	running.Issue = mergedIssue
	tokens := event.Result.Tokens
	if tokens == (TokenTotals{}) {
		tokens = running.Tokens
	}
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
	}
	if o.logger != nil {
		o.logger.Info("merge_worker_programmatic_merge", mergeWorkerLogAttrs(mergedIssue, "target_state", targetState)...)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "merge_worker_programmatic_merge",
		Message: "programmatically merged " + issueLabel(mergedIssue) + " and moved it to " + targetState,
	})
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
	o.completeTerminalRunning(ctx, state, issueID, running, terminalCompletedAt(mergedIssue, o.cfg.TerminalStates, event.CompletedAt), tokens)
	mergeTiming := o.recordMergeCompleted(state, mergeTimingIssue, event.CompletedAt, targetState)
	if completed, ok := state.Completed[issueID]; ok {
		completed.MergeTiming = mergeTiming
		state.Completed[issueID] = completed
	}
	o.logMergeWorkerSuccess(mergeTimingIssue, targetState)
	if event.Result.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	}
	return true
}

func (o *Orchestrator) waitForMergeWorkerCurrentHeadCI(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	issue connector.Issue,
) {
	o.waitForMergeWorkerRetry(
		ctx,
		state,
		event,
		running,
		issue,
		running.Attempt,
		"waiting for current-head CI",
		"merge_worker_waiting_current_head_ci",
		"merge worker is waiting for current-head CI for ",
	)
}

func (o *Orchestrator) waitForMergeWorkerRequiredCheckPropagation(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	issue connector.Issue,
) {
	o.waitForMergeWorkerRetry(
		ctx,
		state,
		event,
		running,
		issue,
		nextAttempt(running.Attempt),
		"waiting for required checks to appear on the current head",
		"merge_worker_waiting_required_check_propagation",
		"merge worker is waiting for required checks to appear for ",
	)
}

func (o *Orchestrator) waitForMergeWorkerPullRequestHydration(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	issue connector.Issue,
) {
	o.waitForMergeWorkerRetry(
		ctx,
		state,
		event,
		running,
		issue,
		running.Attempt,
		"waiting for fresh pull request hydration",
		"merge_worker_waiting_pull_request_hydration",
		"merge worker is waiting for fresh pull request hydration for ",
	)
}

func (o *Orchestrator) waitForMergeWorkerRetry(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	issue connector.Issue,
	attempt int,
	retryError string,
	eventName string,
	eventMessage string,
) {
	running.Issue = issue
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "waiting", retryError)
	if attempt < 1 {
		attempt = 1
	}
	o.scheduleRetry(state, issue, attempt, event.CompletedAt, retryError, true, running.WorkerHost)
	if o.logger != nil {
		o.logger.Info(eventName, mergeWorkerLogAttrs(issue, "attempt", attempt)...)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   eventName,
		Message: eventMessage + issueLabel(issue),
	})
}

func (o *Orchestrator) failProgrammaticMergeWorkerResult(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	reason string,
	err error,
) {
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalFailure, err, reason, errorString(err))
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalFailure, reason, errorString(err), "merging", "programmatic merge failed")
	o.logMergeWorkerFailure(running.Issue, reason, err)
	o.recordMergeFailed(state, running.Issue, event.CompletedAt, reason, err)
	attempt := nextAttempt(running.Attempt)
	if attempt > maxMergeWorkerRunnerFailures {
		if o.blockExhaustedMergeWorker(ctx, state, running, event.CompletedAt, attempt, err) {
			return
		}
	}
	o.scheduleRetry(
		state,
		running.Issue,
		attempt,
		event.CompletedAt,
		errorString(err),
		false,
		running.WorkerHost,
	)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "merge_worker_programmatic_merge_failed",
		Message: "programmatic merge failed for " + issueLabel(running.Issue) + ": " + errorString(err),
	})
}

func mergeWorkerTurnSucceeded(event runpkg.Completion) bool {
	return event.Err == nil && !strings.EqualFold(strings.TrimSpace(event.Result.FinalState), runpkg.FinalStateFailed)
}

func mergeFastPathResult(event runpkg.Completion) bool {
	if event.Request.Mode != runpkg.RunModeMerge {
		return false
	}
	switch strings.TrimSpace(event.Result.Output) {
	case runpkg.RunOutputMergeFastPathClean, runpkg.RunOutputMergeFastPathCheckedHead:
		return true
	default:
		return false
	}
}

func mergeWorkerProgrammaticMergeReady(issue connector.Issue) bool {
	if strings.TrimSpace(issue.ID) == "" || issue.PullRequest == nil {
		return false
	}
	pullRequest := issue.PullRequest
	if pullRequestHydrationBlocksProgress(pullRequest) {
		return false
	}
	if normalizePullRequestState(pullRequest.State) != "open" || pullRequest.Draft {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(pullRequest.MergeableState)) {
	case "clean", "behind":
	default:
		return false
	}
	if !mergeWorkerCIGreen(pullRequest.CIStatus) {
		return false
	}
	return pullRequestRepository(issue) != "" &&
		pullRequestNumber(issue) > 0 &&
		strings.TrimSpace(pullRequest.HeadSHA) != ""
}

func mergeWorkerProgrammaticMergeWaiting(issue connector.Issue) bool {
	if strings.TrimSpace(issue.ID) == "" || issue.PullRequest == nil {
		return false
	}
	pullRequest := issue.PullRequest
	if pullRequestHydrationBlocksProgress(pullRequest) {
		return false
	}
	if normalizePullRequestState(pullRequest.State) != "open" || pullRequest.Draft {
		return false
	}
	mergeable := strings.ToLower(strings.TrimSpace(pullRequest.MergeableState))
	if mergeable != "" && mergeable != "clean" && mergeable != "unknown" && mergeable != "behind" && mergeable != "blocked" {
		return false
	}
	if mergeable == "blocked" && len(pullRequest.RunningChecks) == 0 {
		return false
	}
	if pullRequestRepository(issue) == "" || pullRequestNumber(issue) <= 0 || strings.TrimSpace(pullRequest.HeadSHA) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(pullRequest.CIStatus)) {
	case "", "pending", "running", "queued", "in_progress", "waiting":
		return true
	default:
		return false
	}
}

func mergeWorkerMissingRequiredChecks(issue connector.Issue) []string {
	if issue.PullRequest == nil {
		return nil
	}
	checks := make([]string, 0, len(issue.PullRequest.RequiredCheckFailures))
	seen := map[string]struct{}{}
	for _, check := range issue.PullRequest.RequiredCheckFailures {
		if !strings.EqualFold(strings.TrimSpace(check.Status), "missing") &&
			!strings.EqualFold(strings.TrimSpace(check.Conclusion), "missing") {
			continue
		}
		name := strings.TrimSpace(check.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		checks = append(checks, name)
	}
	return checks
}

func mergeWorkerMissingRequiredChecksPropagating(issue connector.Issue, attempt int) bool {
	if len(mergeWorkerMissingRequiredChecks(issue)) == 0 || attempt >= maxMergeWorkerRunnerFailures {
		return false
	}
	if issue.PullRequest == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(issue.PullRequest.CIStatus)) {
	case "", "pending", "running", "queued", "in_progress", "waiting":
		return true
	default:
		return false
	}
}

func (o *Orchestrator) reapplyMergeWorkerCITriggerLabel(ctx context.Context, issue connector.Issue, missingChecks []string) (bool, error) {
	cfg := gate.Effective(o.cfg.AutoPromote.Gate)
	label := strings.TrimSpace(cfg.CITriggerLabel)
	attrs := mergeWorkerLogAttrs(issue, "missing_required_checks", strings.Join(missingChecks, ","))
	if label == "" {
		if o.logger != nil {
			o.logger.Info("ci_trigger_label_skipped", append(attrs, "reason", "not_configured")...)
		}
		return false, nil
	}
	repository := pullRequestRepository(issue)
	number := pullRequestNumber(issue)
	headSHA := ""
	if issue.PullRequest != nil {
		headSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	}
	attrs = append(attrs, "label", label)
	if repository == "" || number <= 0 || headSHA == "" {
		if o.logger != nil {
			o.logger.Info("ci_trigger_label_skipped", append(attrs, "reason", "pull_request_identity_incomplete")...)
		}
		return false, nil
	}
	key := strings.ToLower(repository) + "#" + strconv.Itoa(number) + "|" + strings.ToLower(label)
	if o.ciTriggerLabelHeads == nil {
		o.ciTriggerLabelHeads = map[string]string{}
	}
	if o.ciTriggerLabelHeads[key] == headSHA {
		if o.logger != nil {
			o.logger.Info("ci_trigger_label_skipped", append(attrs, "reason", "already_reapplied_for_head")...)
		}
		return false, nil
	}
	reapplier, ok := o.connector.(connector.PullRequestLabelReapplier)
	if !ok {
		if o.logger != nil {
			o.logger.Info("ci_trigger_label_skipped", append(attrs, "reason", "connector_unsupported")...)
		}
		return false, nil
	}
	stagger := time.Duration(gate.DefaultCITriggerLabelStaggerSeconds) * time.Second
	if cfg.CITriggerLabelStaggerSeconds != nil {
		stagger = time.Duration(*cfg.CITriggerLabelStaggerSeconds) * time.Second
	}
	if err := reapplier.ReapplyPullRequestLabel(ctx, repository, number, label, stagger); err != nil {
		if o.logger != nil {
			o.logger.Error("ci_trigger_label_failed", append(attrs, "stagger", stagger, "error", err)...)
		}
		return false, fmt.Errorf("reapply CI trigger label %q to %s#%d: %w", label, repository, number, err)
	}
	o.ciTriggerLabelHeads[key] = headSHA
	if o.logger != nil {
		o.logger.Info("ci_trigger_label_reapplied", append(attrs, "stagger", stagger)...)
	}
	return true, nil
}

func (o *Orchestrator) reworkMergeWorkerResult(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	issue connector.Issue,
	reason string,
	missingChecks []string,
) {
	issueID := strings.TrimSpace(event.IssueID)
	running.Issue = issue
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, autoPromoteReworkState, event.CompletedAt, reason); err != nil {
		o.failProgrammaticMergeWorkerResult(ctx, state, event, running, "merge_worker_rework_failed", err)
		return
	}
	if comment := mergeWorkerReworkComment(issue, reason, missingChecks); comment != "" {
		if err := o.connector.CreateComment(ctx, issueID, comment); err != nil && o.logger != nil {
			o.logger.Warn("merge worker rework comment failed", "issue_id", issueID, "reason", reason, "error", err)
		}
	}
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "rework", "merge worker routed current head to Rework")
	o.logMergeWorkerFailure(issue, reason, nil)
	o.recordMergeFailed(state, issue, event.CompletedAt, reason, nil)
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("abandon merge worker rework claim failed", "issue_id", issueID, "error", err)
	}
	o.clearAutoPromotedIssueDispatchMemory(state, issueID)
	if state.PriorAttempts == nil {
		state.PriorAttempts = map[string]runpkg.PriorAttempt{}
	}
	state.PriorAttempts[issueID] = runpkg.PriorAttempt{Source: "merge_worker", Reason: reason}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "merge_worker_routed_to_rework",
		Message: "routed merge worker to Rework for " + issueLabel(issue) + ": " + reason,
	})
}

func mergeWorkerReworkComment(issue connector.Issue, reason string, missingChecks []string) string {
	var b strings.Builder
	b.WriteString("Merge worker routed this issue from Merging to Rework.")
	b.WriteString("\n\n- reason: ")
	b.WriteString(reason)
	if len(missingChecks) > 0 {
		b.WriteString("\n- missing_required_checks: ")
		b.WriteString(strings.Join(missingChecks, ", "))
	}
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			b.WriteString("\n- pull request: ")
			b.WriteString(url)
		}
		if headSHA := strings.TrimSpace(issue.PullRequest.HeadSHA); headSHA != "" {
			b.WriteString("\n- head_sha: ")
			b.WriteString(headSHA)
		}
		if mergeableState := strings.TrimSpace(issue.PullRequest.MergeableState); mergeableState != "" {
			b.WriteString("\n- mergeable_state: ")
			b.WriteString(mergeableState)
		}
	}
	b.WriteString("\n\nRefresh or re-push the current PR head so required checks run, then complete the normal Rework gate.")
	return b.String()
}

func mergeWorkerCIGreen(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "green", "pass", "passed":
		return true
	default:
		return false
	}
}

func (o *Orchestrator) handleIncompleteMergeWorkerResult(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) {
	err := errors.New(mergeWorkerTerminalStateMissing)
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalFailure, err, workAttemptErrorMergeIncomplete, err.Error())
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalFailure, workAttemptErrorMergeIncomplete, err.Error(), "merging", "merge worker completed without terminal state")
	o.logMergeWorkerFailure(running.Issue, "terminal_state_missing", err)
	o.recordMergeFailed(state, running.Issue, event.CompletedAt, "terminal_state_missing", err)
	attempt := nextAttempt(running.Attempt)
	if attempt > maxMergeWorkerRunnerFailures {
		if o.blockExhaustedMergeWorker(ctx, state, running, event.CompletedAt, attempt, err) {
			return
		}
	}
	o.scheduleRetry(
		state,
		running.Issue,
		attempt,
		event.CompletedAt,
		err.Error(),
		false,
		running.WorkerHost,
	)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "merge_worker_terminal_state_missing",
		Message: "merge worker completed without terminal state for " + issueLabel(running.Issue),
	})
}

func (o *Orchestrator) blockExhaustedMergeWorker(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
	attempt int,
	err error,
) bool {
	issueID := strings.TrimSpace(running.Issue.ID)
	if issueID == "" || o.connector == nil {
		return false
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, running.Issue, blockedStatusState, completedAt, mergeWorkerRetryExhaustedReason); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge_worker_block_failed",
				"issue_id", issueID,
				"identifier", running.Issue.Identifier,
				"reason", mergeWorkerRetryExhaustedReason,
				"target_state", blockedStatusState,
				"error", err,
			)
		}
		return false
	}
	if comment := mergeWorkerRetryExhaustedComment(running.Issue, attempt, err); strings.TrimSpace(comment) != "" {
		if err := o.connector.CreateComment(ctx, issueID, comment); err != nil && o.logger != nil {
			o.logger.Warn(
				"merge_worker_block_comment_failed",
				"issue_id", issueID,
				"identifier", running.Issue.Identifier,
				"reason", mergeWorkerRetryExhaustedReason,
				"error", err,
			)
		}
	}
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("abandon exhausted merge worker claim failed", "issue_id", issueID, "error", err)
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	delete(state.Completed, issueID)
	issue := cloneIssue(running.Issue)
	issue.State = blockedStatusState
	issue.BlockerReason = mergeWorkerRetryExhaustedReason + ": " + errorString(err)
	blockedAt := completedAt.UTC()
	issue.StageUpdatedAt = &blockedAt
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issueID] = Blocked{
		Issue:     issue,
		Reason:    mergeWorkerRetryExhaustedReason,
		BlockedAt: completedAt,
		Source:    BlockedSourceProjectStatus,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   "merge_worker_retry_exhausted",
		Message: "merge worker retries exhausted for " + issueLabel(running.Issue) + ": " + errorString(err),
	})
	return true
}

func mergeWorkerRetryExhaustedComment(issue connector.Issue, attempt int, err error) string {
	var b strings.Builder
	b.WriteString("Merge worker retries were exhausted; parked this issue in Blocked to stop automatic redispatch.")
	b.WriteString("\n\n- reason: ")
	b.WriteString(mergeWorkerRetryExhaustedReason)
	if attempt > 0 {
		b.WriteString("\n- attempt: ")
		b.WriteString(strconv.Itoa(attempt))
	}
	if errText := errorString(err); errText != "" {
		b.WriteString("\n- error: ")
		b.WriteString(errText)
	}
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			b.WriteString("\n- pull request: ")
			b.WriteString(url)
		}
		if mergeableState := strings.ToLower(strings.TrimSpace(issue.PullRequest.MergeableState)); mergeableState != "" {
			b.WriteString("\n- mergeable_state: ")
			b.WriteString(mergeableState)
		}
		if ciStatus := strings.TrimSpace(issue.PullRequest.CIStatus); ciStatus != "" {
			b.WriteString("\n- ci_status: ")
			b.WriteString(ciStatus)
		}
	}
	b.WriteString("\n\nResolve the merge failure, then move the issue back to Merging to retry.")
	return b.String()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func (o *Orchestrator) completePlanRunning(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) {
	cfg := gate.EffectivePlan(o.cfg.Plan)
	issueID := strings.TrimSpace(event.IssueID)
	issue := cloneIssue(running.Issue)
	body := planArtifactComment(issue, event.Result.Output)
	if err := o.connector.CreateComment(ctx, issueID, body); err != nil {
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalFailure, err, "plan_comment_failed", err.Error())
		o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalFailure, "plan_comment_failed", err.Error(), "reviewing", "plan comment failed")
		o.scheduleRetry(state, issue, nextAttempt(running.Attempt), event.CompletedAt, "plan comment failed: "+err.Error(), false, running.WorkerHost)
		return
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, cfg.Stop, event.CompletedAt, "plan_artifact_created"); err != nil {
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalFailure, err, "plan_transition_failed", err.Error())
		o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalFailure, "plan_transition_failed", err.Error(), "reviewing", "plan review transition failed")
		o.scheduleRetry(state, issue, nextAttempt(running.Attempt), event.CompletedAt, "plan review transition failed: "+err.Error(), false, running.WorkerHost)
		return
	}
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "plan review created")
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("abandon completed plan claim failed", "issue_id", issueID, "error", err)
	}
	delete(state.planRework, issueID)
	issue.State = cfg.Stop
	state.Completed[issueID] = Completed{
		Issue:           issue,
		SessionID:       running.SessionID,
		StartedAt:       running.StartedAt,
		CompletedAt:     event.CompletedAt,
		FinalState:      cfg.Stop,
		Tokens:          event.Result.Tokens,
		RuntimeIdentity: running.RuntimeIdentity,
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, event.Result.Tokens)
	if event.Result.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	}
	if diffStatsPresent(event.Result.DiffStats) {
		state.DiffStats[issueID] = event.Result.DiffStats
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "plan_review_created",
		Message: "created plan artifact for " + issueLabel(issue) + " and moved to " + cfg.Stop,
	})
}

func (o *Orchestrator) scheduleRetry(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	err string,
	continuation bool,
	workerHost string,
) {
	o.dispatchPlanner().scheduleRetry(state, issue, attempt, now, err, continuation, workerHost)
}

func (o *Orchestrator) scheduleRetryAfter(
	state *State,
	issue connector.Issue,
	attempt int,
	now time.Time,
	delay time.Duration,
	err string,
	workerHost string,
) {
	o.dispatchPlanner().scheduleRetryAfter(state, issue, attempt, now, delay, err, workerHost)
}

func (o *Orchestrator) retryDelay(attempt int, continuation bool) time.Duration {
	return o.dispatchPlanner().retryDelay(attempt, continuation)
}

func (o *Orchestrator) releaseClaim(state *State, issueID string) {
	o.cancelRunning(state, issueID)
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
}

func (o *Orchestrator) completeTerminalRunning(
	ctx context.Context,
	state *State,
	issueID string,
	running Running,
	completedAt time.Time,
	tokens TokenTotals,
) {
	o.completeDurableWorkAttempt(ctx, state, running, completedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "worker reached terminal state")
	o.releaseGlobalDispatchSlot(running.globalSlot)
	if running.cancel != nil {
		running.cancel()
	}
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	delete(state.InstantFailures, issueID)
	delete(state.RepeatedFailures, issueID)
	if err := o.abandonClaim(ctx, issueID); err != nil {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      cleanupEventAt(completedAt),
			Event:   "claim_release_failed",
			Message: fmt.Sprintf("claim lease release failed for %s: %v", issueLabel(running.Issue), err),
		})
	}
	issue := o.ensureClosedCompletedRunningIssueDone(ctx, state, issueID, running.Issue, completedAt)
	finalState := strings.TrimSpace(issue.State)
	if finalState == "" {
		finalState = FinalStateCompleted
	}
	mergeTiming := MergeTiming{}
	if mergeWorkerIssue(running.Issue) {
		mergeTiming = o.recordMergeCompleted(state, running.Issue, completedAt, finalState)
	}
	o.recordEfficiencyReceipt(ctx, issue, completedAt)
	state.Completed[issueID] = Completed{
		Issue:           cloneIssue(issue),
		SessionID:       running.SessionID,
		StartedAt:       running.StartedAt,
		CompletedAt:     completedAt,
		FinalState:      finalState,
		Tokens:          tokens,
		MergeTiming:     mergeTiming,
		RuntimeIdentity: running.RuntimeIdentity,
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, tokens)
	if diffStatsPresent(running.DiffStats) {
		state.DiffStats[issueID] = running.DiffStats
	}
	if mergeWorkerIssue(running.Issue) {
		o.logMergeWorkerSuccess(running.Issue, finalState)
	}
	o.reapWorkspace(ctx, state, issue, workspaceReapReason(issue, o.cfg.TerminalStates), completedAt)
}

func (o *Orchestrator) recordEfficiencyReceipt(ctx context.Context, issue connector.Issue, completedAt time.Time) {
	if o.efficiency == nil || normalizeState(issue.State) != normalizeState(doneStateName(o.cfg.TerminalStates)) {
		return
	}
	receipt, err := o.efficiency.CompleteEfficiencyReceipt(ctx, efficiency.Completion{
		ProjectID:   o.workflowMetricsProjectID(),
		IssueID:     issue.ID,
		Identifier:  issue.Identifier,
		IssueURL:    issue.URL,
		PRNumber:    workflowMetricsPRNumber(issue),
		CompletedAt: completedAt,
		Thresholds:  o.cfg.EfficiencyThresholds,
	})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("record efficiency receipt failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return
	}
	if o.lifecycleExporter == nil {
		return
	}
	if err := o.lifecycleExporter.ExportLifecycle(ctx, receipt); err != nil && o.logger != nil {
		o.logger.Warn("export efficiency lifecycle failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
}

func (o *Orchestrator) ensureClosedCompletedRunningIssueDone(ctx context.Context, state *State, issueID string, issue connector.Issue, now time.Time) connector.Issue {
	if !issue.Closed || !closedReasonCompleted(issue.ClosedReason) {
		return issue
	}
	targetState := doneStateName(o.cfg.TerminalStates)
	if strings.TrimSpace(targetState) == "" {
		return issue
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "closed_completed_running_done"); err != nil {
		if o.logger != nil {
			o.logger.Warn("mark closed completed running issue done failed", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "error", err)
		}
		return issue
	}
	if o.logger != nil {
		o.logger.Info("marked closed completed running issue done", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState)
	}
	issue.State = targetState
	return issue
}

func terminalCompletedAt(issue connector.Issue, terminalStates []string, fallback time.Time) time.Time {
	if stateIn(issue.State, terminalStates) && issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		return *issue.StageUpdatedAt
	}
	if issue.UpdatedAt != nil && !issue.UpdatedAt.IsZero() {
		return *issue.UpdatedAt
	}
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		return *issue.StageUpdatedAt
	}
	if !fallback.IsZero() {
		return fallback
	}
	return time.Now().UTC()
}

func (o *Orchestrator) cancelRunning(state *State, issueID string) {
	running, ok := state.Running[issueID]
	if !ok {
		return
	}
	o.releaseGlobalDispatchSlot(running.globalSlot)
	running.globalSlot = scheduler.Slot{}
	state.Running[issueID] = running
	cancelRunning(state, issueID)
}

func cancelRunning(state *State, issueID string) {
	running, ok := state.Running[issueID]
	if !ok || running.cancel == nil {
		return
	}
	running.cancel()
	running.cancel = nil
	state.Running[issueID] = running
}

func (o *Orchestrator) releaseRunningSlots(state *State) {
	for issueID, running := range state.Running {
		o.releaseGlobalDispatchSlot(running.globalSlot)
		running.globalSlot = scheduler.Slot{}
		state.Running[issueID] = running
	}
}
