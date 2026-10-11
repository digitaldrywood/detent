package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (o *Orchestrator) completeNativeChangeRun(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	finalState string,
) bool {
	change := event.Result.NativeChange
	if change != nil && change.Landing != nil {
		event.Result.NativeLanding = change.Landing
		return o.completeNativeLandingRun(ctx, state, event, running)
	}
	if event.Result.NativeLanding != nil {
		return false
	}
	reader, ok := o.connector.(connector.WorkflowStateReader)
	if !ok {
		return false
	}
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
	}
	if o.handlePermissionWaitCompletion(ctx, state, event, running) {
		return true
	}
	mode := firstNonBlank(event.Request.Mode, running.Mode)
	recoveryRequired := errors.Is(event.Err, runpkg.ErrNativeRecoveryRequired)
	preProvider := !event.Result.TurnStarted && running.TurnCount == 0 && !running.WorkProductPushed && running.Tokens.TotalTokens == 0 && event.Result.Tokens.TotalTokens == 0
	recoveryRefusal := recoveryRequired && preProvider && running.Cancellation == nil && preTurnFailureClass(event, running) == "" && !errors.Is(event.Err, context.Canceled) && !errors.Is(event.Err, context.DeadlineExceeded)
	if mergeWorkerIssue(running.Issue) || mode != "" && mode != runpkg.RunModeImplement && (mode != runpkg.RunModePlan || !recoveryRefusal) {
		return false
	}
	if finalState == "" {
		finalState = FinalStateCompleted
	}
	issue := running.Issue
	issueID := strings.TrimSpace(event.IssueID)
	handoff := func(err error) bool {
		o.warnNativeCompletion(issue, err)
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
		return true
	}
	if errors.Is(event.Err, runpkg.ErrExecutionAuthorityUnavailable) {
		return handoff(event.Err)
	}
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	humanReview := cfg.humanReviewEnabled() || autoPromoteOptoutLabel(issue, cfg)
	if event.Err != nil || terminalStateForRun(nil, finalState) != store.WorkAttemptTerminalSuccess {
		if preProvider && !recoveryRequired && o.handlePreTurnFailure(ctx, state, event, running) {
			return true
		}
		states, err := reader.WorkflowStates(ctx)
		if err != nil {
			return handoff(fmt.Errorf("read native workflow states: %w", err))
		}
		target := terminalAttemptTodoState(o.cfg.ActiveStates)
		var allowed bool
		if recoveryRefusal {
			target, allowed = connector.CompletionLane(states, issue.State, "Blocked", true)
			if !allowed || dispatchableState(states, target) {
				return handoff(fmt.Errorf("native workflow allows no recovery refusal handoff from %s to Blocked", issue.State))
			}
		} else if humanReview && recoveryRequired && preTurnFailureClass(event, running) == "" {
			target, allowed = connector.CompletionLane(states, issue.State, "Blocked", true)
			if !allowed {
				target, allowed = nativeRetryLane(states, issue.State, cfg.ReworkState)
			}
			if !allowed {
				return handoff(fmt.Errorf("native workflow allows no source recovery destination from %s", issue.State))
			}
		} else if humanReview && preTurnFailureClass(event, running) == "" {
			review := cfg.reviewTargetState()
			if autoPromoteOptoutLabel(issue, cfg) {
				review = cfg.SourceState
			}
			target, allowed = connector.LandingRefusalLane(states, issue.State, review, false)
			if !allowed {
				return handoff(fmt.Errorf("native workflow allows no move from %s to the review lane %s", issue.State, review))
			}
		} else {
			if running.DiffStats.RecoveryStateAvailable {
				target = cfg.ReworkState
			}
			target, allowed = nativeRetryLane(states, issue.State, target)
			if !allowed {
				return handoff(fmt.Errorf("native workflow allows no retry from %s", issue.State))
			}
		}
		if normalizeState(issue.State) != normalizeState(target) {
			metadata := workflowLaneMetadata{}
			if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issueID, issue, target, event.CompletedAt, terminalAttemptWithoutWorkProductReason, metadata); err != nil {
				return handoff(fmt.Errorf("move failed native item to %s: %w", target, err))
			}
		}
		failed := running
		failed.Issue.State = target
		failed.CompletionLane = target
		if o.handlePreTurnFailure(ctx, state, event, failed) {
			o.recordCompletionUsage(ctx, state, event, issue)
			return true
		}
		if err := o.abandonClaim(ctx, issueID); err != nil {
			return handoff(err)
		}
		terminal := terminalStateForRun(event.Err, finalState)
		class := runnerWorkAttemptErrorClass(event.Err)
		message := errorString(event.Err)
		o.recordProjectAttemptOutcome(state, issueID, event.CompletedAt, terminal, event.Err, class, message)
		o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, terminal, class, message, "failed", "native worker failed; source preserved", nativeChangeMetadata(change))
		o.recordCompletionUsage(ctx, state, event, issue)
		o.releaseClaim(state, issueID)
		return true
	}
	report, reported := workpad.SignalFromComment(event.Result.FinalMessage, "", "")
	humanReview = humanReview || reported && report != nil && report.Invalid == nil && report.HumanAction != ""
	blocked := reported && report != nil && report.Invalid == nil && report.Status == workpad.StatusBlocked
	clearedBlockers := false
	if blocked {
		blocked = len(report.Blockers) > 0 && report.HumanAction == "" && report.ReasonCode == ""
		for _, blocker := range report.Blockers {
			blocked = blocked && blocker.Owner == workpad.BlockerOwnerOrchestrator && !blocker.Unverifiable && blocker.Predicate != nil && blocker.Predicate.Type == workpad.PredicateIssueState
		}
		if blocked {
			reportedIssue := cloneIssue(issue)
			reportedIssue.DependencySource = connector.BlockedRefSourceWorkpad
			evidence := o.evaluateRecordedBlockerSignal(ctx, state, reportedIssue, report, nil, event.CompletedAt)
			clearedBlockers = evidence.Found && !evidence.Holds && !evidence.Unverifiable && !evidence.HumanOwned
			blocked = !clearedBlockers
		}
	}
	if o.completeRecordedInstanceBlockers(ctx, state, event, running, report) {
		return true
	}
	accepted := reported && report != nil && report.Invalid == nil && report.Status == workpad.StatusComplete && len(report.Blockers) == 0 && report.HumanAction == ""
	if change == nil && !blocked && !clearedBlockers && (humanReview || accepted || running.DiffStats.Status != "clean" || running.DiffStats.UnpushedCommits > 0) {
		return false
	}
	if change != nil && change.Error != "" {
		return handoff(errors.New(change.Error))
	}
	changed := change != nil && change.Changed
	if changed && change.ChangeID == "" {
		return handoff(fmt.Errorf("native change request was not opened: %s", change.Error))
	}
	if changed && change.VersionID == "" && change.VersionError == "" {
		return handoff(errors.New("the native change has no published current version"))
	}
	states, err := reader.WorkflowStates(ctx)
	if err != nil {
		return handoff(fmt.Errorf("read native workflow states: %w", err))
	}
	if change != nil {
		change = o.refreshNativeChangeReview(ctx, issueID, change)
	}
	needsReview := change != nil && change.VersionError != "" || !changed && !accepted || reported && !accepted
	review := cfg.reviewTargetState()
	if autoPromoteOptoutLabel(issue, cfg) {
		review = cfg.SourceState
	}
	target, ok := connector.CompletionLane(states, issue.State, review, changed || needsReview)
	if changed || needsReview {
		if humanReview {
			target, ok = connector.LandingRefusalLane(states, issue.State, review, false)
		} else {
			target, ok = "", false
		}
	}
	if blocked {
		target, ok = connector.CompletionLane(states, issue.State, "Blocked", true)
	}
	if clearedBlockers {
		target, ok = connector.CompletionLane(states, issue.State, normalizeDependencyAutoUnblockConfig(o.cfg.DependencyAutoUnblock).TargetState, true)
	}
	validatorRework := change != nil && change.Validator != nil && change.Validator.Verdict == "rework" && (report == nil || len(report.Blockers) == 0 && report.HumanAction == "")
	unfinished := validatorRework || change != nil && reported && report != nil && report.Invalid == nil && report.Status == workpad.StatusInProgress && len(report.Blockers) == 0 && report.HumanAction == ""
	if unfinished {
		if rework, allowed := nativeRetryLane(states, issue.State, cfg.ReworkState); allowed {
			target, ok = rework, true
		}
	}
	if !humanReview && !blocked && (change != nil && change.VersionError != "" || !ok && (needsReview || changed && !change.Reviewed)) {
		o.observeNativeCompletion(ctx, event, false)
		if !changed {
			retryState := terminalAttemptTodoState(o.cfg.ActiveStates)
			if running.DiffStats.RecoveryStateAvailable {
				retryState = cfg.ReworkState
			}
			retryState, allowed := nativeRetryLane(states, issue.State, retryState)
			if !allowed {
				return handoff(fmt.Errorf("native workflow allows no retry from %s", issue.State))
			}
			if normalizeState(issue.State) != normalizeState(retryState) {
				if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issueID, issue, retryState, event.CompletedAt, terminalAttemptWithoutWorkProductReason, workflowLaneMetadata{}); err != nil {
					return handoff(fmt.Errorf("move unfinished native item to %s: %w", retryState, err))
				}
			}
		}
		if err := o.abandonClaim(ctx, issueID); err != nil {
			return handoff(err)
		}
		o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "provider completed; issue acceptance is not recorded", nativeChangeMetadata(change))
		o.recordCompletionUsage(ctx, state, event, issue)
		o.releaseClaim(state, issueID)
		return true
	}
	if changed && change.Reviewed && !needsReview && !unfinished && !autoPromoteOptoutLabel(issue, cfg) {
		if landing, direct := connector.CompletionLane(states, issue.State, autoPromoteMergingState, true); direct && dispatchableState(states, landing) {
			target, ok = landing, true
		} else if _, allowed := connector.CompletionLane(states, issue.State, "", false); allowed {
			if err := o.continueNativeLandingRun(ctx, state, event, running); err != nil {
				return handoff(err)
			}
			return true
		} else {
			return handoff(fmt.Errorf("native workflow allows no landing path from %s", strings.TrimSpace(issue.State)))
		}
	}
	if !ok {
		if change != nil && change.VersionError != "" {
			return handoff(errors.New(change.VersionError))
		}
		if changed || needsReview {
			return handoff(fmt.Errorf("native workflow allows no move from %s to the review lane %s", strings.TrimSpace(issue.State), review))
		}
		return handoff(fmt.Errorf("native workflow allows no move from %s to a terminal lane", strings.TrimSpace(issue.State)))
	}
	if normalizeState(issue.State) != normalizeState(target) {
		if err := o.updateIssueStateByID(ctx, state, issueID, issue, target, event.CompletedAt, "completed_active_review_transition"); err != nil {
			return handoff(fmt.Errorf("move native item to %s: %w", target, err))
		}
	}
	var comment string
	if needsReview {
		disposition := "no valid complete detent-status disposition"
		if report != nil && report.Invalid == nil {
			disposition = "detent-status " + report.Status
		}
		comment = fmt.Sprintf("The provider turn completed with %s. Moved from %s to %s; the completed turn and any genuine source version are preserved, but issue acceptance is not recorded.", disposition, displayStateName(issue.State), displayStateName(target))
		if unfinished && normalizeState(target) == normalizeState(cfg.ReworkState) {
			comment = fmt.Sprintf("The provider turn completed with %s and no reported blocker or human action. Implementation remains unfinished in %s; the completed turn and any genuine source version are preserved for further work, but issue acceptance is not recorded.", disposition, displayStateName(target))
		}
	} else {
		comment = nativeCompletionComment(change, issue.State, target)
	}
	if needsReview {
		comment = nativeCompletionReason(comment, report, event.Result.FinalMessage)
		if change != nil && change.VersionError != "" {
			comment += "\n\n" + change.VersionError
		}
	}
	if err := o.connector.CreateComment(ctx, issueID, comment); err != nil {
		o.warnNativeCompletion(issue, fmt.Errorf("comment on the completed run: %w", err))
	}
	if !changed || needsReview {
		o.observeNativeCompletion(ctx, event, !needsReview && !unfinished)
	}
	if err := o.abandonClaim(ctx, issueID); err != nil {
		return handoff(err)
	}
	attemptCompleted := o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "worker completed", nativeChangeMetadata(change))
	completed := Completed{
		Issue:                      cloneIssue(issue),
		SessionID:                  running.SessionID,
		StartedAt:                  running.StartedAt,
		CompletedAt:                event.CompletedAt,
		FinalState:                 finalState,
		successfulAttemptPersisted: attemptCompleted,
		Tokens:                     event.Result.Tokens,
		RuntimeIdentity:            running.RuntimeIdentity,
	}
	o.recordCompletionUsage(ctx, state, event, issue)
	o.recordCompletedActiveReviewTransition(state, issue, completed, target)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "completed_issue_review_transition",
		Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + target + " after successful completion",
	})
	if o.logger != nil {
		changeID := ""
		if change != nil {
			changeID = change.ChangeID
		}
		o.logger.Info("completed native issue transition",
			"issue_id", issueID, "identifier", issue.Identifier,
			"from_state", issue.State, "target_state", target,
			"changed", changed, "change_id", changeID)
	}
	return true
}

