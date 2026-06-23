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
) map[string]struct{} {
	if len(state.Completed) == 0 || len(issues) == 0 {
		return nil
	}

	handled := map[string]struct{}{}
	for _, issue := range issues {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		completed, ok := state.Completed[issueID]
		if !ok {
			continue
		}
		targetState := completedActiveReviewTargetState(issue, completed.FinalState, o.cfg.ActiveStates, o.cfg.TerminalStates)
		if targetState == "" {
			continue
		}

		handled[issueID] = struct{}{}
		if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
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
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "completed_issue_review_transition",
			Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + targetState + " after successful completion",
		})
	}
	if len(handled) == 0 {
		return nil
	}
	return handled
}

func completedActiveReviewTargetState(issue connector.Issue, finalState string, activeStates []string, terminalStates []string) string {
	if !stateIn(issue.State, activeStates) || stateIn(issue.State, terminalStates) {
		return ""
	}
	switch normalizeState(issue.State) {
	case normalizeState(autoPromoteSourceState), normalizeState(autoPromoteReworkState), normalizeState(autoPromoteMergingState):
		return ""
	}
	if !completedActiveIssueReadyForReview(issue) {
		return ""
	}
	switch normalizeState(finalState) {
	case "", normalizeState(FinalStateCompleted), normalizeState(autoPromoteSourceState):
		return autoPromoteSourceState
	default:
		return ""
	}
}

func completedActiveIssueReadyForReview(issue connector.Issue) bool {
	if issue.PullRequest == nil {
		return false
	}
	return normalizePullRequestState(issue.PullRequest.State) == "open"
}
