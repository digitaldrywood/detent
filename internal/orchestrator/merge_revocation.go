package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	mergeRevocationStateChanged          = "merge_state_changed"
	mergeRevocationApprovalLabelRemoved  = "merge_approval_label_removed"
	mergeRevocationCITriggerLabelRemoved = "merge_ci_trigger_label_removed"
	mergeRevocationMissingPullRequest    = "missing_pull_request"
	mergeRevocationDraftPullRequest      = "draft_pull_request"
	mergeRevocationPullRequestNotOpen    = "pull_request_not_open"
	maxIdenticalMergeRevocations         = 3
)

const (
	mergeRevocationCommentLimit  = 10
	mergeRevocationCommentWindow = 24 * time.Hour
	mergeRevocationCommentPrefix = "<!-- detent:merge-revocation "
)

type mergeRevocation struct {
	issue       connector.Issue
	reason      string
	targetState string
}

type mergeRevocationCommentState struct {
	localWrites                 []mergeRevocationCommentWrite
	lastSignature               string
	warnedUntil                 time.Time
	budgetEscalated             bool
	resourceExhausted           bool
	resourceExhaustionEscalated bool
}

type mergeRevocationCommentWrite struct {
	signature string
	at        time.Time
}

type mergeRevocationStreak struct {
	reason string
	count  int
}

func (o *Orchestrator) revokeRunningMergeIfIneligible(
	ctx context.Context,
	state *State,
	running Running,
	now time.Time,
) (Running, bool) {
	if _, pending := o.pendingMergeRevocations[running.Issue.ID]; pending {
		return running, true
	}
	if decision, revoked := mergeRevocationForIssue(running.Issue, o.cfg, false); revoked {
		o.beginMergeRevocation(state, running, decision, now)
		return running, true
	}

	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		return running, false
	}
	refreshed, err := hydrator.HydratePullRequest(ctx, running.Issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge eligibility pull request refresh failed",
				"issue_id", running.Issue.ID,
				"identifier", running.Issue.Identifier,
				"error", err,
			)
		}
		return running, false
	}
	running.Issue = mergeIssueTrackerFields(running.Issue, refreshed)
	if decision, revoked := mergeRevocationForIssue(running.Issue, o.cfg, true); revoked {
		o.beginMergeRevocation(state, running, decision, now)
		return running, true
	}
	return running, false
}

func mergeRevocationForIssue(issue connector.Issue, cfg Config, checkPullRequest bool) (mergeRevocation, bool) {
	if normalizeState(issue.State) != normalizeState(autoPromoteMergingState) {
		return mergeRevocation{
			issue:  cloneIssue(issue),
			reason: mergeRevocationStateChanged,
		}, true
	}
	if _, revoked := mergeApprovalLabelRevoked(issue, cfg); revoked {
		return mergeRevocation{
			issue:       cloneIssue(issue),
			reason:      mergeRevocationApprovalLabelRemoved,
			targetState: normalizeAutoPromoteConfig(cfg.AutoPromote).SourceState,
		}, true
	}
	if !checkPullRequest {
		return mergeRevocation{}, false
	}

	pullRequest := issue.PullRequest
	if pullRequest == nil {
		return mergeRevocation{
			issue:       cloneIssue(issue),
			reason:      mergeRevocationMissingPullRequest,
			targetState: normalizeAutoPromoteConfig(cfg.AutoPromote).SourceState,
		}, true
	}
	if pullRequestHydrationBlocksProgress(pullRequest) {
		return mergeRevocation{}, false
	}
	if _, revoked := mergeCITriggerLabelRevoked(issue, cfg); revoked {
		return mergeRevocation{
			issue:       cloneIssue(issue),
			reason:      mergeRevocationCITriggerLabelRemoved,
			targetState: normalizeAutoPromoteConfig(cfg.AutoPromote).SourceState,
		}, true
	}
	switch normalizePullRequestState(pullRequest.State) {
	case "open":
		if !pullRequest.Draft {
			return mergeRevocation{}, false
		}
		return mergeRevocation{
			issue:       cloneIssue(issue),
			reason:      mergeRevocationDraftPullRequest,
			targetState: normalizeAutoPromoteConfig(cfg.AutoPromote).SourceState,
		}, true
	case "closed":
		return mergeRevocation{
			issue:       cloneIssue(issue),
			reason:      mergeRevocationPullRequestNotOpen,
			targetState: autoPromoteReworkState,
		}, true
	default:
		return mergeRevocation{}, false
	}
}

