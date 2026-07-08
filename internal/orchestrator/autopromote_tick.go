package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	autoPromoteSourceState           = "Human Review"
	autoPromoteMergingState          = "Merging"
	autoPromoteReworkState           = "Rework"
	defaultMergeWorkerStartupTimeout = 2 * time.Minute
	mergeWorkerProjectStateFull      = "project_state_capacity_full"
)

type autoPromoteTickResult struct {
	transitioned       map[string]struct{}
	dispatchCandidates []connector.Issue
}

type staleMergingPullRequestDecision struct {
	targetState string
	reason      string
}

type autoPromoteReworkLimitSummary struct {
	Limit        int
	Count        int
	ReasonCounts []autoPromoteReworkReasonCount
	Signature    autoPromoteReworkSignature
}

type autoPromoteReworkReasonCount struct {
	Reason string
	Count  int
}

type autoPromoteReworkSignature struct {
	PRNumber     int64
	HeadSHA      string
	FailedChecks []string
}

func (o *Orchestrator) autoPromoteHumanReviewIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) autoPromoteTickResult {
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	if !cfg.Enabled {
		if o.logger != nil {
			o.logger.Debug("auto promote skipped", "reason", AutoPromoteReasonDisabled)
		}
		return autoPromoteTickResult{}
	}

	result := autoPromoteTickResult{transitioned: map[string]struct{}{}}
	for _, issue := range o.autoPromoteEvaluationIssues(state, issues, cfg) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}

		summary := AutoPromoteSummaryFromIssue(issue)
		decision := EvaluateAutoPromote(issue, summary, cfg, now)
		if decision.Reason == AutoPromoteReasonValidatorMissing {
			validation, shouldComment, ok := o.validatorStageResult(ctx, issue)
			if !ok {
				o.startValidatorStage(ctx, issue, now)
				o.logAutoPromoteDecision(issue, decision, "")
				continue
			}
			summary.Validator = validation
			if shouldComment {
				o.commentValidatorResult(ctx, issue, validation)
				o.markValidatorResultCommented(ctx, issue)
			}
			decision = EvaluateAutoPromote(issue, summary, cfg, now)
		}
		if decision.Reason == AutoPromoteReasonCINotGreen &&
			o.retryTransientPullRequestChecks(ctx, state, issue, now, string(AutoPromoteReasonCINotGreen)) {
			o.logAutoPromoteDecision(issue, autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonCINotGreen), "")
			continue
		}
		if decision.Action == AutoPromoteActionPromote {
			issue, decision = o.hydrateAutoPromoteWorkpadDecision(ctx, issue, summary, cfg, now)
		}
		targetState := autoPromoteTargetState(decision.Action, cfg)
		if targetState == "" {
			o.logAutoPromoteDecision(issue, decision, "")
			continue
		}
		if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, targetState, now) {
			continue
		}
		o.recordAutoPromoteReworkHandoff(state, issue, summary, decision, targetState)
		result.transitioned[issueID] = struct{}{}
		o.clearAutoPromotedIssueDispatchMemory(state, issueID)
		if mergeWorkerIssue(promotedIssue(issue, targetState, now)) {
			promoted := promotedIssue(issue, targetState, now)
			o.recordMergeQueueEntered(state, promoted, now, "auto_promote")
			result.dispatchCandidates = append(result.dispatchCandidates, promoted)
			o.logMergeWorkerPickup(promoted, "auto_promote")
		}
	}
	if len(result.transitioned) == 0 {
		return autoPromoteTickResult{}
	}
	return result
}

func (o *Orchestrator) autoPromoteEvaluationIssues(
	state *State,
	issues []connector.Issue,
	cfg AutoPromoteConfig,
) []connector.Issue {
	out := issuesInStates(issues, []string{cfg.SourceState})
	seen := make(map[string]struct{}, len(out))
	for _, issue := range out {
		if issueID := strings.TrimSpace(issue.ID); issueID != "" {
			seen[issueID] = struct{}{}
		}
	}

	for _, issue := range issuesInStates(issues, o.cfg.ActiveStates) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if normalizeState(issue.State) == "todo" && !autoPromoteIssueCompleted(state, issueID) {
			continue
		}
		if _, ok := seen[issueID]; ok {
			continue
		}
		if !autoPromoteActiveGatePendingIssue(issue, state, o.cfg, cfg) {
			continue
		}
		out = append(out, cloneIssue(issue))
		seen[issueID] = struct{}{}
	}
	return out
}

func autoPromoteIssueCompleted(state *State, issueID string) bool {
	if state == nil {
		return false
	}
	_, ok := state.Completed[issueID]
	return ok
}

func autoPromoteActiveGatePendingIssue(
	issue connector.Issue,
	state *State,
	cfg Config,
	autoCfg AutoPromoteConfig,
) bool {
	autoCfg = normalizeAutoPromoteConfig(autoCfg)
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return false
	}
	if !stateIn(issue.State, cfg.ActiveStates) || stateIn(issue.State, cfg.TerminalStates) {
		return false
	}
	switch normalizeState(issue.State) {
	case normalizeState(autoCfg.SourceState), normalizeState(autoCfg.PassState), normalizeState(autoCfg.ReworkState):
		return false
	}
	if autoPromoteHumanReviewRequired(issue, autoCfg, autoCfg.Gate) {
		return false
	}
	if autoPromoteActiveDispatchInProgress(state, issueID) {
		return false
	}
	if state != nil {
		if completed, ok := state.Completed[issueID]; ok {
			return completedActiveFinalStateReviewEligible(completed.FinalState, autoCfg.SourceState) &&
				completedActiveIssueReadyForReview(issue, gateRequiresPullRequest(autoCfg.Gate))
		}
	}
	return issueHasOpenPullRequest(issue)
}

func autoPromoteActiveDispatchInProgress(state *State, issueID string) bool {
	if state == nil {
		return false
	}
	if _, ok := state.Running[issueID]; ok {
		return true
	}
	if _, ok := state.Claimed[issueID]; ok {
		return true
	}
	if _, ok := state.Retry[issueID]; ok {
		return true
	}
	return false
}

func issueHasOpenPullRequest(issue connector.Issue) bool {
	return issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) == "open"
}

func (o *Orchestrator) hydrateAutoPromoteWorkpadDecision(
	ctx context.Context,
	issue connector.Issue,
	summary AutoPromoteSummary,
	cfg AutoPromoteConfig,
	now time.Time,
) (connector.Issue, AutoPromoteDecision) {
	if len(issue.Comments) == 0 && strings.TrimSpace(issue.BlockerReason) == "" {
		reader, ok := o.connector.(connector.IssueCommentReader)
		if !ok {
			return issue, EvaluateAutoPromote(issue, summary, cfg, now)
		}
		comments, err := reader.FetchIssueComments(ctx, issue)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("fetch auto-promote workpad comments failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
			}
			return issue, autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonWorkpadHydrationUnavailable)
		}
		issue = cloneIssue(issue)
		issue.Comments = comments
	}
	return issue, EvaluateAutoPromote(issue, summary, cfg, now)
}

