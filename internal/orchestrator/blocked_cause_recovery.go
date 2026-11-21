package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	blockedRecoveryOwnerOrchestrator               = "orchestrator"
	blockedRecoveryOwnerHuman                      = "human"
	blockedRecoveryOwnerOperator                   = "operator"
	blockedRecoveryPredicateManaged                = "managed"
	workflowActionCauseBlockedRecovery             = "cause_blocked_recovery"
	blockedReadyPullRequestLookupNoneReason        = "ready_pr_lookup_none"
	blockedReadyPullRequestLookupUnavailableReason = "ready_pr_lookup_unavailable"
)

func (o *Orchestrator) newBlockedRecoveryMetadata(
	ctx context.Context,
	issue connector.Issue,
	runMode string,
	cause string,
	targetState string,
	fallback DiffStats,
) workflowLaneMetadata {
	resumable := dependencyAutoUnblockStartedSignal(issue)
	if !resumable {
		var present bool
		if o.recoveryInspector != nil {
			inspectionIssue := cloneIssue(issue)
			if strings.TrimSpace(targetState) != "" {
				inspectionIssue.State = targetState
			}
			present, resumable = o.recoveryInspector.BlockedRecoveryWorkspace(ctx, runpkg.RunRequest{
				Issue: inspectionIssue,
			})
		}
		if !present {
			resumable = fallback.UnpushedCommits > 0 || fallback.FilesChanged > 0
		}
	}
	if resumable {
		targetState = autoPromoteReworkState
	}
	return workflowLaneMetadata{
		BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{
			Owner:           blockedRecoveryOwnerOrchestrator,
			SourceRevision:  holdSourceRevision(issue),
			ExpiresAt:       time.Now().UTC().Add(o.cfg.MaxRetryBackoff),
			Cause:           strings.TrimSpace(cause),
			Predicate:       blockedRecoveryPredicateManaged,
			TargetState:     firstNonBlank(targetState, "Todo"),
			RunMode:         strings.TrimSpace(runMode),
			IntentResumable: resumable,
		},
	}
}

func (o *Orchestrator) recoverCauseBlockedIssue(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	now time.Time,
) bool {
	if normalizeState(issue.State) != normalizeState(blockedStatusState) && (issue.IssueContract == nil || issue.IssueContract.ReturnState == "" || normalizeState(issue.State) != normalizeState(o.cfg.StopRunTargetState)) {
		return false
	}
	issue = issue.WithNativeWorkpadAuthority()
	if o.currentBlockedOperatorStop(ctx, state, issue) {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "operator_stop")
		return false
	}
	if explicitHumanAction(issue) != "" {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "human_action")
		return false
	}
	if o.currentBlockedHumanIntent(ctx, state, issue) {
		parkedAt, found := o.currentBlockedRecoveryParkedAt(ctx, state, issue)
		if !found {
			return false
		}
		evidence := clearedHumanActionEvidence(issue, parkedAt, now)
		if evidence == nil {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "human_action")
			return false
		}
		return o.applyRecordedBlockerRecovery(ctx, state, issue, nil, []telemetry.BlockerEvidence{*evidence}, now)
	}
	signal, _ := rawIssueWorkpadSignal(issue)
	if signal != nil && signal.Invalid == nil {
		dependencies := workpad.CloneSignal(signal)
		dependencies.Blockers = nil
		for _, blocker := range signal.Blockers {
			if blocker.Owner == workpad.BlockerOwnerOrchestrator && blocker.Predicate != nil && blocker.Predicate.Type == workpad.PredicateIssueState {
				dependencies.Blockers = append(dependencies.Blockers, blocker)
			}
		}
		recorded := o.evaluateRecordedBlockerSignal(ctx, state, issue, dependencies, nil, now)
		if recorded.Found {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", string(BlockedRecoveryReasonDependencyBlocker))
			setBlockedEvidence(state, issue.ID, recorded.Evidence)
			if recorded.Holds {
				return false
			}
			if !recorded.Unverifiable {
				withDependencies, _, _ := o.issueWithCurrentWorkpadDependencyRefs(ctx, o.issueWithDependencyRefs(issue))
				blockers := o.resolveDependencyBlockers(ctx, withDependencies)
				return o.applyRecordedBlockerRecovery(ctx, state, withDependencies, blockers, recorded.Evidence, now)
			}
			if o.logger != nil {
				o.logger.Info("retry unverifiable dependency evaluation", "issue_id", issue.ID, "blocker_evidence", recorded.Evidence)
			}
		}
	}
	if o.nativeWorkflow() {
		return false
	}
	if err := o.retryMachineFailure(ctx, state, issue, nextAttempt(state.Retry[issue.ID].Attempt), now, issue.BlockerReason, ""); err != nil {
		if o.logger != nil {
			o.logger.Warn("blocked recovery retry failed", "issue_id", issue.ID, "error", err)
		}
		return false
	}
	return true
}