func (o *Orchestrator) observeNativeCompletion(ctx context.Context, event runpkg.Completion, accepted bool) {
	execution := event.Request.Execution
	if execution == nil {
		if source, ok := o.scheduling.(interface{ RunExecution(string) runpkg.Execution }); ok {
			execution = source.RunExecution(event.IssueID)
		}
	}
	if runtime, ok := execution.(runpkg.RuntimeExecution); ok {
		at := time.Now().UTC()
		if err := runtime.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{HeartbeatAt: at, Phase: "completed", Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: accepted, ObservedAt: at}}); err != nil {
			o.warnNativeCompletion(event.Request.Issue, fmt.Errorf("observe host completion: %w", err))
		}
	}
}

func (o *Orchestrator) continueNativeLandingRun(ctx context.Context, state *State, event runpkg.Completion, running Running) error {
	request := event.Request
	if request.Execution == nil {
		if source, ok := o.scheduling.(interface{ RunExecution(string) runpkg.Execution }); ok {
			request.Execution = source.RunExecution(event.IssueID)
		}
	}
	if _, ok := request.Execution.(runpkg.LandingExecution); !ok || o.supervisor == nil {
		return errors.New("native landing execution is unavailable")
	}
	slotIssue := cloneIssue(running.Issue)
	slotIssue.State = autoPromoteMergingState
	slot, available, decision := o.acquireGlobalDispatchSlot(ctx, slotIssue, running.WorkerHost, o.clockNow(), 0)
	if !available {
		return fmt.Errorf("native landing capacity unavailable: %s", decision.Reason)
	}
	request.Issue = running.Issue
	request.Mode = runpkg.RunModeMerge
	request.Attempt = running.Attempt
	request.WorkAttemptID = running.WorkAttemptID
	request.Generation = running.Generation
	request.WorkerHost = running.WorkerHost
	request.DeferExecutionFinish = true
	request.AcquireModelPermit = nil
	runCtx, cancel := context.WithTimeoutCause(ctx, o.cfg.MergeWorkerMaxDuration, runpkg.ErrMergeWorkerDurationExceeded)
	runCtx, stop := context.WithCancelCause(runCtx)
	if request.OnUsageUpdate != nil {
		request.OnUsageUpdate = o.usageUpdateHandler(runCtx, event.IssueID, nil, running.progress)
	}
	if request.OnActivityUpdate != nil {
		request.OnActivityUpdate = o.activityUpdateHandler(runCtx, running.Issue)
	}
	running.Mode = runpkg.RunModeMerge
	running.ModelPermitExempt = true
	running.globalSlot = slot
	running.cancel = cancel
	running.stop = stop
	running.CompletionOwnershipReleased = false
	state.Running[event.IssueID] = running
	o.trackRunningHeartbeat(state, running, state.Claimed[event.IssueID], event.CompletedAt)
	o.publishRuntimeState(state)
	running.done = o.supervisor.Dispatch(runCtx, request, o.runResults)
	state.Running[event.IssueID] = running
	return nil
}