func mergeRevocationRequiresImmediateStop(revocation mergeRevocation, result runpkg.RunResult) bool {
	if revocation.reason != mergeRevocationCITriggerLabelRemoved {
		return true
	}
	return !result.PullRequestHeadPushed || result.CITriggerLabelReapplied
}

func mergeApprovalLabelRevoked(issue connector.Issue, cfg Config) (string, bool) {
	gateCfg := gate.Effective(cfg.AutoPromote.Gate)
	if gateCfg.Kind != gate.KindHumanReview {
		return "", false
	}
	approvalLabel := normalizeLabel(gateCfg.ApprovalLabel)
	for _, label := range issue.Labels {
		if normalizeLabel(label) == approvalLabel {
			return approvalLabel, false
		}
	}
	return approvalLabel, true
}

func mergeCITriggerLabelRevoked(issue connector.Issue, cfg Config) (string, bool) {
	triggerLabel := normalizeLabel(gate.Effective(cfg.AutoPromote.Gate).CITriggerLabel)
	if triggerLabel == "" || issue.PullRequest == nil || issue.PullRequest.Labels == nil {
		return triggerLabel, false
	}
	for _, label := range issue.PullRequest.Labels {
		if normalizeLabel(label) == triggerLabel {
			return triggerLabel, false
		}
	}
	return triggerLabel, true
}

func (o *Orchestrator) beginMergeRevocation(state *State, running Running, revocation mergeRevocation, now time.Time) {
	issueID := strings.TrimSpace(running.Issue.ID)
	if issueID == "" {
		return
	}
	if o.pendingMergeRevocations == nil {
		o.pendingMergeRevocations = map[string]mergeRevocation{}
	}
	running.Issue = cloneIssue(revocation.issue)
	o.pendingMergeRevocations[issueID] = revocation
	state.Running[issueID] = running
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	if running.stop != nil {
		running.stop(runpkg.ErrMergeRevoked)
	} else if running.cancel != nil {
		running.cancel()
	}
	if o.logger != nil {
		o.logger.Info(
			"merge_worker_revocation_requested",
			mergeWorkerLogAttrs(revocation.issue,
				"reason", revocation.reason,
				"target_state", revocation.targetState,
			)...,
		)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "merge_worker_revocation_requested",
		Message: "stopping merge worker for " + issueLabel(revocation.issue) + ": " + revocation.reason,
	})
}

func (o *Orchestrator) handleMergeRevocationCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	revocation, ok := o.pendingMergeRevocations[event.IssueID]
	if !ok {
		return false
	}
	delete(o.pendingMergeRevocations, event.IssueID)
	o.finishMergeRevocation(ctx, state, event, running, revocation)
	return true
}

