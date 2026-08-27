package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) reconcileTarget(ctx context.Context, state *State, request targetedRefreshRequest) {
	reconciler, ok := o.connector.(connector.TargetedReconciler)
	if !ok {
		fallback := request.manualRefreshRequest
		fallback.operations = append(fallback.operations, "full_refresh_fallback")
		o.tickWithManual(ctx, state, request.requestedAt, &fallback)
		return
	}

	now := o.now().UTC()
	startManualRefresh(state, request.manualRefreshRequest, now)
	o.markRefresh(state, now)
	completed := false
	defer func() {
		o.finishRefresh(state, now, true)
		finishManualRefresh(state, request.manualRefreshRequest, completed)
	}()

	result, err := reconciler.ReconcileIssue(ctx, request.target)
	if err != nil {
		message := "targeted tracker reconcile failed: " + err.Error()
		markRefreshError(state, message, now)
		recordStateEvent(state, telemetry.ActivityEvent{At: now, Event: "targeted_reconcile_failed", Message: message})
		return
	}

	o.applyTargetedReconcile(ctx, state, request.target, result, now)
	clearRefreshError(state)
	o.markRefreshSucceeded(state, now)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "targeted_reconcile_succeeded",
		Message: targetedReconcileMessage(request.target, result),
	})
	completed = true
}

func (o *Orchestrator) applyTargetedReconcile(
	ctx context.Context,
	state *State,
	target connector.ReconcileTarget,
	result connector.ReconcileResult,
	now time.Time,
) {
	if !result.Found || strings.TrimSpace(result.Issue.ID) == "" {
		state.BoardIssues = removeTargetedIssue(state.BoardIssues, target, result.Issue)
		state.Pipeline = removeTargetedIssue(state.Pipeline, target, result.Issue)
		state.epicTransitionWatch = removeTargetedIssue(state.epicTransitionWatch, target, result.Issue)
		return
	}

	priorRunning, hadRunning := state.Running[strings.TrimSpace(result.Issue.ID)]
	issue := mergeTargetedIssue(state, result.Issue, now)
	visibleStates := append(append([]string(nil), o.cfg.ActiveStates...), o.cfg.ObservedStates...)
	state.BoardIssues = reconcileTargetedIssueSlice(state.BoardIssues, issue, stateIn(issue.State, visibleStates))
	state.Pipeline = reconcileTargetedIssueSlice(state.Pipeline, issue, stateIn(issue.State, autoPromoteFetchStates(o.cfg.AutoPromote)))
	state.epicTransitionWatch = reconcileTargetedIssueSlice(state.epicTransitionWatch, issue, stateIn(issue.State, o.cfg.ActiveStates))
	o.updateTargetedIssueEntries(state, issue)

	if normalizeState(issue.State) == normalizeState(blockedStatusState) {
		o.setBlockedStatusIssue(ctx, state, issue, now)
	} else {
		clearBlockedStatusIssue(state, issue.ID)
	}

	if running, ok := state.Running[issue.ID]; ok &&
		(!stateIn(running.Issue.State, o.cfg.ActiveStates) || workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates)) {
		if hadRunning {
			receipt, receiptFound, receiptErr := o.laneMutationReceipt(ctx, priorRunning, running.Issue.State)
			if receiptErr != nil {
				if o.logger != nil {
					o.logger.Warn("targeted lane mutation receipt lookup failed", "issue_id", issue.ID, "error", receiptErr)
				}
				return
			}
			if receiptFound {
				if receipt.Disposition == laneMutationRevokeWorker {
					o.beginLaneRevocationForMutation(ctx, state, priorRunning, running.Issue, now, receipt)
					return
				}
				running.laneMutation = receipt
				state.Running[issue.ID] = running
				return
			}
		}
		if accepted, acceptedCompletion := o.acceptCurrentAttemptCompletionLane(ctx, state, running, running.Issue, now); acceptedCompletion {
			state.Running[issue.ID] = accepted
			return
		}
		if running.Generation == 0 && workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
			o.completeTerminalRunning(ctx, state, issue.ID, running, terminalCompletedAt(running.Issue, o.cfg.TerminalStates, now), running.Tokens)
			return
		}
		if hadRunning {
			running = priorRunning
		}
		o.beginLaneRevocation(ctx, state, running, issue, now, laneRevocationStateChanged)
	}
}