func nativeRetryLane(states []connector.WorkflowState, current, preferred string) (string, bool) {
	for _, state := range states {
		if normalizeState(state.Name) != normalizeState(preferred) || state.Terminal || state.OperatorOnly || !state.Dispatchable {
			continue
		}
		if normalizeState(current) == normalizeState(state.Name) {
			return state.Name, true
		}
		return connector.CompletionLane(states, current, state.Name, true)
	}
	return "", false
}

func nativeCompletionReason(comment string, report *workpad.Signal, finalMessage string) string {
	if report != nil && report.Invalid == nil {
		if reason := workpad.Reason(report); reason != "" {
			comment += "\n\nReason: " + workpad.FinalSummary(reason)
		}
	}
	if summary := workpad.FinalSummary(finalMessage); summary != "" {
		comment += "\n\nAgent summary:\n" + summary
	}
	return comment
}

// refreshNativeChangeReview reads whether the version the run published is
// reviewed as the completion is applied. A run that published no version has
// nothing to land, whatever an earlier version's review says. The run captured it when it published the version,
// and an approval or check that arrives before the completion lands is
// deliberately left to the completion by the Hub, so the answer the run
// captured may be stale. A failed read keeps the captured answer.
func (o *Orchestrator) refreshNativeChangeReview(ctx context.Context, issueID string, change *runpkg.NativeChange) *runpkg.NativeChange {
	reader, ok := o.connector.(connector.ChangeReviewReader)
	if change.VersionError != "" {
		refused := *change
		refused.Reviewed = false
		return &refused
	}
	if !ok || !change.Changed || change.ChangeID == "" || change.VersionID == "" {
		return change
	}
	reviewed, err := reader.ChangeReviewed(ctx, issueID, change.ChangeID, change.VersionID)
	if err != nil || reviewed == change.Reviewed {
		return change
	}
	refreshed := *change
	refreshed.Reviewed = reviewed
	return &refreshed
}