func (o *Orchestrator) reconcileStaleLinkedPullRequestIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	transitioned := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, o.cfg.ActiveStates) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" || issue.PullRequest == nil {
			continue
		}
		pullRequestState := normalizePullRequestState(issue.PullRequest.State)
		if pullRequestState != "open" && pullRequestState != "merged" {
			continue
		}
		if stateIn(issue.State, o.cfg.TerminalStates) {
			continue
		}
		if staleTodoPullRequestAlreadyActive(state, issueID) {
			continue
		}

		if pullRequestState == "merged" {
			summary := staleMergedPullRequestSummaryFromIssue(issue)
			decision := staleMergedPullRequestDecision(issue, summary)
			targetState := staleMergedPullRequestTargetState(decision, o.cfg.AutoPromote, o.cfg.TerminalStates)
			if targetState == "" {
				o.logAutoPromoteDecision(issue, decision, "")
				continue
			}
			if normalizeState(targetState) == normalizeState(issue.State) {
				continue
			}
			if !o.applyStaleMergedPullRequestDecision(ctx, state, issue, summary, decision, targetState, now) {
				continue
			}
			transitioned[issueID] = struct{}{}
			o.clearAutoPromotedIssueDispatchMemory(state, issueID)
			continue
		}

		if normalizeState(issue.State) != "todo" {
			continue
		}
		summary := AutoPromoteSummaryFromIssue(issue)
		if !summary.PullRequestPresent {
			continue
		}
		decision := staleTodoPullRequestDecision(issue, summary, o.cfg.AutoPromote, now)
		if decision.Action == AutoPromoteActionPromote {
			issue, decision = o.hydrateAutoPromoteWorkpadDecision(ctx, issue, summary, o.cfg.AutoPromote, now)
		}
		targetState := staleTodoPullRequestTargetState(decision, o.cfg.AutoPromote)
		if autoPromoteActiveGatePendingIssue(issue, state, o.cfg, o.cfg.AutoPromote) &&
			normalizeState(targetState) == normalizeState(normalizeAutoPromoteConfig(o.cfg.AutoPromote).SourceState) &&
			staleTodoPullRequestShouldStayActive(decision) {
			continue
		}
		if targetState == "" {
			o.logAutoPromoteDecision(issue, decision, "")
			continue
		}
		if !o.applyStaleTodoPullRequestDecision(ctx, state, issue, summary, decision, targetState, now) {
			continue
		}
		transitioned[issueID] = struct{}{}
		o.clearAutoPromotedIssueDispatchMemory(state, issueID)
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func (o *Orchestrator) reconcileStaleMergingPullRequestIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	transitioned := map[string]struct{}{}
	o.recordMergeQueueEntries(state, issues, now, "tracker")
	consumedRepositories := activeMergeWorkerRepositories(state)
	for _, issue := range staleMergingQueueIssues(issues, o.cfg) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		repository := mergeWorkerRepositoryKey(issue)
		if mergeWorkerRepositoryConsumed(consumedRepositories, repository) {
			continue
		}
		if staleMergingPullRequestDispatchActive(state, issueID) {
			consumedRepositories = consumeMergeWorkerRepository(consumedRepositories, repository)
			continue
		}
		decision := staleMergingPullRequestDecisionForIssue(issue, o.cfg.TerminalStates)
		if decision.reason == string(AutoPromoteReasonCINotGreen) &&
			o.retryTransientPullRequestChecks(ctx, state, issue, now, decision.reason) {
			consumedRepositories = consumeMergeWorkerRepository(consumedRepositories, repository)
			continue
		}
		if decision.targetState == "" {
			if strings.TrimSpace(decision.reason) != "" {
				o.logStaleMergingPullRequestDeferred(issue, decision)
			}
			consumedRepositories = consumeMergeWorkerRepository(consumedRepositories, repository)
			continue
		}
		if !o.applyStaleMergingPullRequestDecision(ctx, state, issue, decision, now) {
			consumedRepositories = consumeMergeWorkerRepository(consumedRepositories, repository)
			continue
		}
		transitioned[issueID] = struct{}{}
		o.clearAutoPromotedIssueDispatchMemory(state, issueID)
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func staleMergingPullRequestDecisionForIssue(issue connector.Issue, terminalStates []string) staleMergingPullRequestDecision {
	if strings.TrimSpace(issue.ID) == "" {
		return staleMergingPullRequestDecision{}
	}
	if closedCompletedIssueNeedsStatusReconciliation(issue, terminalStates) {
		return staleMergingPullRequestDecision{targetState: doneStateName(terminalStates), reason: "issue_closed_completed"}
	}
	pullRequest := issue.PullRequest
	if pullRequest == nil {
		return staleMergingPullRequestDecision{targetState: autoPromoteSourceState, reason: string(AutoPromoteReasonMissingPullRequest)}
	}
	pullRequestState := normalizePullRequestState(pullRequest.State)
	if pullRequestState == "" && pullRequestHydrationBlocksProgress(pullRequest) {
		return staleMergingPullRequestDecision{reason: string(AutoPromoteReasonPullRequestHydrationUnavailable)}
	}
	switch pullRequestState {
	case "merged":
		return staleMergingPullRequestDecision{targetState: doneStateName(terminalStates), reason: "pull_request_merged"}
	case "open":
		if pullRequestHydrationBlocksProgress(pullRequest) {
			return staleMergingPullRequestDecision{reason: string(AutoPromoteReasonPullRequestHydrationUnavailable)}
		}
		if pullRequest.Draft {
			return staleMergingPullRequestDecision{targetState: autoPromoteSourceState, reason: "draft_pull_request"}
		}
		if staleMergingCIRed(pullRequest.CIStatus) {
			return staleMergingPullRequestDecision{targetState: autoPromoteReworkState, reason: string(AutoPromoteReasonCINotGreen)}
		}
		return staleMergingPullRequestDecision{}
	default:
		return staleMergingPullRequestDecision{targetState: autoPromoteReworkState, reason: "pull_request_not_open"}
	}
}

func (o *Orchestrator) applyStaleMergingPullRequestDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	decision staleMergingPullRequestDecision,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.updateIssueStateByID(ctx, issueID, issue, decision.targetState, now, decision.reason); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"stale_merging_pr_reconciliation_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.reason,
				"target_state", decision.targetState,
				"error", err,
			)
		}
		return false
	}

	body := staleMergingPullRequestComment(issue, decision)
	if strings.TrimSpace(body) != "" {
		if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
			o.logger.Warn(
				"stale_merging_pr_reconciliation_comment_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.reason,
				"target_state", decision.targetState,
				"error", err,
			)
		}
	}

	o.logStaleMergingPullRequestDecision(issue, decision)
	if normalizeState(decision.targetState) == normalizeState(doneStateName(o.cfg.TerminalStates)) {
		o.recordMergeCompleted(state, issue, now, decision.targetState)
	} else {
		o.recordMergeFailed(state, issue, now, decision.reason, nil)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "stale_merging_pr_reconciled",
		Message: "reconciled stale Merging PR for " + issueLabel(issue) + " to " + decision.targetState + ": " + decision.reason,
	})
	return true
}

func staleMergingPullRequestComment(issue connector.Issue, decision staleMergingPullRequestDecision) string {
	var b strings.Builder
	b.WriteString("Reconciled this issue from Merging to ")
	b.WriteString(decision.targetState)
	b.WriteString(".")
	b.WriteString("\n\n- reason: ")
	b.WriteString(decision.reason)
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
	return b.String()
}

func (o *Orchestrator) logStaleMergingPullRequestDecision(issue connector.Issue, decision staleMergingPullRequestDecision) {
	if o.logger == nil {
		return
	}
	attrs := mergeWorkerLogAttrs(issue,
		"reason", decision.reason,
		"target_state", decision.targetState,
	)
	o.logger.Info("stale_merging_pr_reconciled", attrs...)
}

func (o *Orchestrator) logStaleMergingPullRequestDeferred(issue connector.Issue, decision staleMergingPullRequestDecision) {
	if o.logger == nil {
		return
	}
	attrs := mergeWorkerLogAttrs(issue, "reason", decision.reason)
	o.logger.Info("stale_merging_pr_reconciliation_deferred", attrs...)
}

func (o *Orchestrator) logMergeWorkerPickup(issue connector.Issue, source string) {
	if o.logger == nil || !mergeWorkerIssue(issue) {
		return
	}
	attrs := mergeWorkerLogAttrs(issue, "source", strings.TrimSpace(source))
	o.logger.Info("merge_worker_pickup", attrs...)
}

func (o *Orchestrator) logMergeWorkerAttempt(issue connector.Issue, attempt int, workerHost string) {
	if o.logger == nil || !mergeWorkerIssue(issue) {
		return
	}
	attrs := mergeWorkerLogAttrs(issue,
		"attempt", attempt,
		"worker_host", strings.TrimSpace(workerHost),
	)
	o.logger.Info("merge_worker_attempt", attrs...)
}

func (o *Orchestrator) logMergeWorkerSuccess(issue connector.Issue, finalState string) {
	if o.logger == nil || !mergeWorkerIssue(issue) {
		return
	}
	attrs := mergeWorkerLogAttrs(issue, "final_state", strings.TrimSpace(finalState))
	o.logger.Info("merge_worker_success", attrs...)
}

func (o *Orchestrator) logMergeWorkerFailure(issue connector.Issue, reason string, err error) {
	if o.logger == nil || !mergeWorkerIssue(issue) {
		return
	}
	attrs := mergeWorkerLogAttrs(issue, "reason", strings.TrimSpace(reason))
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logger.Warn("merge_worker_failure", attrs...)
}

func (o *Orchestrator) logMergeWorkerSlotWait(
	issue connector.Issue,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
) {
	if o.logger == nil || !mergeWorkerIssue(issue) {
		return
	}
	o.logger.Info("merge_worker_slot_wait", mergeWorkerSlotDecisionAttrs(issue, decision, projectStats)...)
}

func (o *Orchestrator) logMergeWorkerSlotAcquired(
	issue connector.Issue,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
	timing MergeTiming,
) {
	if o.logger == nil || !mergeWorkerIssue(issue) {
		return
	}
	attrs := mergeWorkerSlotDecisionAttrs(issue, decision, projectStats)
	attrs = append(attrs, mergeTimingAttrs(timing)...)
	o.logger.Info("merge_worker_slot_acquired", attrs...)
}

func (o *Orchestrator) logDispatchSlotWait(
	issue connector.Issue,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
) {
	if o.logger == nil {
		return
	}
	o.logger.Info("dispatch_slot_wait", mergeWorkerSlotDecisionAttrs(issue, decision, projectStats)...)
}

