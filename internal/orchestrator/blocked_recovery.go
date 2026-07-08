package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
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

type BlockedRecoveryKind string

const (
	BlockedRecoveryKindConflict    BlockedRecoveryKind = "conflict"
	BlockedRecoveryKindNoCI        BlockedRecoveryKind = "no-ci"
	BlockedRecoveryKindRerun       BlockedRecoveryKind = "rerun"
	BlockedRecoveryKindPriorSignal BlockedRecoveryKind = "prior-signal"
)

type BlockedRecoveryDecision struct {
	Action      BlockedRecoveryAction
	Reason      BlockedRecoveryReason
	Kind        BlockedRecoveryKind
	TargetState string
	Detail      string
}

func EvaluateBlockedRecovery(issue connector.Issue) BlockedRecoveryDecision {
	issue = issueWithTextDependencyRefs(issue)
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

	if autoPromoteMergeConflicts(pr.MergeableState) {
		return blockedRecoveryDecision(BlockedRecoveryActionRework, BlockedRecoveryReasonMergeConflicts, "linked PR has merge conflicts")
	}
	switch strings.ToLower(strings.TrimSpace(pr.MergeableState)) {
	case "behind":
		return blockedRecoveryDecision(BlockedRecoveryActionRework, BlockedRecoveryReasonStaleBase, "linked PR branch is behind the base branch")
	}

	text := blockedRecoveryText(issue)
	agentText := blockedRecoveryAgentText(text)
	priorSignal := blockedRecoveryHasPriorSignal(pr)
	if blockedRecoveryNoCurrentHeadCI(pr) && (agentText || priorSignal) {
		kind := BlockedRecoveryKindNoCI
		if !agentText && priorSignal {
			kind = BlockedRecoveryKindPriorSignal
		}
		return blockedRecoveryDecisionWithKind(BlockedRecoveryActionRework, BlockedRecoveryReasonMissingCurrentHeadCI, kind, "latest PR head has no CI signal")
	}
	if agentText {
		return blockedRecoveryDecisionWithKind(BlockedRecoveryActionRework, BlockedRecoveryReasonPullRequestMaintenance, BlockedRecoveryKindRerun, "blocked reason describes agent-recoverable PR maintenance")
	}
	return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonNoRecoverableSignal, "")
}

func blockedRecoveryDecision(action BlockedRecoveryAction, reason BlockedRecoveryReason, detail string) BlockedRecoveryDecision {
	return blockedRecoveryDecisionWithKind(action, reason, blockedRecoveryKindForReason(reason), detail)
}

func blockedRecoveryDecisionWithKind(action BlockedRecoveryAction, reason BlockedRecoveryReason, kind BlockedRecoveryKind, detail string) BlockedRecoveryDecision {
	decision := BlockedRecoveryDecision{
		Action: action,
		Reason: reason,
		Kind:   kind,
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
		if o.issueHasStickyBlockReason(ctx, state, issue) {
			continue
		}
		decision := EvaluateBlockedRecovery(issue)
		if decision.Action != BlockedRecoveryActionRework {
			continue
		}
		signature := blockedRecoverySignature(issue, decision)
		if match, ok := o.workflowTimelineLaneActionSignature(ctx, issue, "blocked_recovery", workflowActionBlockedRecovery, signature); ok {
			o.handleBlockedRecoveryExhausted(ctx, state, issue, decision, signature, match, now)
			continue
		}
		if !o.applyBlockedRecovery(ctx, state, issue, decision, signature, now) {
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
	signature string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	targetState := strings.TrimSpace(decision.TargetState)
	if targetState == "" {
		targetState = autoPromoteReworkState
	}
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionBlockedRecovery, signature)
	if err := o.updateIssueStateByIDWithMetadata(ctx, issueID, issue, targetState, now, "blocked_recovery", metadata); err != nil {
		if o.logger != nil {
			o.logger.Warn("blocked recovery transition failed", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "reason", decision.Reason, "signature", signature, "error", err)
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
		o.logger.Info("blocked recovery transition", "issue_id", issueID, "identifier", issue.Identifier, "from_state", issue.State, "target_state", targetState, "reason", decision.Reason, "signature", signature)
	}
	return true
}

func (o *Orchestrator) handleBlockedRecoveryExhausted(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	decision BlockedRecoveryDecision,
	signature string,
	match workflowTimelineMetadataMatch,
	now time.Time,
) {
	issueID := strings.TrimSpace(issue.ID)
	targetState := strings.TrimSpace(decision.TargetState)
	if targetState == "" {
		targetState = autoPromoteReworkState
	}
	if o.logger != nil {
		o.logger.Info(
			"blocked recovery exhausted",
			"issue_id", issueID,
			"identifier", issue.Identifier,
			"signature", signature,
			"matched_event_id", match.Event.ID,
			"matched_event_reason", match.Event.Reason,
			"matched_event_phase", match.Event.PhaseName,
			"would_target_state", targetState,
			"would_reason", decision.Reason,
		)
	}
	if _, ok := o.workflowTimelineActionSignature(ctx, issue, workflowActionBlockedRecoveryExhausted, signature); ok {
		return
	}
	body := blockedRecoveryExhaustedComment(issue, targetState, decision, signature, match)
	if err := o.connector.CreateComment(ctx, issueID, body); err != nil {
		if o.logger != nil {
			o.logger.Warn("blocked recovery exhausted comment failed", "issue_id", issueID, "identifier", issue.Identifier, "signature", signature, "error", err)
		}
		return
	}
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionBlockedRecoveryExhausted, signature)
	o.recordWorkflowReviewAction(ctx, issue, "blocked_recovery_exhausted", "blocked_recovery_exhausted", now, metadata)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "blocked_recovery_exhausted",
		Message: "blocked recovery exhausted for " + issueLabel(issue) + ": " + signature,
	})
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

