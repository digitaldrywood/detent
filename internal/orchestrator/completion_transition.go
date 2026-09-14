package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
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
	// The project's own lanes are read at most once per tick, and only once
	// an item has actually reached the promotion: the configured lane is the
	// target until the workflow says the project does not have it.
	workflow := o.tickWorkflowStates(ctx)
	o.forgetPromotionFallbackLogs(state)
	result := autoPromoteTickResult{transitioned: map[string]struct{}{}}
	for _, issue := range issues {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if _, running := state.Running[issueID]; running {
			continue
		}
		completed, ok := state.Completed[issueID]
		if !ok {
			continue
		}
		if gateRequiresPullRequest(cfg.Gate) {
			// The issue in hand was read when the attempt was dispatched, so
			// the change the attempt produced is newer than it. A tracker
			// that reviews changes of its own is asked now, for this item
			// only, because that answer is what a native item is judged on.
			issue = o.hydrateCompletedChangeReview(ctx, issue)
			var hydrated bool
			issue, hydrated = o.hydrateAutoPromoteReviewThreads(ctx, issue)
			if !hydrated {
				result.transitioned[issueID] = struct{}{}
				continue
			}
		}
		if normalizeState(issue.State) == normalizeState(cfg.ReworkState) &&
			normalizeState(completed.Issue.State) != normalizeState(issue.State) {
			continue
		}
		targetState, missing := completedActiveReviewTargetState(
			issue,
			completed.FinalState,
			completed.CompletionKind,
			o.cfg.ActiveStates,
			o.cfg.TerminalStates,
			cfg,
		)
		if targetState == "" {
			// A completed item that is not ready for review used to be
			// skipped in silence, which is what hid the whole native
			// promotion for seven dogfood runs. The fact that is missing is
			// named once per item.
			o.logCompletedActiveReviewNotReady(issue, missing)
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

		states, known := workflow()
		targetState, substituted := o.resolveCompletedActiveReviewTarget(states, known, issue, targetState)
		if targetState == "" {
			// The project offers nowhere to promote into. The item is left
			// where it is, which is what happened before the fallback
			// existed, and the reason is in the log once.
			continue
		}

		result.transitioned[issueID] = struct{}{}
		// Auto-promote speaks the configured review, pass and rework
		// vocabulary. A project that does not have the configured review
		// lane does not have the rest of it either, so a substituted target
		// is applied as a plain transition instead.
		if !substituted {
			if direct, promoted := o.tryDirectCompletedActiveAutoPromote(ctx, state, issue, targetState, completed.FinalState, cfg, now); direct {
				if completedActiveReviewThreadsKeepParked(issue, promoted.State, cfg) {
					continue
				}
				if normalizeState(issue.State) == normalizeState(promoted.State) {
					result.dispatchCandidates = append(result.dispatchCandidates, promoted)
				} else if mergeWorkerIssue(promoted) {
					o.recordMergeQueueEntered(state, promoted, now, "completed_active_auto_promote")
					result.dispatchCandidates = append(result.dispatchCandidates, promoted)
					o.logMergeWorkerPickup(promoted, "completed_active_auto_promote")
				}
				o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, promoted.State)
				continue
			}
		}

		if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "completed_active_review_transition", laneMutationAcceptCompletion); err != nil {
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

func completedActiveReviewThreadsKeepParked(issue connector.Issue, targetState string, cfg AutoPromoteConfig) bool {
	return gateRequiresPullRequest(cfg.Gate) &&
		normalizeState(issue.State) == normalizeState(targetState) &&
		issue.PullRequest != nil &&
		len(issue.PullRequest.UnresolvedReviewThreads) > 0
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

		if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "artifact_gate_wait_review_reconciliation", laneMutationAcceptCompletion); err != nil {
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
	if normalizeState(reviewState) != normalizeState(cfg.SourceState) {
		return false, connector.Issue{}
	}

	summary := AutoPromoteSummaryFromIssue(issue)
	summary.CompletedFinalState = completedFinalState
	summary.OperationalCompletionAccepted = autoPromoteOperationalCompletionAccepted(state, issue.ID)
	unresolvedReviewThreads := gateRequiresPullRequest(cfg.Gate) && len(summary.UnresolvedReviewThreads) > 0
	if !unresolvedReviewThreads && (!cfg.Enabled || cfg.QuietDuration != 0) {
		return false, connector.Issue{}
	}
	decision := autoPromoteDecision(AutoPromoteActionRework, AutoPromoteReasonUnresolvedReviewThreads)
	if !unresolvedReviewThreads {
		decision = EvaluateAutoPromote(issue, summary, cfg, now)
	}
	if autoPromoteDecisionNeedsWorkpadHydration(decision) {
		issue, decision = o.hydrateAutoPromoteWorkpadDecision(ctx, issue, summary, cfg, now)
	}
	targetState := autoPromoteTargetState(decision.Action, cfg)
	if targetState == "" {
		o.logAutoPromoteDecision(issue, decision, "")
		return false, connector.Issue{}
	}
	if normalizeState(issue.State) == normalizeState(targetState) {
		promoted := promotedIssue(issue, targetState, now)
		o.logAutoPromoteDecision(issue, decision, targetState)
		o.logCompletedActiveAutoPromoteSameState(issue, decision, cfg)
		return true, promoted
	}
	effectiveTargetState, applied := o.applyAutoPromoteDecisionWithTarget(ctx, state, issue, summary, decision, targetState, now)
	if !applied {
		return false, connector.Issue{}
	}
	return true, promotedIssue(issue, effectiveTargetState, now)
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
	sameState := normalizeState(issue.State) == normalizeState(targetState)
	if sameState {
		delete(state.Completed, issueID)
	} else {
		updated := mergeIssueTrackerFields(completed.Issue, issue)
		updated.State = targetState
		completed.Issue = updated
		state.Completed[issueID] = completed
	}
	delete(state.Claimed, issueID)
	if !sameState {
		delete(state.Retry, issueID)
		delete(state.BudgetRefusals, issueID)
	}
	delete(state.PriorAttempts, issueID)
}

func (o *Orchestrator) logCompletedActiveAutoPromoteSameState(
	issue connector.Issue,
	decision AutoPromoteDecision,
	cfg AutoPromoteConfig,
) {
	if o.logger == nil {
		return
	}
	gateCfg := gate.Effective(cfg.Gate)
	if gateCfg.Kind != gate.KindArtifact || decision.Action != AutoPromoteActionRework {
		return
	}
	statusField := strings.TrimSpace(gateCfg.Artifact.StatusField)
	o.logger.Warn(
		"completed artifact gate status unchanged after successful rework",
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
		"state", issue.State,
		"action", decision.Action,
		"reason", decision.Reason,
		"gate_status_field", statusField,
		"gate_status", artifactStatusFromIssue(issue, statusField),
	)
}

// completedActiveReviewTargetState reports the lane a completed item is
// promoted into, and the readiness fact that is missing when it reports none.
// The second result is empty for every other reason the target is empty, so a
// caller can tell "not ready" from "nothing to do here".
func completedActiveReviewTargetState(
	issue connector.Issue,
	finalState string,
	completionKind string,
	activeStates []string,
	terminalStates []string,
	cfg AutoPromoteConfig,
) (string, string) {
	cfg = normalizeAutoPromoteConfig(cfg)
	if !stateIn(issue.State, activeStates) || stateIn(issue.State, terminalStates) {
		return "", ""
	}
	reviewState := cfg.SourceState
	switch normalizeState(issue.State) {
	case normalizeState(reviewState), normalizeState(autoPromoteMergingState):
		return "", ""
	}
	operationalCompletionAccepted := completedOperationalCompletionAccepted(issue, completionKind)
	if ready, missing := completedActiveIssueReadyForReview(
		issue,
		gateRequiresPullRequest(cfg.Gate),
		operationalCompletionAccepted,
	); !ready {
		return "", missing
	}
	if !completedActiveFinalStateReviewEligible(finalState, reviewState) {
		return "", ""
	}
	// A native item reaches here with no pull request at all, because its
	// change is reviewed on the tracker's own change request instead, so the
	// thread check has to tolerate the absence rather than assume it away.
	if !operationalCompletionAccepted && gateRequiresPullRequest(cfg.Gate) &&
		issue.PullRequest != nil && len(issue.PullRequest.UnresolvedReviewThreads) > 0 {
		return reviewState, ""
	}
	if !completedActiveShouldEnterReview(issue, cfg, operationalCompletionAccepted) {
		return "", ""
	}
	return reviewState, ""
}

func completedActiveShouldEnterReview(issue connector.Issue, cfg AutoPromoteConfig, operationalCompletionAccepted bool) bool {
	if operationalCompletionAccepted {
		return true
	}
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

// Readiness facts a completed item can be missing. They are the log's
// vocabulary as well as the predicate's, so a skipped promotion names the
// thing that was not there.
const (
	// completedReviewMissingPullRequest is a gate that wants a pull request
	// on an issue that has none, on a tracker that reviews no change of its
	// own: the pull request is the only evidence such a tracker can offer.
	completedReviewMissingPullRequest = "pull_request"
	// completedReviewMissingOpenPullRequest is a pull request that was opened
	// for the item and is no longer open.
	completedReviewMissingOpenPullRequest = "open_pull_request"
	// completedReviewMissingChange is a tracker that reviews changes of its
	// own and holds none for the item as it stands.
	completedReviewMissingChange = "change_request"
)

// completedActiveIssueReadyForReview reports whether a completed item may be
// promoted out of its active lane, and the fact that is missing when it may
// not.
//
// The rule used to be a pull request and nothing else: every gate kind but
// artifact required issue.PullRequest to be open. That is right for a tracker
// whose changes are reviewed on pull requests and wrong for one whose are not.
// A hub-native item has no pull request and can have none when its project has
// no GitHub connector (decisions section 18.6, "projects without a GitHub
// connector show the change request alone"), so every completed native item
// failed the rule, the promotion was skipped, and the item stayed in its
// active lane looking like outstanding work -- operations.md section 8, the
// seventh dogfood run.
//
// The first fix asked whether the project has a connector, and promoted on the
// tracker's own change only when it has none. The eighth dogfood run found the
// half that leaves: a hosted project that does have a GitHub connector, whose
// conversation-driven attempt posts its diff and opens no pull request,
// because on the hosted flow opening one is a separate explicit action
// (section 18.6, POST {nativeBase}/work-items/:id/pull-requests/actions
// {action: open}) that the merge lane executes and the model has no credential
// for. The rule demanded a pull request that would never exist on its own and
// the item sat In Progress forever -- operations.md section 8, the eighth
// dogfood run.
//
// So the question is not whether the project has a connector, it is whether
// this item's own tracker holds an answer for it:
//
//   - a tracker that reviews changes of its own is asked for its change: a
//     change request or an attempt diff (section 18.5) recorded for the item at
//     the revision it stands at now, which is the same "as it stands" the claim
//     brake of section 9.2.1 uses. Whether a connector could mirror it decides
//     nothing, because nothing in the hosted flow opens the pull request.
//   - a pull request that was opened for the item is still judged exactly as
//     before: it must be open, so a merged or closed one is not ready.
//   - a tracker that reports no change review surface at all -- every non-hub
//     connector, and the github_compatible issueFromWorkItem path, where the
//     runner itself opens the pull request -- leaves the pull request as the
//     only authority, which is what every connector had before.
func completedActiveIssueReadyForReview(
	issue connector.Issue,
	requirePullRequest bool,
	operationalCompletionAccepted bool,
) (bool, string) {
	if operationalCompletionAccepted {
		return true, ""
	}
	if !requirePullRequest {
		return true, ""
	}
	if review := issue.ChangeReview; review != nil {
		return completedActiveChangeReadyForReview(issue, *review)
	}
	if issue.PullRequest == nil {
		return false, completedReviewMissingPullRequest
	}
	if normalizePullRequestState(issue.PullRequest.State) != "open" {
		return false, completedReviewMissingOpenPullRequest
	}
	return true, ""
}

// completedActiveChangeReadyForReview is the rule for an item whose tracker
// reviews changes of its own: the change has to cover the item as it stands,
// and a pull request that exists has to be open. An item with no pull request
// is ready on its change alone, whether or not its project has a connector,
// because opening one is an explicit action nothing in the attempt takes.
func completedActiveChangeReadyForReview(issue connector.Issue, review connector.ChangeReview) (bool, string) {
	if !review.ChangeAtCurrentRevision {
		return false, completedReviewMissingChange
	}
	if issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) != "open" {
		return false, completedReviewMissingOpenPullRequest
	}
	return true, ""
}

func completedOperationalCompletionAccepted(issue connector.Issue, completionKind string) bool {
	if strings.TrimSpace(completionKind) != workpad.CompletionOperational {
		return false
	}
	_, ok := operationalCompletionFromIssue(issue)
	return ok
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
		summary.OperationalCompletionAccepted = strings.TrimSpace(completed.CompletionKind) == workpad.CompletionOperational
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
		effectiveTargetState, applied := o.applyAutoPromoteDecisionWithTarget(ctx, state, issue, summary, decision, targetState, now)
		if !applied {
			return false, connector.Issue{}
		}
		promoted := promotedIssue(issue, effectiveTargetState, now)
		o.finishCompletedActiveReviewTransition(ctx, state, issue, completed, effectiveTargetState)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "completed_active_gate_wait_timeout",
			Message: "moved " + issueLabel(issue) + " from " + strings.TrimSpace(issue.State) + " to " + effectiveTargetState + " after auto-promote gate wait timeout",
		})
		return true, promoted
	}
	targetState := cfg.SourceState
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, targetState, now, "auto_promote_gate_wait_timeout", laneMutationAcceptCompletion); err != nil {
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