func (o *Orchestrator) recordDispatchSlotWait(
	state *State,
	issue connector.Issue,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
	now time.Time,
) {
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "dispatch_slot_wait",
		Message: dispatchSlotWaitMessage(issue, decision, projectStats),
	})
}

func dispatchSlotWaitMessage(
	issue connector.Issue,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
) string {
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = dispatchIssueFailureGlobalSlotUnavailable
	}
	return fmt.Sprintf(
		"dispatch waiting for %s state=%s reason=%s global_capacity=%d global_used=%d global_available=%d project_state_capacity=%d project_state_used=%d project_state_available=%d selected_project_id=%s selected_state=%s",
		issueLabel(issue),
		strings.TrimSpace(issue.State),
		reason,
		decision.GlobalCapacity,
		decision.GlobalUsed,
		decision.GlobalAvailable,
		projectStats.capacity,
		projectStats.used,
		projectStats.available,
		strings.TrimSpace(decision.SelectedProjectID),
		strings.TrimSpace(decision.SelectedState),
	)
}

func mergeWorkerSlotDecisionAttrs(
	issue connector.Issue,
	decision scheduler.DispatchGateDecision,
	projectStats projectStateSlotStats,
) []any {
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = dispatchIssueFailureGlobalSlotUnavailable
	}
	attrs := mergeWorkerLogAttrs(issue,
		"state", strings.TrimSpace(issue.State),
		"reason", reason,
		"global_capacity", decision.GlobalCapacity,
		"global_used", decision.GlobalUsed,
		"global_available", decision.GlobalAvailable,
		"project_state_capacity", projectStats.capacity,
		"project_state_used", projectStats.used,
		"project_state_available", projectStats.available,
		"lower_priority_running", decision.LowerPriorityRunning,
		"selected_project_id", strings.TrimSpace(decision.SelectedProjectID),
		"selected_state", strings.TrimSpace(decision.SelectedState),
		"ready_projects", decision.ReadyProjects,
		"running_projects", decision.RunningProjects,
	)
	return attrs
}

func (o *Orchestrator) failStalledMergeWorkerStarts(state *State, now time.Time) {
	if state == nil || state.Draining {
		return
	}
	timeout := o.mergeWorkerStartupTimeout()
	if timeout <= 0 {
		return
	}
	for _, issueID := range sortedKeys(state.Running) {
		running := state.Running[issueID]
		if !mergeWorkerIssue(running.Issue) || running.StartedAt.IsZero() || mergeWorkerStartupObserved(running) {
			continue
		}
		if now.Before(running.StartedAt.Add(timeout)) {
			continue
		}
		err := fmt.Errorf("merge worker did not report process or session startup within %s", timeout)
		o.logMergeWorkerFailure(running.Issue, "runner_startup_timeout", err)
		o.recordMergeFailed(state, running.Issue, now, "runner_startup_timeout", err)
		o.releaseGlobalDispatchSlot(running.globalSlot)
		if running.cancel != nil {
			running.cancel()
		}
		delete(state.Running, issueID)
		o.scheduleRetry(state, running.Issue, nextAttempt(running.Attempt), now, err.Error(), false, running.WorkerHost)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "merge_worker_startup_timeout",
			Message: "merge worker startup timed out for " + issueLabel(running.Issue) + ": " + err.Error(),
		})
	}
}

func (o *Orchestrator) mergeWorkerStartupTimeout() time.Duration {
	timeout := defaultMergeWorkerStartupTimeout
	if o != nil && o.cfg.PollInterval > 0 && o.cfg.PollInterval*2 > timeout {
		timeout = o.cfg.PollInterval * 2
	}
	return timeout
}

func mergeWorkerStartupObserved(running Running) bool {
	return strings.TrimSpace(running.ProcessIdentity) != "" ||
		strings.TrimSpace(running.WorkspacePath) != "" ||
		strings.TrimSpace(running.SessionID) != "" ||
		strings.TrimSpace(running.LastEvent) != "" ||
		!running.LastEventAt.IsZero() ||
		running.TurnCount > 0 ||
		len(running.RecentEvents) > 0 ||
		diffStatsPresent(running.DiffStats)
}

func mergeWorkerIssue(issue connector.Issue) bool {
	return normalizeState(issue.State) == normalizeState(autoPromoteMergingState)
}

func mergeWorkerLogAttrs(issue connector.Issue, attrs ...any) []any {
	out := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
	}
	if issue.PullRequest != nil {
		if issue.PullRequest.Number > 0 {
			out = append(out, "pull_request_number", issue.PullRequest.Number)
		}
		if repository := pullRequestRepository(issue); repository != "" {
			out = append(out, "repository", repository)
		}
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			out = append(out, "pull_request", url)
		}
		if mergeableState := strings.TrimSpace(issue.PullRequest.MergeableState); mergeableState != "" {
			out = append(out, "mergeable_state", strings.ToLower(mergeableState))
		}
		if ciStatus := strings.TrimSpace(issue.PullRequest.CIStatus); ciStatus != "" {
			out = append(out, "ci_status", ciStatus)
		}
		if headSHA := strings.TrimSpace(issue.PullRequest.HeadSHA); headSHA != "" {
			out = append(out, "head_sha", headSHA)
		}
		if baseSHA := strings.TrimSpace(issue.PullRequest.BaseSHA); baseSHA != "" {
			out = append(out, "base_sha", baseSHA)
		}
		if reason := pullRequestHydrationUnavailableReason(issue.PullRequest); reason != "" {
			out = append(out, "pull_request_hydration_reason", reason)
		}
		if reason := strings.TrimSpace(issue.PullRequest.HydrationDegradedReason); reason != "" {
			out = append(out, "pull_request_hydration_degraded_reason", reason)
		}
		if issue.PullRequest.HydrationNextRetryAt != nil && !issue.PullRequest.HydrationNextRetryAt.IsZero() {
			out = append(out, "pull_request_hydration_next_retry_at", issue.PullRequest.HydrationNextRetryAt.UTC().Format(time.RFC3339))
		}
	}
	return append(out, attrs...)
}

func staleMergingPullRequestDispatchActive(state *State, issueID string) bool {
	if state == nil {
		return false
	}
	if _, ok := state.Running[issueID]; ok {
		return true
	}
	if _, ok := state.Claimed[issueID]; ok {
		return true
	}
	if _, ok := state.Retry[issueID]; ok {
		return true
	}
	return false
}

func staleMergingQueueIssues(issues []connector.Issue, cfg Config) []connector.Issue {
	queue := issuesInStates(issues, []string{autoPromoteMergingState})
	sortIssuesForDispatch(queue, cfg.DispatchPriorityByState, cfg.DispatchPriorityByLabel)
	return queue
}

func activeMergeWorkerRepositories(state *State) map[string]struct{} {
	if state == nil {
		return nil
	}
	repositories := map[string]struct{}{}
	for _, running := range state.Running {
		repositories = consumeActiveMergeWorkerRepository(repositories, running.Issue)
	}
	for _, claimed := range state.Claimed {
		repositories = consumeActiveMergeWorkerRepository(repositories, claimed.Issue)
	}
	for _, retry := range state.Retry {
		repositories = consumeActiveMergeWorkerRepository(repositories, retry.Issue)
	}
	if len(repositories) == 0 {
		return nil
	}
	return repositories
}

func consumeActiveMergeWorkerRepository(repositories map[string]struct{}, issue connector.Issue) map[string]struct{} {
	if !mergeWorkerIssue(issue) {
		return repositories
	}
	return consumeMergeWorkerRepository(repositories, mergeWorkerRepositoryKey(issue))
}

func consumeMergeWorkerRepository(repositories map[string]struct{}, repository string) map[string]struct{} {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return repositories
	}
	if repositories == nil {
		repositories = map[string]struct{}{}
	}
	repositories[repository] = struct{}{}
	return repositories
}

func mergeWorkerRepositoryConsumed(repositories map[string]struct{}, repository string) bool {
	if repository == "" || len(repositories) == 0 {
		return false
	}
	_, ok := repositories[repository]
	return ok
}

func mergeWorkerRepositoryKey(issue connector.Issue) string {
	return strings.ToLower(strings.TrimSpace(pullRequestRepository(issue)))
}

func (o *Orchestrator) mergeWorkerDispatchCandidates(state *State, issues []connector.Issue) []connector.Issue {
	candidates := o.staleMergingQueueDispatchCandidates(state, issues)
	if len(candidates) == 0 {
		return nil
	}
	out := make([]connector.Issue, 0, len(candidates))
	selectedByState := map[string]int{}
	for _, issue := range candidates {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if staleMergingPullRequestDispatchActive(state, issueID) {
			continue
		}
		stateKey := normalizeState(issue.State)
		projectStats := o.projectStateSlotStats(issue, state)
		selected := selectedByState[stateKey]
		if selected > 0 {
			projectStats.used += selected
			projectStats.available -= selected
			if projectStats.available < 0 {
				projectStats.available = 0
			}
		}
		if projectStats.available <= 0 {
			o.logMergeWorkerSlotWait(
				issue,
				scheduler.DispatchGateDecision{Reason: mergeWorkerProjectStateFull},
				projectStats,
			)
			break
		}
		selectedByState[stateKey] = selected + 1
		o.clearAutoPromotedIssueDispatchMemory(state, issueID)
		o.logMergeWorkerPickup(issue, "stale_merging")
		out = append(out, issue)
	}
	return out
}