// dispatchableState reports a lane a runner claims work from. A landing lane
// that does not dispatch would hold an accepted change where nothing lands it.
func dispatchableState(states []connector.WorkflowState, name string) bool {
	for _, state := range states {
		if normalizeState(state.Name) == normalizeState(name) {
			return state.Dispatchable
		}
	}
	return false
}

func (o *Orchestrator) warnNativeCompletion(issue connector.Issue, err error) {
	if o.logger == nil {
		return
	}
	o.logger.Warn("native completion not applied",
		"issue_id", issue.ID, "identifier", issue.Identifier, "state", issue.State, "error", err)
}

func nativeChangeMetadata(change *runpkg.NativeChange) map[string]any {
	if change == nil {
		return nil
	}
	metadata := map[string]any{"native_changed": change.Changed, "native_files": change.Files}
	if change.ChangeID != "" {
		metadata["native_change_id"] = change.ChangeID
	}
	if change.HeadSHA != "" {
		metadata["native_head_sha"] = change.HeadSHA
	}
	if change.VersionID != "" {
		metadata["native_version_id"] = change.VersionID
	}
	if change.VersionError != "" {
		metadata["native_version_error"] = change.VersionError
	}
	if change.VersionCode != "" {
		metadata["native_version_code"] = change.VersionCode
	}
	return metadata
}

func nativeCompletionComment(change *runpkg.NativeChange, from, to string) string {
	from, to = displayStateName(from), displayStateName(to)
	if change.VersionError != "" {
		return fmt.Sprintf("The run completed, but Change Request %s could not publish its final version for head %s: %s. Moved from %s to %s.",
			change.ChangeID, shortCommit(change.HeadSHA), change.VersionError, from, to)
	}
	if change.Changed {
		comment := fmt.Sprintf("The run succeeded and opened Change Request %s (%d files, head %s). Moved from %s to %s.",
			change.ChangeID, change.Files, shortCommit(change.HeadSHA), from, to)
		switch {
		case change.Reviewed && normalizeState(to) == normalizeState(autoPromoteMergingState):
			comment += " The current version needs no further review, so the runner lands it next."
		case change.Reviewed:
			comment += fmt.Sprintf(" The current version needs no further review, but the workflow has no move from %s to %s, so it waits in %s.", from, displayStateName(autoPromoteMergingState), to)
		}
		return comment
	}
	return fmt.Sprintf("The run succeeded without committing a change against %s, so there is nothing to review. Moved from %s to %s so it is not run again.",
		shortCommit(change.BaseSHA), from, to)
}

func shortCommit(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "its base"
	}
	return sha
}
