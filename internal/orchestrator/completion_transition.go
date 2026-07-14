package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) transitionCompletedActiveIssuesToReview(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) autoPromoteTickResult {
	if len(state.Completed) == 0 || len(issues) == 0 {
		return autoPromoteTickResult{}
	}

	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	result := autoPromoteTickResult{transitioned: map[string]struct{}{}}
	for _, issue := range issues {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		completed, ok := state.Completed[issueID]
		if !ok {
			continue
		}
		targetState := completedActiveReviewTargetState(
			issue,
			completed.FinalState,
			o.cfg.ActiveStates,
			o.cfg.TerminalStates,
			cfg,
		)
		if targetState == "" {
			if transitioned, promoted := o.transitionTimedOutCompletedActiveGateWait(ctx, state, issue, completed, cfg, now); transitioned {
				result.transitioned[issueID] = struct{}{}
				if mergeWorkerIssue(promoted) {
					o.recordMergeQueueEntered(state, promoted, now, "completed_active_gate_wait_timeout")
					result.dispatchCandidates = append(result.dispatchCandidates, promoted)
					o.logMergeWorkerPickup(promoted, "completed_active_gate_wait_timeout")
				}
			}
			continue
		}

		result.transitioned[issueID] = struct{}{}
		if direct, promoted := o.tryDirectCompletedActiveAutoPromote(ctx, state, issue, targetState, completed.FinalState, cfg, now); direct {
			if mergeWorkerIssue(promoted) {
				o.recordMergeQueueEntered(state, promoted, now, "completed_active_auto_promote")
				result.dispatchCandidates = append(result.dispatchCandidates, promoted)
				o.logMergeWorkerPickup(promoted, "completed_active_auto_promote")
			}
			o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, promoted.State)
			continue
		}

		if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "completed_active_review_transition"); err != nil {
			if o.logger != nil {
				o.logger.Warn(
					"completed issue review transition failed",
					"issue_id", issueID,
					"identifier", issue.Identifier,
					"from_state", issue.State,
					"target_state", targetState,
					"error", err,
				)
			}
			continue
		}

		o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, targetState)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "completed_issue_review_transition",
			Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + targetState + " after successful completion",
		})
		if o.logger != nil {
			o.logger.Info(
				"completed issue review transition",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"from_state", issue.State,
				"target_state", targetState,
			)
		}
	}
	if len(result.transitioned) == 0 {
		return autoPromoteTickResult{}
	}
	return result
}

func (o *Orchestrator) transitionActiveArtifactGateWaitIssuesToReview(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) autoPromoteTickResult {
	if len(issues) == 0 {
		return autoPromoteTickResult{}
	}

	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	result := autoPromoteTickResult{transitioned: map[string]struct{}{}}
	for _, issue := range issues {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if _, running := state.Running[issueID]; running {
			continue
		}
		targetState := activeArtifactGateWaitReviewTargetState(
			issue,
			o.cfg.ActiveStates,
			o.cfg.TerminalStates,
			cfg,
		)
		if targetState == "" {
			continue
		}

		if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "artifact_gate_wait_review_reconciliation"); err != nil {
			if o.logger != nil {
				o.logger.Warn(
					"artifact gate wait review reconciliation failed",
					"issue_id", issueID,
					"identifier", issue.Identifier,
					"from_state", issue.State,
					"target_state", targetState,
					"error", err,
				)
			}
			continue
		}
		if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
			o.logger.Warn("abandon artifact gate wait claim failed", "issue_id", issueID, "error", err)
		}
		delete(state.Claimed, issueID)
		delete(state.Retry, issueID)
		delete(state.BudgetRefusals, issueID)
		delete(state.PriorAttempts, issueID)
		result.transitioned[issueID] = struct{}{}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "artifact_gate_wait_review_reconciliation",
			Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + targetState + " after artifact gate wait status",
		})
		if o.logger != nil {
			o.logger.Info(
				"artifact gate wait review reconciliation",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"from_state", issue.State,
				"target_state", targetState,
			)
		}
	}
	if len(result.transitioned) == 0 {
		return autoPromoteTickResult{}
	}
	return result
}