func mergeTargetedIssue(state *State, refreshed connector.Issue, now time.Time) connector.Issue {
	current, ok := targetedIssueFromState(state, refreshed)
	if !ok {
		return cloneIssue(refreshed)
	}
	stateChanged := normalizeState(current.State) != normalizeState(refreshed.State)
	merged := mergeIssueTrackerFields(current, refreshed)
	if stateChanged {
		if refreshed.StageUpdatedAt != nil {
			merged.StageUpdatedAt = timePointerFromPtr(refreshed.StageUpdatedAt)
		} else if refreshed.UpdatedAt != nil {
			merged.StageUpdatedAt = timePointerFromPtr(refreshed.UpdatedAt)
		} else {
			merged.StageUpdatedAt = timePointer(now)
		}
	}
	return merged
}

func targetedIssueFromState(state *State, issue connector.Issue) (connector.Issue, bool) {
	if state == nil {
		return connector.Issue{}, false
	}
	for _, issues := range [][]connector.Issue{state.BoardIssues, state.Pipeline, state.epicTransitionWatch} {
		for _, current := range issues {
			if targetedIssuesMatch(current, issue) {
				return current, true
			}
		}
	}
	if running, ok := state.Running[strings.TrimSpace(issue.ID)]; ok {
		return running.Issue, true
	}
	return connector.Issue{}, false
}

func reconcileTargetedIssueSlice(issues []connector.Issue, issue connector.Issue, keep bool) []connector.Issue {
	out := make([]connector.Issue, 0, len(issues)+1)
	found := false
	for _, current := range issues {
		if !targetedIssuesMatch(current, issue) {
			out = append(out, cloneIssue(current))
			continue
		}
		found = true
		if keep {
			out = append(out, cloneIssue(issue))
		}
	}
	if keep && !found {
		out = append(out, cloneIssue(issue))
	}
	return out
}

func removeTargetedIssue(issues []connector.Issue, target connector.ReconcileTarget, issue connector.Issue) []connector.Issue {
	out := make([]connector.Issue, 0, len(issues))
	identifier := ""
	if target.WorkItemNumber > 0 && strings.TrimSpace(target.Scope) != "" {
		identifier = fmt.Sprintf("%s#%d", strings.TrimSpace(target.Scope), target.WorkItemNumber)
	}
	for _, current := range issues {
		if targetedIssuesMatch(current, issue) || identifier != "" && strings.EqualFold(strings.TrimSpace(current.Identifier), identifier) {
			continue
		}
		out = append(out, cloneIssue(current))
	}
	return out
}

func targetedIssuesMatch(left connector.Issue, right connector.Issue) bool {
	if leftID, rightID := strings.TrimSpace(left.ID), strings.TrimSpace(right.ID); leftID != "" && rightID != "" {
		return leftID == rightID
	}
	if leftIdentifier, rightIdentifier := strings.TrimSpace(left.Identifier), strings.TrimSpace(right.Identifier); leftIdentifier != "" && rightIdentifier != "" {
		return strings.EqualFold(leftIdentifier, rightIdentifier)
	}
	return false
}

func (o *Orchestrator) updateTargetedIssueEntries(state *State, issue connector.Issue) {
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return
	}
	if running, ok := state.Running[issueID]; ok {
		running.Issue = mergeIssueTrackerFields(running.Issue, issue)
		state.Running[issueID] = running
	}
	if claimed, ok := state.Claimed[issueID]; ok {
		claimed.Issue = mergeIssueTrackerFields(claimed.Issue, issue)
		state.Claimed[issueID] = claimed
	}
	if retry, ok := state.Retry[issueID]; ok {
		retry.Issue = mergeIssueTrackerFields(retry.Issue, issue)
		state.Retry[issueID] = retry
	}
	if blocked, ok := state.Blocked[issueID]; ok {
		blocked.Issue = mergeIssueTrackerFields(blocked.Issue, issue)
		state.Blocked[issueID] = blocked
	}
	if completed, ok := state.Completed[issueID]; ok {
		completed.Issue = mergeIssueTrackerFields(completed.Issue, issue)
		state.Completed[issueID] = completed
	}
	delete(state.AutoPromoteDecisions, issueID)
}

func targetedReconcileMessage(target connector.ReconcileTarget, result connector.ReconcileResult) string {
	if result.Found {
		return "reconciled " + issueLabel(result.Issue) + " from " + strings.TrimSpace(target.Event)
	}
	return "targeted tracker item is no longer visible: " + strings.TrimSpace(target.Scope)
}
