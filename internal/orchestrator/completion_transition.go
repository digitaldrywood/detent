package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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
			cfg.SourceState,
			gateRequiresPullRequest(cfg.Gate),
		)
		if targetState == "" {
			continue
		}

		result.transitioned[issueID] = struct{}{}
		if direct, promoted := o.tryDirectCompletedActiveAutoPromote(ctx, state, issue, targetState, cfg, now); direct {
			if mergeWorkerIssue(promoted) {
				o.recordMergeQueueEntered(state, promoted, now, "completed_active_auto_promote")
				result.dispatchCandidates = append(result.dispatchCandidates, promoted)
				o.logMergeWorkerPickup(promoted, "completed_active_auto_promote")
			}
			o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, promoted.State)
			continue
		}

		if err := o.updateIssueStateByID(ctx, issueID, issue, targetState, now, "completed_active_review_transition"); err != nil {
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
	cfg AutoPromoteConfig,
	now time.Time,
) (bool, connector.Issue) {
	if !cfg.Enabled || cfg.QuietDuration != 0 || normalizeState(reviewState) != normalizeState(cfg.SourceState) {
		return false, connector.Issue{}
	}

	summary := AutoPromoteSummaryFromIssue(issue)
	decision := EvaluateAutoPromote(issue, summary, cfg, now)
	if decision.Action == AutoPromoteActionPromote {
		issue, decision = o.hydrateAutoPromoteWorkpadDecision(ctx, issue, summary, cfg, now)
	}
	if decision.Action != AutoPromoteActionPromote {
		o.logAutoPromoteDecision(issue, decision, "")
		return false, connector.Issue{}
	}
	targetState := autoPromoteTargetState(decision.Action, cfg)
	if targetState == "" {
		o.logAutoPromoteDecision(issue, decision, "")
		return false, connector.Issue{}
	}
	if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, targetState, now) {
		return false, connector.Issue{}
	}
	promoted := promotedIssue(issue, targetState, now)
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
	reviewState string,
	requirePullRequest bool,
) string {
	if !stateIn(issue.State, activeStates) || stateIn(issue.State, terminalStates) {
		return ""
	}
	reviewState = strings.TrimSpace(reviewState)
	if reviewState == "" {
		reviewState = autoPromoteSourceState
	}
	switch normalizeState(issue.State) {
	case normalizeState(reviewState), normalizeState(autoPromoteReworkState), normalizeState(autoPromoteMergingState):
		return ""
	}
	if !completedActiveIssueReadyForReview(issue, requirePullRequest) {
		return ""
	}
	switch normalizeState(finalState) {
	case "", normalizeState(FinalStateCompleted), normalizeState(reviewState):
		return reviewState
	default:
		return ""
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