func blockedRecoveryExhaustedComment(
	issue connector.Issue,
	targetState string,
	decision BlockedRecoveryDecision,
	signature string,
	match workflowTimelineMetadataMatch,
) string {
	var b strings.Builder
	b.WriteString("Blocked recovery already moved this issue to ")
	b.WriteString(strings.TrimSpace(targetState))
	b.WriteString(" for the same PR maintenance signature. It is back in Blocked without a new recovery signal, so a human needs to review it before Detent tries again.")
	b.WriteString("\n\nReason: ")
	b.WriteString(blockedRecoveryReasonLabel(decision.Reason))
	if decision.Detail != "" {
		b.WriteString(" (")
		b.WriteString(decision.Detail)
		b.WriteString(")")
	}
	b.WriteString("\nSignature: ")
	b.WriteString(signature)
	b.WriteString("\nMatched recovery event: ")
	b.WriteString(workflowTimelineEventLabel(match.Event))
	if pr := issue.PullRequest; pr != nil && pr.Number > 0 {
		b.WriteString(fmt.Sprintf("\nLinked PR: #%d", pr.Number))
		if url := strings.TrimSpace(pr.URL); url != "" {
			b.WriteString(" ")
			b.WriteString(url)
		}
	}
	return b.String()
}

func workflowTimelineEventLabel(event store.WorkflowPhaseEvent) string {
	parts := []string{}
	if event.ID > 0 {
		parts = append(parts, fmt.Sprintf("id=%d", event.ID))
	}
	if phase := strings.TrimSpace(event.PhaseName); phase != "" {
		parts = append(parts, "phase="+phase)
	}
	if reason := strings.TrimSpace(event.Reason); reason != "" {
		parts = append(parts, "reason="+reason)
	}
	if status := strings.TrimSpace(event.Status); status != "" {
		parts = append(parts, "status="+status)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, " ")
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

func blockedRecoveryKindForReason(reason BlockedRecoveryReason) BlockedRecoveryKind {
	switch reason {
	case BlockedRecoveryReasonMergeConflicts, BlockedRecoveryReasonStaleBase:
		return BlockedRecoveryKindConflict
	case BlockedRecoveryReasonMissingCurrentHeadCI:
		return BlockedRecoveryKindNoCI
	case BlockedRecoveryReasonPullRequestMaintenance:
		return BlockedRecoveryKindRerun
	default:
		return ""
	}
}

func blockedRecoverySignature(issue connector.Issue, decision BlockedRecoveryDecision) string {
	kind := decision.Kind
	if kind == "" {
		kind = blockedRecoveryKindForReason(decision.Reason)
	}
	prNumber := 0
	headSHA := ""
	if issue.PRNumber != nil {
		prNumber = *issue.PRNumber
	}
	if pr := issue.PullRequest; pr != nil {
		if pr.Number > 0 {
			prNumber = pr.Number
		}
		headSHA = strings.TrimSpace(pr.HeadSHA)
	}
	return fmt.Sprintf("kind=%s;pr=%d;head=%s", strings.TrimSpace(string(kind)), prNumber, headSHA)
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