func (o *Orchestrator) staleMergingQueueDispatchCandidates(state *State, issues []connector.Issue) []connector.Issue {
	candidates := []connector.Issue{}
	consumedRepositories := activeMergeWorkerRepositories(state)
	for _, issue := range staleMergingQueueIssues(issues, o.cfg) {
		issueID := strings.TrimSpace(issue.ID)
		repository := mergeWorkerRepositoryKey(issue)
		if staleMergingPullRequestDispatchActive(state, issueID) {
			consumedRepositories = consumeMergeWorkerRepository(consumedRepositories, repository)
			continue
		}
		if mergeWorkerRepositoryConsumed(consumedRepositories, repository) {
			continue
		}
		if !staleMergingIssueReadyForDispatch(issue) {
			consumedRepositories = consumeMergeWorkerRepository(consumedRepositories, repository)
			continue
		}
		candidates = append(candidates, cloneIssue(issue))
		consumedRepositories = consumeMergeWorkerRepository(consumedRepositories, repository)
	}
	return candidates
}

func staleMergingIssueReadyForDispatch(issue connector.Issue) bool {
	if strings.TrimSpace(issue.ID) == "" || issue.PullRequest == nil {
		return false
	}
	pullRequest := issue.PullRequest
	if pullRequestHydrationBlocksProgress(pullRequest) {
		return false
	}
	if normalizePullRequestState(pullRequest.State) != "open" {
		return false
	}
	if pullRequest.Draft {
		return false
	}
	return !staleMergingCIRed(pullRequest.CIStatus)
}

func staleMergingCIRed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "red", "fail", "failed", "failure", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func staleTodoPullRequestAlreadyActive(state *State, issueID string) bool {
	if state == nil {
		return false
	}
	if _, ok := state.Running[issueID]; ok {
		return true
	}
	if _, ok := state.Claimed[issueID]; ok {
		return true
	}
	return false
}

func staleTodoPullRequestDecision(
	issue connector.Issue,
	summary AutoPromoteSummary,
	cfg AutoPromoteConfig,
	now time.Time,
) AutoPromoteDecision {
	if autoPromoteMergeConflicts(summary.MergeableState) {
		return autoPromoteDecision(AutoPromoteActionRework, AutoPromoteReasonMergeConflicts)
	}
	cfg = normalizeAutoPromoteConfig(cfg)
	if !cfg.Enabled {
		return autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonDisabled)
	}
	return EvaluateAutoPromote(issue, summary, cfg, now)
}

func staleTodoPullRequestShouldStayActive(decision AutoPromoteDecision) bool {
	return decision.Reason != AutoPromoteReasonWorkpadBlocker
}

func staleTodoPullRequestTargetState(decision AutoPromoteDecision, cfg AutoPromoteConfig) string {
	cfg = normalizeAutoPromoteConfig(cfg)
	if targetState := autoPromoteTargetState(decision.Action, cfg); targetState != "" {
		return targetState
	}
	switch decision.Reason {
	case AutoPromoteReasonMissingPullRequest:
		return ""
	default:
		return cfg.SourceState
	}
}

func staleMergedPullRequestSummaryFromIssue(issue connector.Issue) AutoPromoteSummary {
	summary := AutoPromoteSummary{
		LastActivityAt: autoPromoteLastActivityAt(issue),
		ArtifactStatus: artifactStatusFromIssue(issue, gate.DefaultArtifactStatusField),
	}
	if issue.PullRequest == nil {
		return summary
	}
	pullRequest := issue.PullRequest
	summary.PullRequestPresent = true
	summary.PullRequestURL = strings.TrimSpace(pullRequest.URL)
	summary.PullRequestHydrationUnavailableReason = pullRequestHydrationUnavailableReason(pullRequest)
	summary.PullRequestHydrationDegradedReason = pullRequestHydrationDegradedReason(pullRequest)
	summary.MergeableState = strings.ToLower(strings.TrimSpace(pullRequest.MergeableState))
	summary.CIStatus = strings.TrimSpace(pullRequest.CIStatus)
	summary.ReviewState = pullRequest.CodexReviewState
	summary.FailedChecks = autoPromoteFailedChecksFromPullRequest(pullRequest)
	summary.P1Findings = autoPromoteFindingsFromPullRequest(pullRequest)
	return summary
}

func staleMergedPullRequestDecision(issue connector.Issue, summary AutoPromoteSummary) AutoPromoteDecision {
	if strings.TrimSpace(summary.PullRequestHydrationUnavailableReason) != "" ||
		strings.TrimSpace(summary.PullRequestHydrationDegradedReason) != "" {
		return autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonPullRequestHydrationUnavailable)
	}
	if staleMergedPullRequestHasFailedCIEvidence(issue.PullRequest, summary) {
		decision := autoPromoteDecision(AutoPromoteActionRework, AutoPromoteReasonCINotGreen)
		decision.CIStatus = strings.TrimSpace(summary.CIStatus)
		return decision
	}
	return autoPromoteDecision(AutoPromoteActionPromote, AutoPromoteReasonPullRequestMerged)
}

func staleMergedPullRequestTargetState(decision AutoPromoteDecision, cfg AutoPromoteConfig, terminalStates []string) string {
	cfg = normalizeAutoPromoteConfig(cfg)
	switch decision.Reason {
	case AutoPromoteReasonCINotGreen:
		return cfg.ReworkState
	case AutoPromoteReasonPullRequestMerged:
		return doneStateName(terminalStates)
	case AutoPromoteReasonPullRequestHydrationUnavailable:
		return cfg.SourceState
	default:
		return ""
	}
}

func staleMergedPullRequestHasFailedCIEvidence(pullRequest *connector.PullRequest, summary AutoPromoteSummary) bool {
	if pullRequest == nil {
		return false
	}
	if staleMergingCIRed(pullRequest.CIStatus) {
		return true
	}
	return len(summary.FailedChecks) > 0
}

func (o *Orchestrator) applyStaleMergedPullRequestDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	targetState string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.updateIssueStateByID(ctx, issueID, issue, targetState, now, string(decision.Reason)); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"stale_merged_pr_reconciliation_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
		return false
	}

	body := staleMergedPullRequestComment(summary, decision, displayStateName(issue.State), targetState)
	if strings.TrimSpace(body) != "" {
		if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
			o.logger.Warn(
				"stale_merged_pr_reconciliation_comment_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
	}

	o.logStaleTodoPullRequestDecision(issue, decision, targetState)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "stale_merged_pr_reconciled",
		Message: "reconciled merged linked PR for " + issueLabel(issue) + " from " + displayStateName(issue.State) + " to " + targetState + ": " + string(decision.Reason),
	})
	return true
}

func staleMergedPullRequestComment(
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	sourceState string,
	targetState string,
) string {
	var b strings.Builder
	sourceState = displayStateName(sourceState)
	if sourceState == "" {
		sourceState = "active"
	}
	switch decision.Reason {
	case AutoPromoteReasonCINotGreen:
		b.WriteString("Reconciled this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to ")
		b.WriteString(targetState)
		b.WriteString(" because its merged linked PR has failing CI evidence.")
	case AutoPromoteReasonPullRequestHydrationUnavailable:
		b.WriteString("Reconciled this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to ")
		b.WriteString(targetState)
		b.WriteString(" because linked PR status hydration is unavailable.")
	case AutoPromoteReasonPullRequestMerged:
		b.WriteString("Reconciled this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to ")
		b.WriteString(targetState)
		b.WriteString(" because its linked PR is already merged.")
	default:
		return ""
	}

	b.WriteString("\n\n")
	b.WriteString("- reason: ")
	b.WriteString(string(decision.Reason))
	if summary.PullRequestURL != "" {
		b.WriteString("\n- pull request: ")
		b.WriteString(summary.PullRequestURL)
	}
	if summary.MergeableState != "" {
		b.WriteString("\n- mergeable_state: ")
		b.WriteString(summary.MergeableState)
	}
	if decision.CIStatus != "" {
		b.WriteString("\n- ci_status: ")
		b.WriteString(decision.CIStatus)
	}
	if failedChecks := strings.Join(summary.FailedChecks, ", "); failedChecks != "" {
		b.WriteString("\n- failed_checks: ")
		b.WriteString(failedChecks)
	}
	if summary.PullRequestHydrationUnavailableReason != "" {
		b.WriteString("\n- pull_request_hydration_unavailable_reason: ")
		b.WriteString(summary.PullRequestHydrationUnavailableReason)
	}
	if summary.PullRequestHydrationDegradedReason != "" {
		b.WriteString("\n- pull_request_hydration_degraded_reason: ")
		b.WriteString(summary.PullRequestHydrationDegradedReason)
	}
	return b.String()
}

