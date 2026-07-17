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

const reworkBreakerStageUpdateSkew = time.Second

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

type reworkBreakerPark struct {
	Event     store.WorkflowPhaseEvent
	Reason    AutoPromoteReason
	PRNumber  int64
	HeadSHA   string
	Signature string
	Timeline  store.WorkflowTimeline
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
	autoPromoteCfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	for _, issue := range issuesInStates(issues, []string{blockedStatusState}) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if park, ok := o.latestReworkBreakerPark(ctx, issue); autoPromoteCfg.Enabled && ok {
			if reworkBreakerAutoUnparkConsumed(park.Timeline, park.Signature) ||
				!reworkBreakerAutoUnparkReady(issue, park) ||
				!o.reworkBreakerAutoPromoteGateReady(ctx, state, issue, autoPromoteCfg, now) {
				continue
			}
			if !o.applyReworkBreakerAutoUnpark(ctx, state, issue, park, autoPromoteCfg.PassState, now) {
				continue
			}
			transitioned[issueID] = struct{}{}
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

func (o *Orchestrator) latestReworkBreakerPark(ctx context.Context, issue connector.Issue) (reworkBreakerPark, bool) {
	timeline, ok := o.issueWorkflowTimeline(ctx, issue)
	if !ok {
		return reworkBreakerPark{}, false
	}
	for index := len(timeline.Events) - 1; index >= 0; index-- {
		event := timeline.Events[index]
		if event.PhaseType != store.WorkflowPhaseTypeLane || !strings.EqualFold(strings.TrimSpace(event.Status), "entered") {
			continue
		}
		if normalizeState(event.PhaseName) != normalizeState(blockedStatusState) || !strings.EqualFold(strings.TrimSpace(event.Reason), "rework_limit") {
			return reworkBreakerPark{}, false
		}
		metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if !ok || metadata.PullRequest == nil {
			return reworkBreakerPark{}, false
		}
		reason, ok := reworkBreakerParkReason(timeline.Events, index, metadata)
		if !ok {
			return reworkBreakerPark{}, false
		}
		prNumber := metadata.PullRequest.Number
		headSHA := strings.TrimSpace(metadata.PullRequest.HeadSHA)
		if prNumber <= 0 || headSHA == "" {
			return reworkBreakerPark{}, false
		}
		return reworkBreakerPark{
			Event:     event,
			Reason:    reason,
			PRNumber:  prNumber,
			HeadSHA:   headSHA,
			Signature: fmt.Sprintf("pr=%d;head=%s", prNumber, headSHA),
			Timeline:  timeline,
		}, true
	}
	return reworkBreakerPark{}, false
}

func reworkBreakerParkReason(
	events []store.WorkflowPhaseEvent,
	parkIndex int,
	parkMetadata workflowLaneMetadata,
) (AutoPromoteReason, bool) {
	if parkMetadata.ReworkBreaker != nil {
		reason := AutoPromoteReason(strings.TrimSpace(parkMetadata.ReworkBreaker.Reason))
		return reason, reworkBreakerReasonEligible(reason)
	}
	for index := parkIndex - 1; index >= 0; index-- {
		event := events[index]
		if event.PhaseType != store.WorkflowPhaseTypeLane || !strings.EqualFold(strings.TrimSpace(event.Status), "entered") {
			continue
		}
		if normalizeState(event.PhaseName) == normalizeState(blockedStatusState) {
			return "", false
		}
		if normalizeState(event.PhaseName) != normalizeState(autoPromoteReworkState) {
			continue
		}
		reason := AutoPromoteReason(strings.TrimSpace(event.Reason))
		if !reworkBreakerReasonEligible(reason) {
			continue
		}
		metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if !ok || !reworkBreakerPullRequestMetadataEqual(metadata.PullRequest, parkMetadata.PullRequest) {
			continue
		}
		return reason, true
	}
	return "", false
}

func reworkBreakerReasonEligible(reason AutoPromoteReason) bool {
	switch reason {
	case AutoPromoteReasonCINotGreen, AutoPromoteReasonMergeConflicts:
		return true
	default:
		return false
	}
}

func reworkBreakerPullRequestMetadataEqual(left, right *workflowLanePullRequestMetadata) bool {
	return left != nil && right != nil &&
		left.Number > 0 && left.Number == right.Number &&
		strings.TrimSpace(left.HeadSHA) != "" && strings.TrimSpace(left.HeadSHA) == strings.TrimSpace(right.HeadSHA)
}

func reworkBreakerAutoUnparkReady(issue connector.Issue, park reworkBreakerPark) bool {
	if normalizeState(issue.State) != normalizeState(blockedStatusState) || issue.Closed || reworkBreakerIssueHeld(issue) {
		return false
	}
	if issue.StageUpdatedAt != nil && issue.StageUpdatedAt.After(park.Event.StartedAt.Add(reworkBreakerStageUpdateSkew)) {
		return false
	}
	pr := issue.PullRequest
	if pr == nil || pr.Draft || normalizePullRequestState(pr.State) != "open" {
		return false
	}
	if pr.Number != int(park.PRNumber) || strings.TrimSpace(pr.HeadSHA) != park.HeadSHA {
		return false
	}
	if pullRequestHydrationUnavailableReason(pr) != "" || pullRequestHydrationDegradedReason(pr) != "" {
		return false
	}
	return reworkBreakerCIGreen(pr) &&
		strings.EqualFold(strings.TrimSpace(pr.MergeableState), "clean") &&
		len(autoPromoteFindingsFromPullRequest(pr)) == 0
}

func reworkBreakerIssueHeld(issue connector.Issue) bool {
	issue = issueWithTextDependencyRefs(issue)
	if len(issue.BlockedBy) > 0 || strings.TrimSpace(issue.BlockerReason) != "" || blockedRecoveryHumanOnly(issue) {
		return true
	}
	signal := issue.WorkpadSignal
	return signal != nil && (signal.Invalid != nil || strings.TrimSpace(signal.HumanAction) != "" || len(signal.Blockers) > 0 || strings.EqualFold(strings.TrimSpace(signal.Status), "blocked"))
}

func reworkBreakerCIGreen(pr *connector.PullRequest) bool {
	if pr == nil || len(pr.RunningChecks) > 0 || len(pr.RequiredCheckFailures) > 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(pr.CIStatus)) {
	case "pass", "passed", "passing", "success", "successful":
		return true
	default:
		return false
	}
}

func (o *Orchestrator) reworkBreakerAutoPromoteGateReady(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	cfg AutoPromoteConfig,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	summary := AutoPromoteSummaryFromIssue(issue)
	summary.CompletedFinalState = autoPromoteCompletedFinalState(state, issueID)
	summary.AutomatedReviewWaitExpired = autoPromoteReviewWaitExpired(state, issueID, cfg, now)
	decision := EvaluateAutoPromote(issue, summary, cfg, now)
	if decision.Reason == AutoPromoteReasonValidatorMissing {
		validation, _, ok := o.validatorStageResult(ctx, issue)
		if !ok {
			return false
		}
		summary.Validator = validation
		decision = EvaluateAutoPromote(issue, summary, cfg, now)
	}
	return decision.Action == AutoPromoteActionPromote
}

func reworkBreakerAutoUnparkConsumed(timeline store.WorkflowTimeline, signature string) bool {
	for _, event := range timeline.Events {
		metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if ok && workflowLaneMetadataHasActionSignature(metadata, workflowActionReworkBreakerAutoUnpark, signature) {
			return true
		}
	}
	return false
}

func (o *Orchestrator) applyReworkBreakerAutoUnpark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park reworkBreakerPark,
	targetState string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	targetState = strings.TrimSpace(targetState)
	if targetState == "" {
		targetState = autoPromoteMergingState
	}
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionReworkBreakerAutoUnpark, park.Signature)
	if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issueID, issue, targetState, now, "rework_breaker_auto_unpark", metadata); err != nil {
		if o.logger != nil {
			o.logger.Warn("rework breaker auto-unpark transition failed", "issue_id", issueID, "identifier", issue.Identifier, "reason", park.Reason, "signature", park.Signature, "error", err)
		}
		return false
	}

	body := reworkBreakerAutoUnparkComment(issue, park, targetState)
	if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
		o.logger.Warn("rework breaker auto-unpark comment failed", "issue_id", issueID, "identifier", issue.Identifier, "reason", park.Reason, "signature", park.Signature, "error", err)
	}

	o.clearAutoPromotedIssueDispatchMemory(state, issueID)
	promoted := promotedIssue(issue, targetState, now)
	if mergeWorkerIssue(promoted) {
		o.recordMergeQueueEntered(state, promoted, now, "rework_breaker_auto_unpark")
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "rework_breaker_auto_unpark",
		Message: "auto-unparked " + issueLabel(issue) + " from Blocked to " + targetState + ": " + string(park.Reason),
	})
	if o.logger != nil {
		o.logger.Info("rework breaker auto-unpark", "issue_id", issueID, "identifier", issue.Identifier, "reason", park.Reason, "signature", park.Signature, "head_sha", park.HeadSHA)
	}
	return true
}