func (o *Orchestrator) tryDirectCompletedActiveAutoPromote(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	reviewState string,
	completedFinalState string,
	cfg AutoPromoteConfig,
	now time.Time,
) (bool, connector.Issue) {
	if !cfg.Enabled || cfg.QuietDuration != 0 || normalizeState(reviewState) != normalizeState(cfg.SourceState) {
		return false, connector.Issue{}
	}

	summary := AutoPromoteSummaryFromIssue(issue)
	summary.CompletedFinalState = completedFinalState
	decision := EvaluateAutoPromote(issue, summary, cfg, now)
	if autoPromoteDecisionNeedsWorkpadHydration(decision) {
		issue, decision = o.hydrateAutoPromoteWorkpadDecision(ctx, issue, summary, cfg, now)
	}
	targetState := autoPromoteTargetState(decision.Action, cfg)
	if targetState == "" {
		o.logAutoPromoteDecision(issue, decision, "")
		return false, connector.Issue{}
	}
	promoted := promotedIssue(issue, targetState, now)
	if normalizeState(issue.State) == normalizeState(targetState) {
		return true, promoted
	}
	if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, targetState, now) {
		return false, connector.Issue{}
	}
	return true, promoted
}

func (o *Orchestrator) finishCompletedActiveReviewTransition(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	completed Completed,
	targetState string,
) {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("abandon completed review claim failed", "issue_id", issueID, "error", err)
	}
	updated := mergeIssueTrackerFields(completed.Issue, issue)
	updated.State = targetState
	completed.Issue = updated
	state.Completed[issueID] = completed
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
}

func completedActiveReviewTargetState(
	issue connector.Issue,
	finalState string,
	activeStates []string,
	terminalStates []string,
	cfg AutoPromoteConfig,
) string {
	cfg = normalizeAutoPromoteConfig(cfg)
	if !stateIn(issue.State, activeStates) || stateIn(issue.State, terminalStates) {
		return ""
	}
	reviewState := cfg.SourceState
	switch normalizeState(issue.State) {
	case normalizeState(reviewState), normalizeState(autoPromoteMergingState):
		return ""
	case normalizeState(autoPromoteReworkState):
		if gate.Effective(cfg.Gate).Kind != gate.KindArtifact {
			return ""
		}
	}
	if !completedActiveIssueReadyForReview(issue, gateRequiresPullRequest(cfg.Gate)) {
		return ""
	}
	if !completedActiveShouldEnterReview(issue, cfg) {
		return ""
	}
	if completedActiveFinalStateReviewEligible(finalState, reviewState) {
		return reviewState
	}
	return ""
}

func completedActiveShouldEnterReview(issue connector.Issue, cfg AutoPromoteConfig) bool {
	if autoPromoteHumanReviewRequired(issue, cfg, cfg.Gate) {
		return true
	}
	if gate.Effective(cfg.Gate).Kind == gate.KindArtifact {
		return true
	}
	return cfg.GateWaitState == autoPromoteGateWaitReview
}

func activeArtifactGateWaitReviewTargetState(
	issue connector.Issue,
	activeStates []string,
	terminalStates []string,
	cfg AutoPromoteConfig,
) string {
	cfg = normalizeAutoPromoteConfig(cfg)
	if gate.Effective(cfg.Gate).Kind != gate.KindArtifact {
		return ""
	}
	if !stateIn(issue.State, activeStates) || stateIn(issue.State, terminalStates) {
		return ""
	}
	switch normalizeState(issue.State) {
	case "todo", normalizeState(cfg.SourceState), normalizeState(autoPromoteMergingState):
		return ""
	}
	if !artifactGateWaitStatusBlocksDispatch(issue, cfg.Gate) {
		return ""
	}
	return cfg.SourceState
}

func completedActiveFinalStateReviewEligible(finalState string, reviewState string) bool {
	switch normalizeState(finalState) {
	case "", normalizeState(FinalStateCompleted), normalizeState(reviewState):
		return true
	default:
		return false
	}
}