func (o *Orchestrator) blockedCauseHoldReason(
	issue connector.Issue,
	state *State,
	workpadBlockers []dependencyBlocker,
	dependencyCfg DependencyAutoUnblockConfig,
	workpadCurrent bool,
) string {
	if reason := BlockedRecoveryHumanHoldReason(issue, o.cfg.AutoPromote.OptoutLabel); reason != "" {
		return reason
	}
	if workpadCurrent && workpadHasRecordedNonDependencyBlocker(issue.WorkpadSignal) {
		return "recorded_blocker"
	}
	if len(workpadBlockers) > 0 && !dependencyBlockersReady(workpadBlockers, dependencyCfg, o.cfg.TerminalStates) {
		return "workpad_blocker"
	}
	if state != nil {
		if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok && blocked.Source == BlockedSourceOperatorStop {
			return "operator_stop"
		}
	}
	return ""
}

func workpadHasRecordedNonDependencyBlocker(signal *workpad.Signal) bool {
	if signal == nil || signal.Invalid != nil || strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return false
	}
	for _, blocker := range signal.Blockers {
		if blocker.Unverifiable || blocker.Predicate != nil && blocker.Predicate.Type != workpad.PredicateIssueState {
			return true
		}
	}
	return false
}

func BlockedRecoveryHumanHoldReason(issue connector.Issue, optoutLabel string) string {
	if issue.WorkpadSignal != nil {
		if strings.TrimSpace(issue.WorkpadSignal.HumanAction) != "" || issue.WorkpadSignal.ReasonCode == "permission_wait" {
			return "human_action"
		}
	}
	configuredOptout := normalizeLabel(optoutLabel)
	for _, label := range issue.Labels {
		normalized := normalizeLabel(label)
		if normalized == "requires-human-review" || configuredOptout != "" && normalized == configuredOptout {
			return "human_action"
		}
	}
	if issue.WorkpadSignal != nil && issue.WorkpadSignal.Invalid != nil {
		return "invalid_workpad_signal"
	}
	return ""
}