func (o *Orchestrator) finishMergeRevocation(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	revocation mergeRevocation,
) {
	completedAt := event.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = o.clockNow().UTC()
	}
	running.Issue = cloneIssue(revocation.issue)
	if !event.Result.RuntimeIdentity.IsZero() {
		running.RuntimeIdentity = running.RuntimeIdentity.Merge(event.Result.RuntimeIdentity)
	}
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
		state.DiffStats[event.IssueID] = event.Result.DiffStats
	}
	tokens := event.Result.Tokens
	if tokens == (TokenTotals{}) {
		tokens = running.Tokens
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, tokens)
	delete(state.Running, event.IssueID)
	delete(state.Claimed, event.IssueID)
	delete(state.Retry, event.IssueID)
	delete(state.BudgetRefusals, event.IssueID)
	delete(state.PriorAttempts, event.IssueID)
	delete(state.InstantFailures, event.IssueID)
	delete(state.RepeatedFailures, event.IssueID)
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	releaseProjectFailureBreakerCanary(state, event.IssueID)
	if event.Result.TurnStarted {
		o.recoverBackendCapacity(state, running, completedAt)
	}
	if err := o.abandonClaim(ctx, event.IssueID); err != nil && o.logger != nil {
		o.logger.Warn("merge revocation claim release failed", "issue_id", event.IssueID, "error", err)
	}
	revocationErr := mergeRevocationError(revocation)
	o.recordProjectAttemptOutcome(
		state,
		event.IssueID,
		completedAt,
		store.WorkAttemptTerminalMergeRevoked,
		revocationErr,
		string(store.WorkAttemptTerminalMergeRevoked),
		revocation.reason,
	)
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		completedAt,
		store.WorkAttemptTerminalMergeRevoked,
		string(store.WorkAttemptTerminalMergeRevoked),
		revocation.reason,
		"merge_revoked",
		"merge eligibility revoked; worker stopped",
	)
	o.recordMergeFailed(state, running.Issue, completedAt, revocation.reason, nil)
	o.routeMergeRevocation(ctx, state, &revocation, completedAt)
	o.commentMergeRevocation(ctx, state, revocation, completedAt)
	o.logWorkerLifecycle(running.Issue, "worker_merge_revoked",
		"attempt", running.Attempt,
		"worker_host", strings.TrimSpace(running.WorkerHost),
		"reason", revocation.reason,
	)
	if o.logger != nil {
		o.logger.Info(
			"merge_worker_revoked",
			mergeWorkerLogAttrs(revocation.issue,
				"reason", revocation.reason,
				"target_state", revocation.targetState,
			)...,
		)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   "merge_worker_revoked",
		Message: "stopped merge worker for " + issueLabel(revocation.issue) + ": " + revocation.reason,
	})
}

func (o *Orchestrator) parkRepeatedMergeRevocations(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	now time.Time,
) (bool, bool) {
	streak, err := o.consecutiveMergeRevocations(ctx, state, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge revocation backoff check failed",
				"issue_id", strings.TrimSpace(issue.ID),
				"identifier", issue.Identifier,
				"error", err,
			)
		}
		return false, false
	}
	if streak.count < maxIdenticalMergeRevocations {
		return false, false
	}

	decision := autoPromoteDecision(AutoPromoteActionSkip, AutoPromoteReasonMergeRevocationLimit)
	if err := o.updateIssueStateByIDStrictWithMetadata(
		ctx,
		state,
		issue.ID,
		issue,
		blockedStatusState,
		now,
		string(AutoPromoteReasonMergeRevocationLimit),
		workflowLaneMetadata{},
	); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge revocation backoff park failed",
				"issue_id", strings.TrimSpace(issue.ID),
				"identifier", issue.Identifier,
				"revocation_reason", streak.reason,
				"consecutive_revocations", streak.count,
				"limit", maxIdenticalMergeRevocations,
				"error", err,
			)
		}
		o.logAutoPromoteDecision(issue, decision, "")
		return true, false
	}

	if err := o.connector.CreateComment(ctx, issue.ID, mergeRevocationLimitComment(issue, streak)); err != nil && o.logger != nil {
		o.logger.Warn(
			"merge revocation backoff comment failed",
			"issue_id", strings.TrimSpace(issue.ID),
			"identifier", issue.Identifier,
			"error", err,
		)
	}
	o.logAutoPromoteDecision(issue, decision, blockedStatusState)
	if o.logger != nil {
		o.logger.Warn(
			"merge_worker_revocation_limit",
			mergeWorkerLogAttrs(issue,
				"revocation_reason", streak.reason,
				"consecutive_revocations", streak.count,
				"limit", maxIdenticalMergeRevocations,
				"target_state", blockedStatusState,
			)...,
		)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "merge_worker_revocation_limit",
		Message: fmt.Sprintf("parked %s after %d consecutive %s merge revocations", issueLabel(issue), streak.count, streak.reason),
	})
	return true, true
}