func completedActiveIssueReadyForReview(issue connector.Issue, requirePullRequest bool) bool {
	if !requirePullRequest {
		return true
	}
	if issue.PullRequest == nil {
		return false
	}
	return normalizePullRequestState(issue.PullRequest.State) == "open"
}

func (o *Orchestrator) transitionTimedOutCompletedActiveGateWait(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	completed Completed,
	cfg AutoPromoteConfig,
	now time.Time,
) (bool, connector.Issue) {
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" || completed.CompletedAt.IsZero() {
		return false, connector.Issue{}
	}
	if now.Before(completed.CompletedAt.Add(cfg.GateWaitTimeout)) {
		return false, connector.Issue{}
	}
	if !autoPromoteActiveGatePendingIssue(issue, state, o.cfg, cfg) {
		return false, connector.Issue{}
	}
	if cfg.GateWaitTimeoutAction == autoPromoteGateWaitTimeoutMerge {
		summary := AutoPromoteSummaryFromIssue(issue)
		summary.CompletedFinalState = completed.FinalState
		summary.AutomatedReviewWaitExpired = true
		decision := EvaluateAutoPromote(issue, summary, cfg, now)
		if autoPromoteDecisionNeedsWorkpadHydration(decision) {
			issue, decision = o.hydrateAutoPromoteWorkpadDecision(ctx, issue, summary, cfg, now)
		}
		targetState := autoPromoteTargetState(decision.Action, cfg)
		if targetState == "" {
			recordAutoPromoteSnapshotDecision(state, issueID, decision)
			o.logAutoPromoteDecision(issue, decision, "")
			return false, connector.Issue{}
		}
		if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, targetState, now) {
			return false, connector.Issue{}
		}
		promoted := promotedIssue(issue, targetState, now)
		o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, targetState)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "completed_active_gate_wait_timeout",
			Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + targetState + " after auto-promote gate wait timeout",
		})
		return true, promoted
	}
	targetState := cfg.SourceState
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "auto_promote_gate_wait_timeout"); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"completed issue gate wait timeout transition failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"from_state", issue.State,
				"target_state", targetState,
				"error", err,
			)
		}
		return false, connector.Issue{}
	}
	if err := o.connector.CreateComment(ctx, issueID, completedActiveGateWaitTimeoutComment(issue, completed, cfg, now)); err != nil && o.logger != nil {
		o.logger.Warn(
			"completed issue gate wait timeout comment failed",
			"issue_id", issueID,
			"identifier", issue.Identifier,
			"error", err,
		)
	}
	o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, targetState)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "completed_active_gate_wait_timeout",
		Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + targetState + " after auto-promote gate wait timeout",
	})
	return true, promotedIssue(issue, targetState, now)
}

func completedActiveGateWaitTimeoutComment(
	issue connector.Issue,
	completed Completed,
	cfg AutoPromoteConfig,
	now time.Time,
) string {
	waited := now.Sub(completed.CompletedAt)
	var b strings.Builder
	b.WriteString("Auto-promote gate wait timed out; moved this issue from ")
	b.WriteString(strings.TrimSpace(issue.State))
	b.WriteString(" to ")
	b.WriteString(cfg.SourceState)
	b.WriteString(".")
	b.WriteString("\n\n- reason: auto_promote_gate_wait_timeout")
	b.WriteString("\n- waited: ")
	b.WriteString(waited.Round(time.Second).String())
	b.WriteString("\n- timeout: ")
	b.WriteString(cfg.GateWaitTimeout.Round(time.Second).String())
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			b.WriteString("\n- pull_request: ")
			b.WriteString(url)
		}
		if ciStatus := strings.TrimSpace(issue.PullRequest.CIStatus); ciStatus != "" {
			b.WriteString("\n- ci_status: ")
			b.WriteString(ciStatus)
		}
		if mergeableState := strings.TrimSpace(issue.PullRequest.MergeableState); mergeableState != "" {
			b.WriteString("\n- mergeable_state: ")
			b.WriteString(strings.ToLower(mergeableState))
		}
	}
	return b.String()
}