func (o *Orchestrator) currentBlockedOperatorStop(ctx context.Context, state *State, issue connector.Issue) bool {
	if state != nil {
		if intent, ok := state.Blocked[issue.ID]; ok && intent.Source == BlockedSourceOperatorStop {
			return true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	return ok &&
		normalizeState(entry.Event.PhaseName) == normalizeState(blockedStatusState) &&
		workflowLaneEntryMatchesCurrent(issue, entry.Event) &&
		(strings.EqualFold(strings.TrimSpace(entry.Event.Reason), string(store.WorkAttemptTerminalOperatorStopped)) || strings.TrimSpace(entry.Event.Reason) == "operator_move" && entry.Metadata.Provenance.Basis == provenance.BasisAuthenticatedHuman)
}

func (o *Orchestrator) currentBlockedRecoveryPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (workflowLaneBlockedRecoveryMetadata, bool) {
	issueID := strings.TrimSpace(issue.ID)
	if state != nil && issueID != "" {
		if blocked, ok := state.Blocked[issueID]; ok &&
			blocked.Recovery != nil &&
			blockedEntryMatchesCurrent(issue, blocked.BlockedAt) && blocked.Recovery.validate(issue, time.Now()) == nil {
			return *blocked.Recovery, true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok ||
		normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) ||
		!workflowLaneEntryMatchesCurrent(issue, entry.Event) {
		return workflowLaneBlockedRecoveryMetadata{}, false
	}
	if entry.Metadata.BlockedRecovery != nil && entry.Metadata.BlockedRecovery.validate(issue, time.Now()) == nil {
		return *entry.Metadata.BlockedRecovery, true
	}
	return workflowLaneBlockedRecoveryMetadata{}, false
}

func (o *Orchestrator) currentBlockedHumanIntent(ctx context.Context, state *State, issue connector.Issue) bool {
	if explicitHumanAction(issue) != "" {
		return true
	}
	if state != nil {
		if blocked, ok := state.Blocked[issue.ID]; ok && blockedEntryMatchesCurrent(issue, blocked.BlockedAt) && explicitHumanAction(blocked.Issue) != "" {
			return true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok || normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) || !workflowLaneEntryMatchesCurrent(issue, entry.Event) {
		return false
	}
	if strings.TrimSpace(entry.Metadata.HumanAction) != "" {
		return true
	}
	signal, parsed := workpad.SignalFromComment(entry.Metadata.ReasonDetail, "", "")
	if parsed && signal != nil && signal.Invalid == nil && strings.TrimSpace(signal.HumanAction) != "" {
		return true
	}
	return false
}

func (o *Orchestrator) currentBlockedRecoveryParkedAt(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (time.Time, bool) {
	issueID := strings.TrimSpace(issue.ID)
	if state != nil && issueID != "" {
		if blocked, ok := state.Blocked[issueID]; ok &&
			blocked.Recovery != nil &&
			blockedEntryMatchesCurrent(issue, blocked.BlockedAt) &&
			!blocked.BlockedAt.IsZero() {
			return blocked.BlockedAt.UTC(), true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	parkedAt := workflowLaneTransitionAt(entry.Event)
	target := blockedStatusState
	if issue.IssueContract != nil && issue.IssueContract.ReturnState != "" {
		target = firstNonBlank(o.cfg.StopRunTargetState, blockedStatusState)
	}
	if ok &&
		normalizeState(entry.Event.PhaseName) == normalizeState(target) &&
		workflowLaneEntryMatchesCurrent(issue, entry.Event) &&
		!parkedAt.IsZero() {
		return parkedAt, true
	}
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		return issue.StageUpdatedAt.UTC(), true
	}
	return time.Time{}, false
}

func (o *Orchestrator) recordBlockedRecoveryDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	action string,
	reason string,
) {
	if o.logger != nil {
		o.logger.Info("blocked recovery decision", "issue_id", strings.TrimSpace(issue.ID),
			"identifier", strings.TrimSpace(issue.Identifier), "action", action, "reason", reason)
	}
	if state == nil || strings.TrimSpace(issue.ID) == "" {
		return
	}
	current, hasRecovery := o.currentBlockedRecoveryPark(ctx, state, issue)
	entry, ok := state.Blocked[issue.ID]
	if !ok {
		if !blockedRecoveryDecisionShouldMaterialize(reason) {
			return
		}
		blockedAt := time.Time{}
		if issue.StageUpdatedAt != nil {
			blockedAt = issue.StageUpdatedAt.UTC()
		}
		o.setBlockedStatusIssue(ctx, state, issue, blockedAt)
		entry, ok = state.Blocked[issue.ID]
		if !ok {
			return
		}
	}
	previousRecoveryReason := strings.TrimSpace(entry.RecoveryReason)
	entry.Issue = cloneIssue(issue)
	if trackerReason := strings.TrimSpace(issue.BlockerReason); trackerReason != "" {
		entry.Reason = trackerReason
	} else if strings.EqualFold(previousRecoveryReason, string(BlockedRecoveryReasonDependencyBlocker)) ||
		strings.EqualFold(previousRecoveryReason, "dependency_recovery") ||
		strings.EqualFold(strings.TrimSpace(entry.Reason), "blocked by project status") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.Reason)), "waiting on ") {
		entry.Reason = ""
	}
	entry.RecoveryAction = strings.TrimSpace(action)
	entry.RecoveryReason = strings.TrimSpace(reason)
	entry.RecoveryRemedy = BlockedRecoveryOperatorRemedy(issue, reason)
	entry.RecoveryReachability = blockedRecoveryReachability(action)
	entry.NeedsHumanAttention = strings.EqualFold(strings.TrimSpace(action), "hold")
	entry.BlockerEvidence = nil
	entry.RecoveryRoot = nil
	if hasRecovery {
		current.IntentResumable = current.intentResumable()
		current.Resumable = false
		current.Reachability = entry.RecoveryReachability
		current.HoldReason = entry.RecoveryReason
		current.OperatorRemedy = entry.RecoveryRemedy
		entry.Recovery = &current
		entry.RecoveryTarget = current.TargetState
		entry.RecoveryIntentResumable = current.IntentResumable
	} else {
		entry.Recovery = nil
		entry.RecoveryIntentResumable = strings.EqualFold(strings.TrimSpace(action), "defer")
	}
	state.Blocked[issue.ID] = entry
}

func blockedRecoveryDecisionShouldMaterialize(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch reason {
	case "no_recovery_predicate", "pr_maintenance_recovery", "rework_breaker_recovery":
		return false
	default:
		return true
	}
}

func blockedDependencyWaitingReason(blockers []dependencyBlocker) string {
	parts := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		part := dependencyBlockerLabel(blocker)
		if state := dependencyBlockerState(blocker); state != "" {
			part += " (" + state + ")"
		}
		parts = append(parts, part)
	}
	return "waiting on " + strings.Join(parts, ", ")
}

func blockedRecoveryReachability(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "hold":
		return "held"
	case "defer":
		return "deferred"
	default:
		return ""
	}
}

