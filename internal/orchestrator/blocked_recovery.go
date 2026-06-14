package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type BlockedRecoveryAction string

const (
	BlockedRecoveryActionNone   BlockedRecoveryAction = ""
	BlockedRecoveryActionRework BlockedRecoveryAction = "rework"
)

type BlockedRecoveryReason string

const (
	BlockedRecoveryReasonNotBlocked             BlockedRecoveryReason = "not_blocked"
	BlockedRecoveryReasonHumanBlocker           BlockedRecoveryReason = "human_blocker"
	BlockedRecoveryReasonDependencyBlocker      BlockedRecoveryReason = "dependency_blocker"
	BlockedRecoveryReasonMissingPullRequest     BlockedRecoveryReason = "missing_pull_request"
	BlockedRecoveryReasonPullRequestNotOpen     BlockedRecoveryReason = "pull_request_not_open"
	BlockedRecoveryReasonNoRecoverableSignal    BlockedRecoveryReason = "no_recoverable_signal"
	BlockedRecoveryReasonMergeConflicts         BlockedRecoveryReason = "merge_conflicts"
	BlockedRecoveryReasonStaleBase              BlockedRecoveryReason = "stale_base"
	BlockedRecoveryReasonMissingCurrentHeadCI   BlockedRecoveryReason = "missing_current_head_ci"
	BlockedRecoveryReasonPullRequestMaintenance BlockedRecoveryReason = "pull_request_maintenance"
)

type BlockedRecoveryDecision struct {
	Action      BlockedRecoveryAction
	Reason      BlockedRecoveryReason
	TargetState string
	Detail      string
}

func EvaluateBlockedRecovery(issue connector.Issue) BlockedRecoveryDecision {
	if normalizeState(issue.State) != "blocked" {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonNotBlocked, "")
	}
	if blockedRecoveryHumanOnly(issue) {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonHumanBlocker, "blocked reason requires a human")
	}
	if len(issue.BlockedBy) > 0 {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonDependencyBlocker, "dependency blockers must clear first")
	}
	if issue.PullRequest == nil {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonMissingPullRequest, "")
	}
	pr := issue.PullRequest
	if normalizePullRequestState(pr.State) != "open" {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonPullRequestNotOpen, "")
	}

	switch strings.ToLower(strings.TrimSpace(pr.MergeableState)) {
	case "dirty":
		return blockedRecoveryDecision(BlockedRecoveryActionRework, BlockedRecoveryReasonMergeConflicts, "linked PR has merge conflicts")
	case "behind":
		return blockedRecoveryDecision(BlockedRecoveryActionRework, BlockedRecoveryReasonStaleBase, "linked PR branch is behind the base branch")
	}

	text := blockedRecoveryText(issue)
	if blockedRecoveryNoCurrentHeadCI(pr) && (blockedRecoveryAgentText(text) || blockedRecoveryHasPriorSignal(pr)) {
		return blockedRecoveryDecision(BlockedRecoveryActionRework, BlockedRecoveryReasonMissingCurrentHeadCI, "latest PR head has no CI signal")
	}
	if blockedRecoveryAgentText(text) {
		return blockedRecoveryDecision(BlockedRecoveryActionRework, BlockedRecoveryReasonPullRequestMaintenance, "blocked reason describes agent-recoverable PR maintenance")
	}
	return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonNoRecoverableSignal, "")
}

func blockedRecoveryDecision(action BlockedRecoveryAction, reason BlockedRecoveryReason, detail string) BlockedRecoveryDecision {
	decision := BlockedRecoveryDecision{
		Action: action,
		Reason: reason,
		Detail: strings.TrimSpace(detail),
	}
	if action == BlockedRecoveryActionRework {
		decision.TargetState = autoPromoteReworkState
	}
	return decision
}

func (o *Orchestrator) recoverBlockedIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	transitioned := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, []string{blockedStatusState}) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		decision := EvaluateBlockedRecovery(issue)
		if decision.Action != BlockedRecoveryActionRework {
			continue
		}
		if !o.applyBlockedRecovery(ctx, state, issue, decision, now) {
			continue
		}
		transitioned[issueID] = struct{}{}
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func (o *Orchestrator) applyBlockedRecovery(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	decision BlockedRecoveryDecision,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	targetState := strings.TrimSpace(decision.TargetState)
	if targetState == "" {
		targetState = autoPromoteReworkState
	}
	if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
		if o.logger != nil {
			o.logger.Warn("blocked recovery transition failed", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "reason", decision.Reason, "error", err)
		}
		return false
	}

	body := blockedRecoveryComment(issue, targetState, decision)
	if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
		o.logger.Warn("blocked recovery comment failed", "issue_id", issueID, "identifier", issue.Identifier, "target_state", targetState, "reason", decision.Reason, "error", err)
	}

	delete(state.Blocked, issueID)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "blocked_recovery_transition",
		Message: "recovered " + issueLabel(issue) + " from " + issue.State + " to " + targetState + ": " + string(decision.Reason),
	})
	if o.logger != nil {
		o.logger.Info("blocked recovery transition", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "reason", decision.Reason)
	}
	return true
}