func (o *Orchestrator) consecutiveMergeRevocations(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (mergeRevocationStreak, error) {
	if o != nil && o.workAttempts != nil {
		attempts, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
			ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
			IssueID:    strings.TrimSpace(issue.ID),
			Identifier: strings.TrimSpace(issue.Identifier),
			IssueURL:   strings.TrimSpace(issue.URL),
			WorkerType: "merge",
			Limit:      maxIdenticalMergeRevocations + 1,
		})
		if err != nil {
			return mergeRevocationStreak{}, err
		}
		return mergeRevocationStreakFromAttempts(attempts, pullRequestNumber(issue)), nil
	}
	return mergeRevocationStreakFromSnapshots(state, issue), nil
}

func mergeRevocationStreakFromAttempts(attempts []store.WorkAttempt, currentPRNumber int) mergeRevocationStreak {
	streak := mergeRevocationStreak{}
	for _, attempt := range attempts {
		if attempt.TerminalState != store.WorkAttemptTerminalMergeRevoked {
			break
		}
		reason := strings.TrimSpace(attempt.ErrorMessage)
		if reason == "" || streak.reason != "" && reason != streak.reason {
			break
		}
		if currentPRNumber > 0 && attempt.PRNumber != nil && *attempt.PRNumber > 0 && int(*attempt.PRNumber) != currentPRNumber {
			break
		}
		if streak.reason == "" {
			streak.reason = reason
		}
		streak.count++
	}
	return streak
}

func mergeRevocationStreakFromSnapshots(state *State, issue connector.Issue) mergeRevocationStreak {
	if state == nil {
		return mergeRevocationStreak{}
	}
	streak := mergeRevocationStreak{}
	currentPRNumber := pullRequestNumber(issue)
	for _, attempt := range state.WorkAttempts {
		if strings.TrimSpace(attempt.IssueID) != strings.TrimSpace(issue.ID) {
			continue
		}
		if attempt.TerminalState != string(store.WorkAttemptTerminalMergeRevoked) {
			break
		}
		reason := strings.TrimSpace(attempt.ErrorMessage)
		if reason == "" || streak.reason != "" && reason != streak.reason {
			break
		}
		if currentPRNumber > 0 && attempt.PRNumber != nil && *attempt.PRNumber > 0 && int(*attempt.PRNumber) != currentPRNumber {
			break
		}
		if streak.reason == "" {
			streak.reason = reason
		}
		streak.count++
	}
	return streak
}

func mergeRevocationLimitComment(issue connector.Issue, streak mergeRevocationStreak) string {
	var body strings.Builder
	body.WriteString("Detent parked this issue in Blocked after repeated identical merge eligibility revocations.")
	body.WriteString("\n\n- reason: ")
	body.WriteString(string(AutoPromoteReasonMergeRevocationLimit))
	body.WriteString("\n- revocation_reason: ")
	body.WriteString(streak.reason)
	body.WriteString("\n- consecutive_revocations: ")
	body.WriteString(strconv.Itoa(streak.count))
	body.WriteString("\n- limit: ")
	body.WriteString(strconv.Itoa(maxIdenticalMergeRevocations))
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			body.WriteString("\n- pull_request: ")
			body.WriteString(url)
		}
	}
	body.WriteString("\n- human_action: correct the repeated merge eligibility failure, then move the issue to Rework")
	return body.String()
}