func BlockedRecoveryOperatorRemedy(issue connector.Issue, reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "invalid_workpad_signal":
		if issue.PullRequest == nil && issue.PRNumber == nil {
			return "Move the issue to Todo or another fresh-work lane; no pull request exists to resume."
		}
		return "Correct the latest Codex Workpad detent-status block, or move the issue to a fresh-work lane."
	case "human_action":
		if issue.WorkpadSignal != nil && strings.TrimSpace(issue.WorkpadSignal.HumanAction) != "" {
			return strings.TrimSpace(issue.WorkpadSignal.HumanAction)
		}
		return "Complete the requested human action, then update the Workpad."
	case "unverifiable_blocker":
		return "Replace the free-text blocker with a typed predicate, or resolve it and update the Workpad."
	case "recorded_blocker_holds":
		return "Detent will re-check the recorded blocker predicate on the next tick."
	case "recorded_blocker":
		return "Detent is retaining the issue while its recorded blocker predicate is active."
	case "workpad_blocker":
		return "Resolve or remove the current Workpad blocker references."
	case "operator_stop":
		return "Review the operator stop and route the issue to its intended lane."
	case "cause_unchanged":
		return "Change the blocking cause, or move the issue to a lane that starts fresh work."
	case "transition_failed":
		return "Retry the lane transition after restoring tracker write access."
	case githubRESTBudgetWaitingReason, githubRESTBudgetObservationPendingReason:
		return "Detent will return the issue to its prior lane after the matching GitHub REST budget rises above the worker reserve."
	case blockedReadyPullRequestLookupNoneReason:
		return "No PR found for the current workspace branch and head."
	case blockedReadyPullRequestLookupUnavailableReason:
		return "Retry exact-head pull request lookup after connector access recovers."
	case "no_recovery_predicate":
		return "Add a durable recovery predicate or move the issue to a lane that starts fresh work."
	default:
		return "Review the blocked recovery reason and move the issue to an appropriate lane."
	}
}

func explicitHumanAction(issue connector.Issue) string {
	if issue.IssueContract != nil && strings.TrimSpace(issue.IssueContract.HumanAction) != "" {
		return issue.IssueContract.HumanAction
	}
	signal, _ := rawIssueWorkpadSignal(issue)
	if current := issue.WorkpadSignal; current != nil && current.RecordedAt != nil && (signal == nil || signal.RecordedAt == nil || signal.RecordedAt.Before(*current.RecordedAt)) {
		signal = current
	}
	if signal != nil && signal.Invalid == nil && signal.Source == workpad.SourceStructured && strings.TrimSpace(signal.HumanAction) != "" {
		return strings.TrimSpace(signal.HumanAction)
	}
	index := currentWorkpadCommentIndex(issue.Comments)
	if index >= 0 && (signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured || signal.Status != workpad.StatusInProgress || !issue.Comments[index].AuthorAuthorized) {
		for _, comment := range issue.Comments {
			prior, ok := workpad.SignalFromComment(comment.Body, comment.URL, dependencyIssueRepo(issue.Identifier))
			if ok && prior != nil && prior.Invalid == nil && prior.Source == workpad.SourceStructured && strings.TrimSpace(prior.HumanAction) != "" {
				return strings.TrimSpace(prior.HumanAction)
			}
		}
	}
	return ""
}