func (o *Orchestrator) applyStaleTodoPullRequestDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	targetState string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.updateIssueStateByID(ctx, issueID, issue, targetState, now, string(decision.Reason)); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"stale_todo_pr_reconciliation_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
		return false
	}

	body := autoPromoteComment(summary, decision, displayStateName(issue.State), targetState)
	if strings.TrimSpace(body) != "" {
		if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
			o.logger.Warn(
				"stale_todo_pr_reconciliation_comment_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
	}

	o.logStaleTodoPullRequestDecision(issue, decision, targetState)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "stale_todo_pr_reconciled",
		Message: "reconciled stale linked PR for " + issueLabel(issue) + " from " + displayStateName(issue.State) + " to " + targetState + ": " + string(decision.Reason),
	})
	return true
}

func (o *Orchestrator) logStaleTodoPullRequestDecision(issue connector.Issue, decision AutoPromoteDecision, targetState string) {
	if o.logger == nil {
		return
	}
	attrs := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
		"reason", decision.Reason,
		"target_state", targetState,
	}
	if issue.PullRequest != nil {
		if issue.PullRequest.Number > 0 {
			attrs = append(attrs, "pull_request_number", issue.PullRequest.Number)
		}
		if mergeableState := strings.TrimSpace(issue.PullRequest.MergeableState); mergeableState != "" {
			attrs = append(attrs, "mergeable_state", strings.ToLower(mergeableState))
		}
		if ciStatus := strings.TrimSpace(issue.PullRequest.CIStatus); ciStatus != "" {
			attrs = append(attrs, "ci_status", ciStatus)
		}
		if failedChecks := strings.Join(autoPromoteFailedChecksFromPullRequest(issue.PullRequest), ", "); failedChecks != "" {
			attrs = append(attrs, "failed_checks", failedChecks)
		}
		if reason := pullRequestHydrationUnavailableReason(issue.PullRequest); reason != "" {
			attrs = append(attrs, "pull_request_hydration_unavailable_reason", reason)
		}
		if reason := pullRequestHydrationDegradedReason(issue.PullRequest); reason != "" {
			attrs = append(attrs, "pull_request_hydration_degraded_reason", reason)
		}
	}
	if decision.WorkpadBlocker != "" {
		attrs = append(attrs, "workpad_blocker", decision.WorkpadBlocker)
	}
	o.logger.Info("stale_todo_pr_reconciled", attrs...)
}

func (o *Orchestrator) clearAutoPromotedIssueDispatchMemory(state *State, issueID string) {
	if state == nil {
		return
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.Blocked, issueID)
	delete(state.Completed, issueID)
}

func (o *Orchestrator) startValidatorStage(ctx context.Context, issue connector.Issue, now time.Time) {
	if o.validator == nil {
		if o.logger != nil {
			o.logger.Warn(
				"validator stage skipped",
				"issue_id", strings.TrimSpace(issue.ID),
				"identifier", issue.Identifier,
				"reason", "validator runner unavailable",
			)
		}
		return
	}

	identity := validatorStageIdentityForIssue(issue)
	if identity.Key == "" {
		return
	}
	if _, _, ok := o.validatorStageResult(ctx, issue); ok {
		return
	}

	o.validatorMu.Lock()
	if o.validatorRuns == nil {
		o.validatorRuns = map[string]struct{}{}
	}
	if o.validatorResults == nil {
		o.validatorResults = map[string]validatorStageResult{}
	}
	if o.validatorFailures == nil {
		o.validatorFailures = map[string]validatorStageFailure{}
	}
	if _, ok := o.validatorRuns[identity.Key]; ok {
		o.validatorMu.Unlock()
		return
	}
	if _, ok := o.validatorResults[identity.Key]; ok {
		o.validatorMu.Unlock()
		return
	}
	if failure, ok := o.validatorFailures[identity.Key]; ok && failure.NextRetryAt.After(now) {
		o.validatorMu.Unlock()
		if o.logger != nil {
			o.logger.Debug(
				"validator stage backoff active",
				"issue_id", identity.IssueID,
				"identifier", issue.Identifier,
				"head_sha", identity.HeadSHA,
				"retry_at", failure.NextRetryAt,
				"attempt", failure.Attempt,
			)
		}
		return
	}
	o.validatorRuns[identity.Key] = struct{}{}
	o.validatorWG.Add(1)
	o.validatorMu.Unlock()

	selectorContext := o.cfg.SelectorContext
	retryConfig := Config{
		MaxRetryBackoff:       o.cfg.MaxRetryBackoff,
		FailureRetryBaseDelay: o.cfg.FailureRetryBaseDelay,
	}
	go func() {
		defer o.validatorWG.Done()

		result, err := o.validator.Validate(ctx, ValidatorRequest{
			Issue:           issue,
			StartedAt:       now.UTC(),
			SelectorContext: selectorContext,
		})

		completedAt := o.clockNow().UTC()
		o.validatorMu.Lock()
		if err != nil {
			delete(o.validatorRuns, identity.Key)
			attempt := o.validatorFailures[identity.Key].Attempt + 1
			o.validatorFailures[identity.Key] = validatorStageFailure{
				Attempt:     attempt,
				NextRetryAt: completedAt.Add(validatorStageRetryDelay(retryConfig, attempt)),
				Error:       err.Error(),
			}
			failure := o.validatorFailures[identity.Key]
			o.validatorMu.Unlock()
			if o.logger != nil {
				o.logger.Warn(
					"validator stage failed",
					"issue_id", strings.TrimSpace(issue.ID),
					"identifier", issue.Identifier,
					"head_sha", identity.HeadSHA,
					"retry_at", failure.NextRetryAt,
					"attempt", attempt,
					"error", err,
				)
			}
			return
		}
		o.validatorMu.Unlock()
		o.recordValidatorVerdict(ctx, issue, identity, result, completedAt)

		o.validatorMu.Lock()
		delete(o.validatorRuns, identity.Key)
		delete(o.validatorFailures, identity.Key)
		o.validatorResults[identity.Key] = validatorStageResult{Result: result}
		o.validatorMu.Unlock()
	}()
}

func (o *Orchestrator) validatorStageResult(ctx context.Context, issue connector.Issue) (gate.ValidatorResult, bool, bool) {
	identity := validatorStageIdentityForIssue(issue)
	if identity.Key == "" {
		return gate.ValidatorResult{}, false, false
	}
	o.validatorMu.Lock()
	result, ok := o.validatorResults[identity.Key]
	o.validatorMu.Unlock()
	if !ok {
		var loaded bool
		result, loaded = o.loadValidatorVerdict(ctx, issue, identity)
		if !loaded {
			return gate.ValidatorResult{}, false, false
		}
		o.validatorMu.Lock()
		if o.validatorResults == nil {
			o.validatorResults = map[string]validatorStageResult{}
		}
		o.validatorResults[identity.Key] = result
		o.validatorMu.Unlock()
	}
	return result.Result, !result.Commented, true
}

func (o *Orchestrator) markValidatorResultCommented(ctx context.Context, issue connector.Issue) {
	identity := validatorStageIdentityForIssue(issue)
	if identity.Key == "" {
		return
	}
	o.validatorMu.Lock()
	result, ok := o.validatorResults[identity.Key]
	if !ok {
		o.validatorMu.Unlock()
		return
	}
	result.Commented = true
	o.validatorResults[identity.Key] = result
	o.validatorMu.Unlock()
	o.markValidatorVerdictCommented(ctx, identity)
}

func (o *Orchestrator) commentValidatorResult(ctx context.Context, issue connector.Issue, result gate.ValidatorResult) {
	commenter, ok := o.connector.(connector.PullRequestCommenter)
	if !ok {
		return
	}
	repository := pullRequestRepository(issue)
	number := pullRequestNumber(issue)
	if repository == "" || number <= 0 {
		return
	}
	if err := commenter.CreatePullRequestComment(ctx, repository, number, validatorResultComment(result)); err != nil && o.logger != nil {
		o.logger.Warn(
			"validator result comment failed",
			"issue_id", strings.TrimSpace(issue.ID),
			"identifier", issue.Identifier,
			"pull_request", number,
			"error", err,
		)
	}
}