func blockedRecoveryComment(issue connector.Issue, targetState string, decision BlockedRecoveryDecision) string {
	var b strings.Builder
	b.WriteString("PR maintenance is agent-recoverable.")
	if strings.TrimSpace(issue.State) != "" && strings.TrimSpace(targetState) != "" {
		b.WriteString(" Moved this issue from ")
		b.WriteString(strings.TrimSpace(issue.State))
		b.WriteString(" to ")
		b.WriteString(strings.TrimSpace(targetState))
		b.WriteString(".")
	}
	b.WriteString("\n\nReason: ")
	b.WriteString(blockedRecoveryReasonLabel(decision.Reason))
	if decision.Detail != "" {
		b.WriteString(" (")
		b.WriteString(decision.Detail)
		b.WriteString(")")
	}
	if pr := issue.PullRequest; pr != nil && pr.Number > 0 {
		b.WriteString(fmt.Sprintf("\nLinked PR: #%d", pr.Number))
		if url := strings.TrimSpace(pr.URL); url != "" {
			b.WriteString(" ")
			b.WriteString(url)
		}
	}
	return b.String()
}

func blockedRecoveryReasonLabel(reason BlockedRecoveryReason) string {
	switch reason {
	case BlockedRecoveryReasonMergeConflicts:
		return "merge conflicts"
	case BlockedRecoveryReasonStaleBase:
		return "stale base"
	case BlockedRecoveryReasonMissingCurrentHeadCI:
		return "missing current-head CI"
	case BlockedRecoveryReasonPullRequestMaintenance:
		return "PR maintenance"
	default:
		return strings.ReplaceAll(string(reason), "_", " ")
	}
}

func blockedRecoveryHumanOnly(issue connector.Issue) bool {
	for _, label := range issue.Labels {
		if normalizeLabel(label) == "requires-human-review" {
			return true
		}
	}
	text := blockedRecoveryReasonText(issue)
	for _, phrase := range []string{
		"missing credential",
		"missing credentials",
		"credential",
		"secret",
		"token",
		"human approval",
		"explicit human approval",
		"requires human",
		"requires-human-review",
		"human review",
		"product direction",
		"ambiguous",
		"approval required",
		"manual approval",
		"unresolved human-requested",
		"human-requested review changes",
		"requested changes from human",
		"missing access",
		"permission",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func blockedRecoveryAgentText(text string) bool {
	for _, phrase := range []string{
		"merge conflict",
		"conflict with main",
		"conflicts with main",
		"stale base",
		"behind main",
		"rebase",
		"retrigger",
		"rerun check",
		"rerun ci",
		"no check-run",
		"no check run",
		"no check-runs",
		"no check runs",
		"missing check",
		"latest head has no",
		"push an empty commit",
		"agent maintenance",
		"pr maintenance",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func blockedRecoveryNoCurrentHeadCI(pr *connector.PullRequest) bool {
	if pr == nil || strings.TrimSpace(pr.HeadSHA) == "" || strings.TrimSpace(pr.CIStatus) != "" {
		return false
	}
	return pr.CheckRunCount == 0 && pr.StatusContextCount == 0
}

func blockedRecoveryHasPriorSignal(pr *connector.PullRequest) bool {
	if pr == nil {
		return false
	}
	headSHA := strings.TrimSpace(pr.HeadSHA)
	latestReviewSHA := strings.TrimSpace(pr.LatestCodexReviewCommitSHA)
	if headSHA == "" || latestReviewSHA == "" || strings.EqualFold(headSHA, latestReviewSHA) {
		return false
	}
	return strings.TrimSpace(pr.LatestCodexReviewState) != "" || pr.LatestCodexReviewSubmittedAt != nil
}

func blockedRecoveryText(issue connector.Issue) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(issue.BlockerReason+" "+issue.Description)), " "))
}

func blockedRecoveryReasonText(issue connector.Issue) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(issue.BlockerReason)), " "))
}