func (o *Orchestrator) routeMergeRevocation(ctx context.Context, state *State, revocation *mergeRevocation, at time.Time) {
	if strings.TrimSpace(revocation.targetState) == "" {
		return
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{revocation.issue.ID})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("merge revocation state refresh failed", "issue_id", revocation.issue.ID, "error", err)
		}
		return
	}
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) != strings.TrimSpace(revocation.issue.ID) {
			continue
		}
		revocation.issue = mergeIssueTrackerFields(revocation.issue, issue)
		if normalizeState(revocation.issue.State) != normalizeState(autoPromoteMergingState) {
			revocation.targetState = ""
			return
		}
		break
	}
	if err := o.updateIssueStateByID(
		ctx,
		state,
		revocation.issue.ID,
		revocation.issue,
		revocation.targetState,
		at,
		string(store.WorkAttemptTerminalMergeRevoked)+":"+revocation.reason,
	); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge revocation route failed",
				"issue_id", revocation.issue.ID,
				"target_state", revocation.targetState,
				"reason", revocation.reason,
				"error", err,
			)
		}
		return
	}
	revocation.issue.State = revocation.targetState
}

func (o *Orchestrator) commentMergeRevocation(
	ctx context.Context,
	state *State,
	revocation mergeRevocation,
	at time.Time,
) {
	if o.connector == nil {
		return
	}
	issueID := strings.TrimSpace(revocation.issue.ID)
	if issueID == "" {
		return
	}
	at = at.UTC()
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	commentState := o.mergeRevocationCommentState(issueID)
	if commentState.resourceExhausted {
		if !commentState.resourceExhaustionEscalated {
			commentState.resourceExhaustionEscalated = o.escalateMergeRevocationCommentLoss(
				ctx,
				state,
				revocation.issue,
				at,
				"merge_revocation_comment_resource_exhausted",
			)
		}
		return
	}
	signature := mergeRevocationCommentSignature(revocation)
	comments := o.mergeRevocationIssueComments(ctx, revocation.issue)
	if commentState.lastSignature == signature || latestMergeRevocationCommentSignature(comments) == signature {
		return
	}
	if mergeRevocationCommentCount(comments, commentState.localWrites, at) >= mergeRevocationCommentLimit {
		if !at.Before(commentState.warnedUntil) {
			commentState.warnedUntil = at.Add(mergeRevocationCommentWindow)
			if o.logger != nil {
				o.logger.Warn(
					"merge revocation comment budget exhausted; issue needs human attention",
					"issue_id", issueID,
					"limit", mergeRevocationCommentLimit,
					"window", mergeRevocationCommentWindow,
				)
			}
		}
		if !commentState.budgetEscalated {
			commentState.budgetEscalated = o.escalateMergeRevocationCommentLoss(
				ctx,
				state,
				revocation.issue,
				at,
				"merge_revocation_comment_budget_exhausted",
			)
		}
		return
	}
	var body strings.Builder
	body.WriteString("Detent stopped the active merge because merge eligibility was revoked.")
	body.WriteString("\n\n- reason: ")
	body.WriteString(revocation.reason)
	body.WriteString("\n- head_sha: ")
	body.WriteString(mergeRevocationHeadSHA(revocation))
	if stateName := strings.TrimSpace(revocation.issue.State); stateName != "" {
		body.WriteString("\n- observed_state: ")
		body.WriteString(stateName)
	}
	if targetState := strings.TrimSpace(revocation.targetState); targetState != "" {
		body.WriteString("\n- routed_to: ")
		body.WriteString(targetState)
	}
	if revocation.issue.PullRequest != nil {
		if url := strings.TrimSpace(revocation.issue.PullRequest.URL); url != "" {
			body.WriteString("\n- pull_request: ")
			body.WriteString(url)
		}
	}
	body.WriteString("\n\n")
	body.WriteString(signature)
	if err := o.connector.CreateComment(ctx, issueID, body.String()); err != nil {
		if errors.Is(err, connector.ErrResourceExhausted) {
			commentState.resourceExhausted = true
			if o.logger != nil {
				o.logger.Warn(
					"merge revocation comment resource exhausted; issue needs human attention",
					"issue_id", issueID,
					"error", err,
				)
			}
			commentState.resourceExhaustionEscalated = o.escalateMergeRevocationCommentLoss(
				ctx,
				state,
				revocation.issue,
				at,
				"merge_revocation_comment_resource_exhausted",
			)
			return
		}
		if o.logger != nil {
			o.logger.Warn("merge revocation comment failed", "issue_id", issueID, "error", err)
		}
		return
	}
	commentState.lastSignature = signature
	commentState.localWrites = append(commentState.localWrites, mergeRevocationCommentWrite{
		signature: signature,
		at:        at,
	})
}