func validatorResultComment(result gate.ValidatorResult) string {
	var b strings.Builder
	b.WriteString("Validator verdict: ")
	b.WriteString(strings.TrimSpace(result.Verdict))
	if result.Score > 0 {
		b.WriteString("\n- score: ")
		b.WriteString(fmt.Sprintf("%.2f", result.Score))
	}
	if strings.TrimSpace(result.Summary) != "" {
		b.WriteString("\n- summary: ")
		b.WriteString(strings.TrimSpace(result.Summary))
	}
	if len(result.Findings) > 0 {
		b.WriteString("\n\nFindings:")
		for _, finding := range result.Findings {
			b.WriteString("\n- ")
			b.WriteString(autoPromoteFindingText(AutoPromoteFinding{
				Body: finding.Body,
				URL:  finding.URL,
				Path: finding.Path,
				Line: finding.Line,
			}))
		}
	}
	return b.String()
}

func pullRequestRepository(issue connector.Issue) string {
	if strings.TrimSpace(issue.PRRepository) != "" {
		return strings.TrimSpace(issue.PRRepository)
	}
	identifier := strings.TrimSpace(issue.Identifier)
	repository, _, ok := strings.Cut(identifier, "#")
	if ok {
		return strings.TrimSpace(repository)
	}
	return ""
}

func pullRequestNumber(issue connector.Issue) int {
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		return issue.PullRequest.Number
	}
	if issue.PRNumber != nil {
		return *issue.PRNumber
	}
	return 0
}

type validatorStageIdentity struct {
	Key     string
	IssueID string
	HeadSHA string
}

func validatorStageIdentityForIssue(issue connector.Issue) validatorStageIdentity {
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return validatorStageIdentity{}
	}
	headSHA := ""
	if issue.PullRequest != nil {
		headSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	}
	if headSHA == "" && issue.PullRequest != nil {
		headSHA = strings.TrimSpace(issue.PullRequest.BranchName)
	}
	if headSHA == "" {
		headSHA = strings.TrimSpace(issue.BranchName)
	}
	if headSHA == "" {
		return validatorStageIdentity{}
	}
	return validatorStageIdentity{
		Key:     issueID + ":" + headSHA,
		IssueID: issueID,
		HeadSHA: headSHA,
	}
}

func validatorStageRetryDelay(cfg Config, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	maxRetryBackoff := cfg.MaxRetryBackoff
	if maxRetryBackoff <= 0 {
		maxRetryBackoff = defaultMaxRetryBackoff
	}
	failureRetryBaseDelay := cfg.FailureRetryBaseDelay
	if failureRetryBaseDelay <= 0 {
		failureRetryBaseDelay = defaultFailureRetryBaseDelay
	}
	delay := failureRetryBaseDelay
	for range attempt - 1 {
		if delay >= maxRetryBackoff || delay > maxRetryBackoff/2 {
			return maxRetryBackoff
		}
		delay *= 2
	}
	if delay > maxRetryBackoff {
		return maxRetryBackoff
	}
	return delay
}

func (o *Orchestrator) loadValidatorVerdict(ctx context.Context, issue connector.Issue, identity validatorStageIdentity) (validatorStageResult, bool) {
	if o.validatorMemo == nil {
		return validatorStageResult{}, false
	}
	verdict, err := o.validatorMemo.ValidatorVerdict(ctx, store.ValidatorVerdictKey{
		ProjectID: o.workflowMetricsProjectID(),
		IssueID:   identity.IssueID,
		HeadSHA:   identity.HeadSHA,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return validatorStageResult{}, false
		}
		if o.logger != nil {
			o.logger.Warn(
				"validator verdict lookup failed",
				"issue_id", identity.IssueID,
				"identifier", issue.Identifier,
				"head_sha", identity.HeadSHA,
				"error", err,
			)
		}
		return validatorStageResult{}, false
	}
	return validatorStageResult{
		Result: gate.ValidatorResult{
			Submitted: verdict.Submitted,
			Verdict:   verdict.Verdict,
			Score:     verdict.Score,
			Summary:   verdict.Summary,
			Findings:  gateFindingsFromStore(verdict.Findings),
		},
		Commented: verdict.Commented,
	}, true
}

func (o *Orchestrator) recordValidatorVerdict(
	ctx context.Context,
	issue connector.Issue,
	identity validatorStageIdentity,
	result gate.ValidatorResult,
	recordedAt time.Time,
) {
	if o.validatorMemo == nil {
		return
	}
	if recordedAt.IsZero() {
		recordedAt = o.clockNow().UTC()
	}
	if err := o.validatorMemo.RecordValidatorVerdict(ctx, store.ValidatorVerdict{
		ProjectID:  o.workflowMetricsProjectID(),
		IssueID:    identity.IssueID,
		HeadSHA:    identity.HeadSHA,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
		PRNumber:   workflowMetricsPRNumber(issue),
		Submitted:  result.Submitted,
		Verdict:    result.Verdict,
		Score:      result.Score,
		Summary:    result.Summary,
		Findings:   storeFindingsFromGate(result.Findings),
		RecordedAt: recordedAt,
		UpdatedAt:  recordedAt,
	}); err != nil && o.logger != nil {
		o.logger.Warn(
			"validator verdict persistence failed",
			"issue_id", identity.IssueID,
			"identifier", issue.Identifier,
			"head_sha", identity.HeadSHA,
			"error", err,
		)
	}
}

func (o *Orchestrator) markValidatorVerdictCommented(ctx context.Context, identity validatorStageIdentity) {
	if o.validatorMemo == nil {
		return
	}
	if err := o.validatorMemo.MarkValidatorVerdictCommented(ctx, store.ValidatorVerdictKey{
		ProjectID: o.workflowMetricsProjectID(),
		IssueID:   identity.IssueID,
		HeadSHA:   identity.HeadSHA,
	}, o.clockNow().UTC()); err != nil && !errors.Is(err, store.ErrNotFound) && o.logger != nil {
		o.logger.Warn(
			"validator verdict comment marker failed",
			"issue_id", identity.IssueID,
			"head_sha", identity.HeadSHA,
			"error", err,
		)
	}
}

func (o *Orchestrator) clockNow() time.Time {
	if o != nil && o.now != nil {
		return o.now()
	}
	return time.Now()
}

func storeFindingsFromGate(findings []gate.Finding) []store.ValidatorFinding {
	out := make([]store.ValidatorFinding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, store.ValidatorFinding{
			Severity: finding.Severity,
			Body:     finding.Body,
			URL:      finding.URL,
			Path:     finding.Path,
			Line:     finding.Line,
		})
	}
	return out
}

func gateFindingsFromStore(findings []store.ValidatorFinding) []gate.Finding {
	out := make([]gate.Finding, 0, len(findings))
	for _, finding := range findings {
		out = append(out, gate.Finding{
			Severity: finding.Severity,
			Body:     finding.Body,
			URL:      finding.URL,
			Path:     finding.Path,
			Line:     finding.Line,
		})
	}
	return out
}

func AutoPromoteSummaryFromIssue(issue connector.Issue) AutoPromoteSummary {
	summary := AutoPromoteSummary{
		LastActivityAt: autoPromoteLastActivityAt(issue),
		ArtifactStatus: artifactStatusFromIssue(issue, gate.DefaultArtifactStatusField),
	}
	if issue.PullRequest == nil {
		return summary
	}

	pullRequest := issue.PullRequest
	summary.PullRequestHydrationUnavailableReason = pullRequestHydrationUnavailableReason(pullRequest)
	summary.PullRequestHydrationDegradedReason = pullRequestHydrationDegradedReason(pullRequest)
	if normalizePullRequestState(pullRequest.State) != "open" {
		return summary
	}
	summary.PullRequestPresent = true
	summary.PullRequestURL = strings.TrimSpace(pullRequest.URL)
	summary.MergeableState = strings.ToLower(strings.TrimSpace(pullRequest.MergeableState))
	summary.CIStatus = pullRequest.CIStatus
	summary.ReviewState = pullRequest.CodexReviewState
	summary.FailedChecks = autoPromoteFailedChecksFromPullRequest(pullRequest)
	summary.P1Findings = autoPromoteFindingsFromPullRequest(pullRequest)
	return summary
}

func pullRequestHydrationUnavailableReason(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return ""
	}
	return strings.TrimSpace(pullRequest.HydrationUnavailableReason)
}

func pullRequestHydrationDegradedReason(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return ""
	}
	return strings.TrimSpace(pullRequest.HydrationDegradedReason)
}

func pullRequestHydrationBlocksProgress(pullRequest *connector.PullRequest) bool {
	return pullRequestHydrationUnavailableReason(pullRequest) != "" ||
		pullRequestHydrationDegradedReason(pullRequest) != ""
}

func autoPromoteLastActivityAt(issue connector.Issue) *time.Time {
	var latest *time.Time
	latest = latestTime(latest, issue.StageUpdatedAt)
	latest = latestTime(latest, issue.UpdatedAt)
	if issue.PullRequest != nil {
		latest = latestTime(latest, issue.PullRequest.ActivityAt)
		latest = latestTime(latest, issue.PullRequest.CodexReviewSubmittedAt)
	}
	return latest
}

func latestTime(current *time.Time, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.After(*current) {
		value := *candidate
		return &value
	}
	return current
}