func reworkBreakerAutoUnparkComment(issue connector.Issue, park reworkBreakerPark, targetState string) string {
	var b strings.Builder
	b.WriteString("Auto-unparked this breaker-parked issue from Blocked to ")
	b.WriteString(strings.TrimSpace(targetState))
	b.WriteString(" after the original merge-path condition cleared.")
	b.WriteString("\n\nPark reason: ")
	b.WriteString(string(park.Reason))
	b.WriteString(fmt.Sprintf("\nLinked PR: #%d", park.PRNumber))
	if issue.PullRequest != nil && strings.TrimSpace(issue.PullRequest.URL) != "" {
		b.WriteString(" ")
		b.WriteString(strings.TrimSpace(issue.PullRequest.URL))
	}
	b.WriteString("\nParked head: ")
	b.WriteString(park.HeadSHA)
	if issue.PullRequest != nil {
		b.WriteString("\nCurrent-head CI: ")
		b.WriteString(strings.TrimSpace(issue.PullRequest.CIStatus))
		b.WriteString("\nMergeable state: ")
		b.WriteString(strings.TrimSpace(issue.PullRequest.MergeableState))
	}
	b.WriteString("\n\nThis is the only automatic unpark permitted for this PR head. If the breaker parks it again without a new head, a human must review it.")
	return b.String()
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
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issueID, issue, targetState, now, "blocked_recovery", metadata); err != nil {
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