func (o *Orchestrator) mergeRevocationCommentState(issueID string) *mergeRevocationCommentState {
	if o.mergeRevocationComments == nil {
		o.mergeRevocationComments = map[string]*mergeRevocationCommentState{}
	}
	state := o.mergeRevocationComments[issueID]
	if state == nil {
		state = &mergeRevocationCommentState{}
		o.mergeRevocationComments[issueID] = state
	}
	return state
}

func (o *Orchestrator) mergeRevocationIssueComments(
	ctx context.Context,
	issue connector.Issue,
) []connector.IssueComment {
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		return issue.Comments
	}
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge revocation comment history failed",
				"issue_id", issue.ID,
				"error", err,
			)
		}
		return issue.Comments
	}
	return comments
}

func mergeRevocationCommentSignature(revocation mergeRevocation) string {
	return fmt.Sprintf(
		"%sreason=%s head_sha=%s -->",
		mergeRevocationCommentPrefix,
		strings.TrimSpace(revocation.reason),
		mergeRevocationHeadSHA(revocation),
	)
}

func mergeRevocationHeadSHA(revocation mergeRevocation) string {
	if revocation.issue.PullRequest == nil {
		return ""
	}
	return strings.TrimSpace(revocation.issue.PullRequest.HeadSHA)
}

func latestMergeRevocationCommentSignature(comments []connector.IssueComment) string {
	for index := len(comments) - 1; index >= 0; index-- {
		if signature := mergeRevocationCommentMarker(comments[index].Body); signature != "" {
			return signature
		}
	}
	return ""
}

func mergeRevocationCommentCount(
	comments []connector.IssueComment,
	localWrites []mergeRevocationCommentWrite,
	at time.Time,
) int {
	cutoff := at.Add(-mergeRevocationCommentWindow)
	persisted := map[string]struct{}{}
	count := 0
	for _, comment := range comments {
		signature := mergeRevocationCommentMarker(comment.Body)
		if signature == "" {
			continue
		}
		persisted[signature] = struct{}{}
		if comment.CreatedAt == nil || !comment.CreatedAt.Before(cutoff) {
			count++
		}
	}
	for _, write := range localWrites {
		if write.at.Before(cutoff) {
			continue
		}
		if _, ok := persisted[write.signature]; ok {
			continue
		}
		count++
	}
	return count
}

func mergeRevocationCommentMarker(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, mergeRevocationCommentPrefix) && strings.HasSuffix(line, " -->") {
			return line
		}
	}
	return ""
}

func (o *Orchestrator) escalateMergeRevocationCommentLoss(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	at time.Time,
	reason string,
) bool {
	targetState := normalizeAutoPromoteConfig(o.cfg.AutoPromote).SourceState
	if err := o.updateIssueStateByID(
		ctx,
		state,
		issue.ID,
		issue,
		targetState,
		at,
		reason,
	); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge revocation comment loss escalation failed",
				"issue_id", issue.ID,
				"target_state", targetState,
				"reason", reason,
				"error", err,
			)
		}
		return false
	}
	return true
}

func mergeRevocationError(revocation mergeRevocation) error {
	return fmt.Errorf("%w: %s", runpkg.ErrMergeRevoked, revocation.reason)
}