func autoPromoteFindingsFromPullRequest(pullRequest *connector.PullRequest) []AutoPromoteFinding {
	if pullRequest == nil {
		return nil
	}
	findings := make([]AutoPromoteFinding, 0, len(pullRequest.CodexReviewFindings))
	for _, finding := range pullRequest.CodexReviewFindings {
		findings = append(findings, AutoPromoteFinding{
			Body: finding.Body,
			URL:  finding.URL,
			Path: finding.Path,
			Line: finding.Line,
		})
	}
	if len(findings) == 0 && strings.EqualFold(strings.TrimSpace(pullRequest.CodexReviewState), "P1") {
		findings = append(findings, AutoPromoteFinding{
			Body: "Codex review reported P1 findings.",
			URL:  strings.TrimSpace(pullRequest.URL),
		})
	}
	return findings
}

func autoPromoteFailedChecksFromPullRequest(pullRequest *connector.PullRequest) []string {
	if pullRequest == nil {
		return nil
	}
	allChecks := append([]connector.PullRequestCheck{}, pullRequest.SlowChecks...)
	allChecks = append(allChecks, pullRequest.RequiredCheckFailures...)
	allChecks = append(allChecks, pullRequest.TransientFailedChecks...)
	checks := make([]string, 0, len(allChecks))
	for _, check := range allChecks {
		if !autoPromoteCheckFailed(check) {
			continue
		}
		checks = append(checks, check.Name)
	}
	return uniqueStrings(checks)
}

func autoPromoteStaleSuccessfulChecks(pullRequest *connector.PullRequest) []string {
	if pullRequest == nil {
		return nil
	}
	checks := make([]string, 0, len(pullRequest.StaleSuccessfulChecks))
	for _, check := range pullRequest.StaleSuccessfulChecks {
		if name := strings.TrimSpace(check.Name); name != "" {
			checks = append(checks, name)
		}
	}
	return uniqueStrings(checks)
}

func autoPromoteCheckFailed(check connector.PullRequestCheck) bool {
	switch strings.ToLower(strings.TrimSpace(check.Conclusion)) {
	case "failure", "failed", "error", "timed_out", "startup_failure", "action_required", "cancelled", "canceled", "missing", "skipped", "neutral":
		return true
	default:
		return false
	}
}

func autoPromoteTargetState(action AutoPromoteAction, cfg AutoPromoteConfig) string {
	cfg = normalizeAutoPromoteConfig(cfg)
	switch action {
	case AutoPromoteActionPromote:
		return cfg.PassState
	case AutoPromoteActionRework:
		return cfg.ReworkState
	default:
		return ""
	}
}

func promotedIssue(issue connector.Issue, targetState string, now time.Time) connector.Issue {
	promoted := cloneIssue(issue)
	promoted.State = targetState
	promotedAt := now.UTC()
	promoted.StageUpdatedAt = &promotedAt
	return promoted
}

func (o *Orchestrator) applyAutoPromoteDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	targetState string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	transitionReason := string(decision.Reason)
	body := autoPromoteComment(summary, decision, displayStateName(issue.State), targetState)
	if decision.Action == AutoPromoteActionRework {
		limit, err := o.autoPromoteReworkLimit(ctx, issue, summary)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn(
					"auto promote rework limit check failed",
					"issue_id", issueID,
					"identifier", issue.Identifier,
					"action", decision.Action,
					"reason", decision.Reason,
					"target_state", targetState,
					"error", err,
				)
			}
			return false
		}
		if limit.Exceeded() {
			targetState = blockedStatusState
			transitionReason = "rework_limit"
			body = autoPromoteReworkLimitComment(summary, decision, displayStateName(issue.State), limit)
		}
	}

	if err := o.updateIssueStateByID(ctx, issueID, issue, targetState, now, transitionReason); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"auto promote transition failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"action", decision.Action,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
		return false
	}

	if strings.TrimSpace(body) != "" {
		if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
			o.logger.Warn(
				"auto promote comment failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"action", decision.Action,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
	}

	o.logAutoPromoteDecision(issue, decision, targetState)
	sourceState := displayStateName(issue.State)
	if sourceState == "" {
		sourceState = autoPromoteSourceState
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "auto_promote_transition",
		Message: "auto-promoted " + issueLabel(issue) + " from " + sourceState + " to " + targetState,
	})
	return true
}

func (s autoPromoteReworkLimitSummary) Exceeded() bool {
	return s.Limit > 0 && s.Count >= s.Limit
}

func (o *Orchestrator) autoPromoteReworkLimit(
	ctx context.Context,
	issue connector.Issue,
	summary AutoPromoteSummary,
) (autoPromoteReworkLimitSummary, error) {
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	limitSummary := autoPromoteReworkLimitSummary{
		Limit:     cfg.ReworkLimit,
		Signature: autoPromoteReworkSignatureFromIssue(issue, summary),
	}
	if cfg.ReworkLimit <= 0 {
		return limitSummary, nil
	}
	if normalizeState(issue.State) == normalizeState(cfg.ReworkState) {
		return limitSummary, nil
	}
	reader, ok := o.workflowMetrics.(WorkflowMetricsTimelineReader)
	if !ok || reader == nil {
		return limitSummary, errors.New("workflow metrics timeline reader unavailable")
	}

	timeline, err := reader.IssueWorkflowTimeline(ctx, store.IssueIdentity{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
	})
	if err != nil {
		return limitSummary, err
	}
	entries := autoPromoteReworkLaneEntries(timeline.Events, cfg.ReworkState)
	entries = autoPromoteReworkLaneEntriesForSignature(entries, limitSummary.Signature)
	limitSummary.Count = len(entries)
	limitSummary.ReasonCounts = autoPromoteReworkReasonCounts(entries)
	return limitSummary, nil
}

func autoPromoteReworkLaneEntries(events []store.WorkflowPhaseEvent, reworkState string) []store.WorkflowPhaseEvent {
	reworkState = normalizeState(reworkState)
	entries := make([]store.WorkflowPhaseEvent, 0, len(events))
	for _, event := range events {
		if event.PhaseType != store.WorkflowPhaseTypeLane {
			continue
		}
		if normalizeState(event.PhaseName) != reworkState {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(event.Status), "entered") {
			continue
		}
		entries = append(entries, event)
	}
	return entries
}

func autoPromoteReworkReasonCounts(events []store.WorkflowPhaseEvent) []autoPromoteReworkReasonCount {
	counts := map[string]int{}
	order := make([]string, 0, len(events))
	for _, event := range events {
		reason := strings.TrimSpace(event.Reason)
		if reason == "" {
			reason = "state_transition"
		}
		if _, ok := counts[reason]; !ok {
			order = append(order, reason)
		}
		counts[reason]++
	}

	out := make([]autoPromoteReworkReasonCount, 0, len(order))
	for _, reason := range order {
		out = append(out, autoPromoteReworkReasonCount{Reason: reason, Count: counts[reason]})
	}
	return out
}

func autoPromoteReworkLaneEntriesForSignature(
	events []store.WorkflowPhaseEvent,
	signature autoPromoteReworkSignature,
) []store.WorkflowPhaseEvent {
	if signature.empty() {
		return events
	}
	matching := make([]store.WorkflowPhaseEvent, 0, len(events))
	for _, event := range events {
		if autoPromoteReworkSignatureMatches(signature, autoPromoteReworkSignatureFromEvent(event)) {
			matching = append(matching, event)
		}
	}
	return matching
}

func autoPromoteReworkSignatureFromIssue(issue connector.Issue, summary AutoPromoteSummary) autoPromoteReworkSignature {
	signature := autoPromoteReworkSignature{}
	if issue.PRNumber != nil && *issue.PRNumber > 0 {
		signature.PRNumber = int64(*issue.PRNumber)
	}
	if issue.PullRequest != nil {
		if issue.PullRequest.Number > 0 {
			signature.PRNumber = int64(issue.PullRequest.Number)
		}
		signature.HeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	}
	signature.FailedChecks = autoPromoteCanonicalChecks(summary.FailedChecks)
	if len(signature.FailedChecks) == 0 && issue.PullRequest != nil {
		signature.FailedChecks = autoPromoteCanonicalChecks(autoPromoteFailedChecksFromPullRequest(issue.PullRequest))
	}
	return signature
}

func autoPromoteReworkSignatureFromEvent(event store.WorkflowPhaseEvent) autoPromoteReworkSignature {
	signature := autoPromoteReworkSignature{}
	if event.PRNumber != nil && *event.PRNumber > 0 {
		signature.PRNumber = *event.PRNumber
	}
	if metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON); ok {
		if metadata.PullRequest != nil {
			if metadata.PullRequest.Number > 0 {
				signature.PRNumber = metadata.PullRequest.Number
			}
			signature.HeadSHA = strings.TrimSpace(metadata.PullRequest.HeadSHA)
			signature.FailedChecks = autoPromoteCanonicalChecks(metadata.PullRequest.FailedChecks)
		}
	}
	return signature
}

func autoPromoteReworkSignatureMatches(current autoPromoteReworkSignature, event autoPromoteReworkSignature) bool {
	if current.empty() {
		return true
	}
	if current.PRNumber > 0 && event.PRNumber > 0 && current.PRNumber != event.PRNumber {
		return false
	}
	if current.HeadSHA != "" && event.HeadSHA != current.HeadSHA {
		return false
	}
	if len(current.FailedChecks) > 0 && !slices.Equal(current.FailedChecks, event.FailedChecks) {
		return false
	}
	if current.HeadSHA != "" || len(current.FailedChecks) > 0 {
		return true
	}
	return current.PRNumber <= 0 || event.PRNumber <= 0 || current.PRNumber == event.PRNumber
}

func (s autoPromoteReworkSignature) empty() bool {
	return s.PRNumber <= 0 && s.HeadSHA == "" && len(s.FailedChecks) == 0
}

func autoPromoteCanonicalChecks(checks []string) []string {
	checks = uniqueStrings(checks)
	if len(checks) == 0 {
		return nil
	}
	slices.Sort(checks)
	return checks
}

func (o *Orchestrator) recordAutoPromoteReworkHandoff(
	state *State,
	issue connector.Issue,
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	targetState string,
) {
	if state == nil || normalizeState(targetState) != normalizeState(autoPromoteReworkState) {
		return
	}
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return
	}
	if state.PriorAttempts == nil {
		state.PriorAttempts = map[string]runpkg.PriorAttempt{}
	}
	state.PriorAttempts[issueID] = runpkg.PriorAttempt{
		Source:    "auto_promote",
		Reason:    string(decision.Reason),
		Validator: summary.Validator,
	}
}

func (o *Orchestrator) logAutoPromoteDecision(issue connector.Issue, decision AutoPromoteDecision, targetState string) {
	if o.logger == nil {
		return
	}

	attrs := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
		"action", decision.Action,
		"reason", decision.Reason,
	}
	if decision.CIStatus != "" {
		attrs = append(attrs, "ci_status", decision.CIStatus)
	}
	if issue.PullRequest != nil {
		if issue.PullRequest.Number > 0 {
			attrs = append(attrs, "pull_request_number", issue.PullRequest.Number)
		}
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			attrs = append(attrs, "pull_request", url)
		}
		if mergeableState := strings.TrimSpace(issue.PullRequest.MergeableState); mergeableState != "" {
			attrs = append(attrs, "mergeable_state", mergeableState)
		}
		if reason := pullRequestHydrationUnavailableReason(issue.PullRequest); reason != "" {
			attrs = append(attrs, "pull_request_hydration_reason", reason)
		}
		if reason := strings.TrimSpace(issue.PullRequest.HydrationDegradedReason); reason != "" {
			attrs = append(attrs, "pull_request_hydration_degraded_reason", reason)
		}
		if issue.PullRequest.HydrationNextRetryAt != nil && !issue.PullRequest.HydrationNextRetryAt.IsZero() {
			attrs = append(attrs, "pull_request_hydration_next_retry_at", issue.PullRequest.HydrationNextRetryAt.UTC().Format(time.RFC3339))
		}
		if failedChecks := strings.Join(autoPromoteFailedChecksFromPullRequest(issue.PullRequest), ", "); failedChecks != "" {
			attrs = append(attrs, "failed_checks", failedChecks)
		}
		if staleSuccessfulChecks := strings.Join(autoPromoteStaleSuccessfulChecks(issue.PullRequest), ", "); staleSuccessfulChecks != "" {
			attrs = append(attrs,
				"ci_anomaly", "stale_successful_check_run",
				"stale_successful_checks", staleSuccessfulChecks,
				"ci_anomaly_action", "treated_completed_successful_check_runs_as_passed",
			)
		}
	}
	if decision.QuietRemaining > 0 {
		attrs = append(attrs, "quiet_remaining", decision.QuietRemaining)
	}
	if decision.WorkpadBlocker != "" {
		attrs = append(attrs, "workpad_blocker", decision.WorkpadBlocker)
	}
	if targetState != "" {
		attrs = append(attrs, "target_state", targetState)
		o.logger.Info("auto promote decision", attrs...)
		return
	}
	o.logger.Info("auto promote decision", attrs...)
}

func autoPromoteComment(
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	sourceState string,
	targetState string,
) string {
	var b strings.Builder
	sourceState = displayStateName(sourceState)
	if sourceState == "" {
		sourceState = autoPromoteSourceState
	}
	switch targetState {
	case autoPromoteMergingState:
		b.WriteString("Auto-promoted this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to Merging.")
	case autoPromoteReworkState:
		b.WriteString("Auto-promote routed this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to Rework")
		switch decision.Reason {
		case AutoPromoteReasonCINotGreen:
			b.WriteString(": current-head CI is failing")
		case AutoPromoteReasonMergeConflicts:
			b.WriteString(": linked PR has merge conflicts")
		}
		b.WriteString(".")
	case autoPromoteSourceState:
		b.WriteString("Reconciled this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to Human Review because it already has a linked PR.")
	default:
		return ""
	}

	b.WriteString("\n\n")
	b.WriteString("- reason: ")
	b.WriteString(string(decision.Reason))
	if summary.PullRequestURL != "" {
		b.WriteString("\n- pull request: ")
		b.WriteString(summary.PullRequestURL)
	}
	if summary.MergeableState != "" {
		b.WriteString("\n- mergeable_state: ")
		b.WriteString(summary.MergeableState)
	}
	if decision.CIStatus != "" {
		b.WriteString("\n- ci_status: ")
		b.WriteString(decision.CIStatus)
	}
	if failedChecks := strings.Join(summary.FailedChecks, ", "); failedChecks != "" {
		b.WriteString("\n- failed_checks: ")
		b.WriteString(failedChecks)
	}

	if len(decision.Findings) > 0 {
		b.WriteString("\n\nFindings:")
		for _, finding := range decision.Findings {
			b.WriteString("\n- ")
			b.WriteString(autoPromoteFindingText(finding))
		}
	}

	return b.String()
}

func autoPromoteReworkLimitComment(
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	sourceState string,
	limit autoPromoteReworkLimitSummary,
) string {
	var b strings.Builder
	sourceState = displayStateName(sourceState)
	if sourceState == "" {
		sourceState = autoPromoteSourceState
	}
	b.WriteString("Auto-promote routed this issue from ")
	b.WriteString(sourceState)
	b.WriteString(" to Blocked because the Rework limit was reached.")
	b.WriteString("\n\n")
	b.WriteString("- rework_limit: ")
	b.WriteString(strconv.Itoa(limit.Limit))
	b.WriteString("\n- prior_rework_transitions: ")
	b.WriteString(strconv.Itoa(limit.Count))
	b.WriteString("\n- current_rework_reason: ")
	b.WriteString(string(decision.Reason))
	if reasons := autoPromoteReworkReasonsText(limit.ReasonCounts); reasons != "" {
		b.WriteString("\n- repeated_rework_reasons: ")
		b.WriteString(reasons)
	}
	if summary.PullRequestURL != "" {
		b.WriteString("\n- pull request: ")
		b.WriteString(summary.PullRequestURL)
	}
	if summary.MergeableState != "" {
		b.WriteString("\n- mergeable_state: ")
		b.WriteString(summary.MergeableState)
	}
	if decision.CIStatus != "" {
		b.WriteString("\n- ci_status: ")
		b.WriteString(decision.CIStatus)
	}
	if failedChecks := strings.Join(summary.FailedChecks, ", "); failedChecks != "" {
		b.WriteString("\n- failed_checks: ")
		b.WriteString(failedChecks)
	}

	if len(decision.Findings) > 0 {
		b.WriteString("\n\nCurrent findings:")
		for _, finding := range decision.Findings {
			b.WriteString("\n- ")
			b.WriteString(autoPromoteFindingText(finding))
		}
	}

	return b.String()
}

func autoPromoteReworkReasonsText(counts []autoPromoteReworkReasonCount) string {
	parts := make([]string, 0, len(counts))
	for _, count := range counts {
		if strings.TrimSpace(count.Reason) == "" || count.Count <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s x%d", count.Reason, count.Count))
	}
	return strings.Join(parts, ", ")
}

func autoPromoteFindingText(finding AutoPromoteFinding) string {
	body := strings.Join(strings.Fields(finding.Body), " ")
	if body == "" {
		body = "P1 finding"
	}
	if finding.Path != "" && finding.Line > 0 {
		body = fmt.Sprintf("%s (%s:%d)", body, finding.Path, finding.Line)
	} else if finding.Path != "" {
		body = body + " (" + finding.Path + ")"
	}
	if finding.URL != "" {
		body = body + " " + finding.URL
	}
	return body
}
