package orchestrator

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	implementProgressMetadataKey       = "completion_progress"
	implementProgressOutcomeNoProgress = "no_progress"
	noProgressLimitReason              = "no_progress_limit"
	workpadBlockedUnactionedReason     = "workpad_blocked_unactioned"
	workpadBlockedUnactionedLimit      = 2
)

type implementCompletionProgressDecision struct {
	Issue                  connector.Issue
	Outcome                store.WorkAttemptTerminalState
	Reason                 string
	CurrentSignature       autoPromoteReworkSignature
	PreviousSignature      autoPromoteReworkSignature
	PreviousSignatureFound bool
	FailedChecksAdded      []string
	FailedChecksRemoved    []string
	WorkspaceDiffStats     DiffStats
	ConsecutiveNoProgress  int
	WorkpadStatus          string
	HumanAction            string
	TrackerState           string
	ConsecutiveHumanAction int
	NoProgressLimit        int
	BlockReason            string
	Block                  bool
	Warning                string
}

type implementProgressRecord struct {
	Outcome                string                            `json:"outcome"`
	Reason                 string                            `json:"reason"`
	CurrentSignature       implementProgressSignatureRecord  `json:"current_signature"`
	PreviousSignature      *implementProgressSignatureRecord `json:"previous_signature,omitempty"`
	PreviousHeadSHA        string                            `json:"previous_head_sha,omitempty"`
	CurrentHeadSHA         string                            `json:"current_head_sha,omitempty"`
	FailedChecksAdded      []string                          `json:"failed_checks_added,omitempty"`
	FailedChecksRemoved    []string                          `json:"failed_checks_removed,omitempty"`
	WorkspaceDiffStats     implementProgressDiffStats        `json:"workspace_diffstat"`
	ConsecutiveNoProgress  int                               `json:"consecutive_no_progress,omitempty"`
	WorkpadStatus          string                            `json:"workpad_status,omitempty"`
	HumanAction            string                            `json:"human_action,omitempty"`
	TrackerState           string                            `json:"tracker_state,omitempty"`
	ConsecutiveHumanAction int                               `json:"consecutive_human_action,omitempty"`
	NoProgressLimit        int                               `json:"no_progress_limit,omitempty"`
	BlockReason            string                            `json:"block_reason,omitempty"`
	Warning                string                            `json:"warning,omitempty"`
}

type implementProgressSignatureRecord struct {
	PRNumber     int64    `json:"pr_number,omitempty"`
	HeadSHA      string   `json:"head_sha,omitempty"`
	FailedChecks []string `json:"failed_checks,omitempty"`
}

type implementProgressDiffStats struct {
	FilesChanged int    `json:"files_changed"`
	AddedLines   int    `json:"added_lines"`
	RemovedLines int    `json:"removed_lines"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	Status       string `json:"status,omitempty"`
}

func (o *Orchestrator) evaluateImplementCompletionProgress(
	ctx context.Context,
	running Running,
	finalState string,
	pullRequestUpdated bool,
) implementCompletionProgressDecision {
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	decision := implementCompletionProgressDecision{
		Issue:              cloneIssue(running.Issue),
		Outcome:            store.WorkAttemptTerminalSuccess,
		Reason:             "success_without_progress_check",
		WorkspaceDiffStats: running.DiffStats,
		NoProgressLimit:    cfg.NoProgressLimit,
	}
	if strings.TrimSpace(finalState) != FinalStateCompleted {
		decision.Reason = "final_state_not_completed"
		return decision
	}
	if !implementProgressLinkedPullRequest(running.Issue) {
		if pullRequestUpdated {
			decision.Reason = "pull_request_created_or_updated"
			return decision
		}
		issue, workpadCurrent := o.refreshImplementCompletionIssue(ctx, running.Issue)
		decision.Issue = issue
		decision.TrackerState = strings.TrimSpace(issue.State)
		if workpadCurrent {
			decision.WorkpadStatus, decision.HumanAction = implementProgressBlockedHumanAction(issue)
		}
		attempts, err := o.recentImplementCompletionAttempts(ctx, issue, running)
		if err != nil {
			decision.Reason = "attempt_history_lookup_failed"
			decision.Warning = err.Error()
			if o.logger != nil {
				o.logger.Warn(
					"implement worker progress history lookup failed",
					"issue_id", running.Issue.ID,
					"identifier", running.Issue.Identifier,
					"error", err,
				)
			}
			return decision
		}
		if decision.HumanAction != "" {
			decision.ConsecutiveHumanAction = 1 + consecutiveImplementBlockedHumanActionAttempts(
				attempts,
				decision.HumanAction,
				decision.TrackerState,
			)
			if decision.ConsecutiveHumanAction >= workpadBlockedUnactionedLimit {
				decision.Outcome = store.WorkAttemptTerminalNoProgress
				decision.Reason = workpadBlockedUnactionedReason
				decision.BlockReason = workpadBlockedUnactionedReason
				decision.Block = true
				return decision
			}
		}
		if !diffStatsPresent(running.DiffStats) {
			decision.Reason = "workspace_diffstat_unavailable_without_pull_request"
			return decision
		}
		if !implementProgressDiffStatsClean(running.DiffStats) {
			if strings.TrimSpace(running.DiffStats.Fingerprint) == "" {
				decision.Reason = "workspace_diff_fingerprint_unavailable_without_pull_request"
				return decision
			}
			matchingAttempts := consecutiveImplementSameNoPRDiffAttempts(
				attempts,
				running.DiffStats,
				decision.WorkpadStatus,
				decision.HumanAction,
			)
			if matchingAttempts == 0 {
				decision.Reason = "workspace_diff_present_without_pull_request"
				return decision
			}
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = "unchanged_workspace_diff_without_pull_request"
			decision.ConsecutiveNoProgress = 1 + matchingAttempts
			decision.Block = decision.NoProgressLimit > 0 && decision.ConsecutiveNoProgress >= decision.NoProgressLimit
			if decision.Block {
				decision.BlockReason = noProgressLimitReason
			}
			return decision
		}
		decision.Outcome = store.WorkAttemptTerminalNoProgress
		decision.Reason = "completed_clean_diff_without_pull_request"
		decision.ConsecutiveNoProgress = 1 + consecutiveImplementNoProgressAttempts(attempts, autoPromoteReworkSignature{})
		decision.Block = decision.NoProgressLimit > 0 && decision.ConsecutiveNoProgress >= decision.NoProgressLimit
		if decision.Block {
			decision.BlockReason = noProgressLimitReason
		}
		return decision
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		decision.Reason = "pull_request_hydrator_unavailable"
		decision.Warning = "pull request hydrator unavailable"
		o.warnImplementProgressHydration(running.Issue, decision.Warning, nil)
		return decision
	}
	issue, err := hydrator.HydratePullRequest(ctx, running.Issue)
	if err != nil {
		decision.Reason = "pull_request_hydration_failed"
		decision.Warning = err.Error()
		o.warnImplementProgressHydration(running.Issue, "pull request hydration failed", err)
		return decision
	}
	decision.Issue = issue
	if reason := implementProgressHydrationUnavailableReason(issue.PullRequest); reason != "" {
		decision.Reason = "pull_request_hydration_unavailable"
		decision.Warning = reason
		o.warnImplementProgressHydration(issue, reason, nil)
		return decision
	}
	signature := autoPromoteReworkSignatureFromIssue(issue, AutoPromoteSummaryFromIssue(issue))
	decision.CurrentSignature = signature
	if !implementProgressSignatureUsable(signature) {
		decision.Reason = "pull_request_signature_incomplete"
		return decision
	}

	attempts, err := o.recentImplementCompletionAttempts(ctx, issue, running)
	if err != nil {
		decision.Reason = "attempt_history_lookup_failed"
		decision.Warning = err.Error()
		if o.logger != nil {
			o.logger.Warn(
				"implement worker progress history lookup failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"error", err,
			)
		}
		return decision
	}
	previous, ok := latestImplementProgressSignature(attempts)
	if !ok {
		decision.Reason = "first_completed_attempt"
		return decision
	}
	decision.PreviousSignature = previous
	decision.PreviousSignatureFound = true
	decision.FailedChecksAdded, decision.FailedChecksRemoved = implementProgressFailedCheckDelta(previous.FailedChecks, signature.FailedChecks)
	if !implementProgressSignatureEqual(previous, signature) {
		decision.Reason = "signature_changed"
		return decision
	}
	if !implementProgressDiffStatsClean(running.DiffStats) {
		if !diffStatsPresent(running.DiffStats) {
			decision.Reason = "workspace_diffstat_unavailable"
			return decision
		}
		decision.Reason = "workspace_diff_present"
		return decision
	}

	decision.Outcome = store.WorkAttemptTerminalNoProgress
	decision.Reason = "unchanged_signature_clean_diff"
	decision.ConsecutiveNoProgress = 1 + consecutiveImplementNoProgressAttempts(attempts, signature)
	decision.Block = decision.NoProgressLimit > 0 && decision.ConsecutiveNoProgress >= decision.NoProgressLimit
	if decision.Block {
		decision.BlockReason = noProgressLimitReason
	}
	return decision
}

func (o *Orchestrator) recentImplementCompletionAttempts(
	ctx context.Context,
	issue connector.Issue,
	running Running,
) ([]store.WorkAttempt, error) {
	if o == nil || o.workAttempts == nil {
		return nil, nil
	}
	limit := normalizeAutoPromoteConfig(o.cfg.AutoPromote).NoProgressLimit + 1
	if limit < 20 {
		limit = 20
	}
	return o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		WorkerType: workAttemptWorkerType(issue, running.Mode),
		Limit:      limit,
	})
}

func (o *Orchestrator) refreshImplementCompletionIssue(ctx context.Context, issue connector.Issue) (connector.Issue, bool) {
	if o == nil || o.connector == nil || strings.TrimSpace(issue.ID) == "" {
		return cloneIssue(issue), false
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{issue.ID})
	if err != nil {
		o.warnImplementProgressRefresh(issue, "fetch tracker state failed", err)
		return cloneIssue(issue), false
	}
	refreshed := connector.Issue{}
	for _, candidate := range issues {
		if strings.TrimSpace(candidate.ID) == strings.TrimSpace(issue.ID) {
			refreshed = mergeIssueTrackerFields(issue, candidate)
			break
		}
	}
	if strings.TrimSpace(refreshed.ID) == "" {
		o.warnImplementProgressRefresh(issue, "tracker issue was not returned", nil)
		return cloneIssue(issue), false
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		o.warnImplementProgressRefresh(refreshed, "issue comment reader unavailable", nil)
		return refreshed, false
	}
	comments, err := reader.FetchIssueComments(ctx, refreshed)
	if err != nil {
		o.warnImplementProgressRefresh(refreshed, "fetch workpad comments failed", err)
		return refreshed, false
	}
	refreshed.Comments = comments
	refreshed.WorkpadSignal = nil
	if signal, ok := autoPromoteIssueWorkpadSignal(refreshed); ok {
		refreshed.WorkpadSignal = signal
	}
	return refreshed, true
}

func implementProgressBlockedHumanAction(issue connector.Issue) (string, string) {
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured {
		return "", ""
	}
	if strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return "", ""
	}
	humanAction := strings.TrimSpace(signal.HumanAction)
	if humanAction == "" {
		return "", ""
	}
	return workpad.StatusBlocked, humanAction
}

func latestImplementProgressSignature(attempts []store.WorkAttempt) (autoPromoteReworkSignature, bool) {
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAttempt(attempt)
		if !ok {
			continue
		}
		signature := record.CurrentSignature.signature()
		if implementProgressSignatureUsable(signature) {
			return signature, true
		}
	}
	return autoPromoteReworkSignature{}, false
}

func consecutiveImplementNoProgressAttempts(attempts []store.WorkAttempt, current autoPromoteReworkSignature) int {
	count := 0
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAttempt(attempt)
		if !ok || !implementProgressRecordMatchesNoProgress(record, current) {
			return count
		}
		count++
	}
	return count
}

func consecutiveImplementSameNoPRDiffAttempts(
	attempts []store.WorkAttempt,
	current DiffStats,
	workpadStatus string,
	humanAction string,
) int {
	count := 0
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAttempt(attempt)
		if !ok || implementProgressSignatureUsable(record.CurrentSignature.signature()) ||
			!implementProgressDiffFingerprintEqual(record.WorkspaceDiffStats.Fingerprint, current.Fingerprint) ||
			!implementProgressWorkpadEqual(record, workpadStatus, humanAction) {
			return count
		}
		count++
	}
	return count
}

func implementProgressWorkpadEqual(record implementProgressRecord, workpadStatus string, humanAction string) bool {
	return strings.TrimSpace(record.WorkpadStatus) == strings.TrimSpace(workpadStatus) &&
		strings.TrimSpace(record.HumanAction) == strings.TrimSpace(humanAction)
}

func consecutiveImplementBlockedHumanActionAttempts(attempts []store.WorkAttempt, humanAction string, trackerState string) int {
	humanAction = strings.TrimSpace(humanAction)
	trackerState = normalizeState(trackerState)
	if humanAction == "" || trackerState == "" {
		return 0
	}
	count := 0
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAttempt(attempt)
		if !ok || strings.TrimSpace(record.WorkpadStatus) != workpad.StatusBlocked ||
			strings.TrimSpace(record.HumanAction) != humanAction ||
			normalizeState(record.TrackerState) != trackerState {
			return count
		}
		count++
	}
	return count
}

func implementProgressDiffFingerprintEqual(left string, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && left == right
}

func implementProgressRecordMatchesNoProgress(record implementProgressRecord, current autoPromoteReworkSignature) bool {
	if !implementProgressSignatureEqual(record.CurrentSignature.signature(), current) {
		return false
	}
	if strings.TrimSpace(record.Outcome) == implementProgressOutcomeNoProgress {
		return true
	}
	return !implementProgressSignatureUsable(current) &&
		strings.TrimSpace(record.Outcome) == string(store.WorkAttemptTerminalSuccess) &&
		strings.TrimSpace(record.Reason) == "no_linked_pull_request" &&
		record.WorkspaceDiffStats.Status != "" &&
		record.WorkspaceDiffStats.FilesChanged == 0 &&
		record.WorkspaceDiffStats.AddedLines == 0 &&
		record.WorkspaceDiffStats.RemovedLines == 0
}

func implementProgressRecordFromAttempt(attempt store.WorkAttempt) (implementProgressRecord, bool) {
	if attempt.TerminalState != store.WorkAttemptTerminalSuccess &&
		attempt.TerminalState != store.WorkAttemptTerminalNoProgress {
		return implementProgressRecord{}, false
	}
	var root struct {
		CompletionProgress implementProgressRecord `json:"completion_progress"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(attempt.WorkerMetadataJSON)), &root); err != nil {
		return implementProgressRecord{}, false
	}
	record := root.CompletionProgress
	if strings.TrimSpace(record.Outcome) == "" {
		return implementProgressRecord{}, false
	}
	return record, true
}

func implementCompletionProgressMetadata(decision implementCompletionProgressDecision) map[string]any {
	record := implementProgressRecord{
		Outcome:                string(decision.Outcome),
		Reason:                 decision.Reason,
		CurrentSignature:       implementProgressSignatureRecordFromSignature(decision.CurrentSignature),
		PreviousHeadSHA:        decision.PreviousSignature.HeadSHA,
		CurrentHeadSHA:         decision.CurrentSignature.HeadSHA,
		FailedChecksAdded:      append([]string(nil), decision.FailedChecksAdded...),
		FailedChecksRemoved:    append([]string(nil), decision.FailedChecksRemoved...),
		WorkspaceDiffStats:     implementProgressDiffStatsFromDiffStats(decision.WorkspaceDiffStats),
		ConsecutiveNoProgress:  decision.ConsecutiveNoProgress,
		WorkpadStatus:          strings.TrimSpace(decision.WorkpadStatus),
		HumanAction:            strings.TrimSpace(decision.HumanAction),
		TrackerState:           strings.TrimSpace(decision.TrackerState),
		ConsecutiveHumanAction: decision.ConsecutiveHumanAction,
		NoProgressLimit:        decision.NoProgressLimit,
		BlockReason:            strings.TrimSpace(decision.BlockReason),
		Warning:                strings.TrimSpace(decision.Warning),
	}
	if decision.PreviousSignatureFound {
		previous := implementProgressSignatureRecordFromSignature(decision.PreviousSignature)
		record.PreviousSignature = &previous
	}
	return map[string]any{implementProgressMetadataKey: record}
}

func implementProgressSignatureRecordFromSignature(signature autoPromoteReworkSignature) implementProgressSignatureRecord {
	return implementProgressSignatureRecord{
		PRNumber:     signature.PRNumber,
		HeadSHA:      strings.TrimSpace(signature.HeadSHA),
		FailedChecks: append([]string(nil), signature.FailedChecks...),
	}
}

func (r implementProgressSignatureRecord) signature() autoPromoteReworkSignature {
	return autoPromoteReworkSignature{
		PRNumber:     r.PRNumber,
		HeadSHA:      strings.TrimSpace(r.HeadSHA),
		FailedChecks: autoPromoteCanonicalChecks(r.FailedChecks),
	}
}

func implementProgressDiffStatsFromDiffStats(diffStats DiffStats) implementProgressDiffStats {
	return implementProgressDiffStats{
		FilesChanged: diffStats.FilesChanged,
		AddedLines:   diffStats.AddedLines,
		RemovedLines: diffStats.RemovedLines,
		Fingerprint:  strings.TrimSpace(diffStats.Fingerprint),
		Status:       strings.TrimSpace(diffStats.Status),
	}
}

func implementProgressSignatureUsable(signature autoPromoteReworkSignature) bool {
	return signature.PRNumber > 0 && strings.TrimSpace(signature.HeadSHA) != ""
}

func implementProgressSignatureEqual(left autoPromoteReworkSignature, right autoPromoteReworkSignature) bool {
	return left.PRNumber == right.PRNumber &&
		strings.TrimSpace(left.HeadSHA) == strings.TrimSpace(right.HeadSHA) &&
		slices.Equal(autoPromoteCanonicalChecks(left.FailedChecks), autoPromoteCanonicalChecks(right.FailedChecks))
}

func implementProgressDiffStatsClean(diffStats DiffStats) bool {
	return diffStatsPresent(diffStats) &&
		diffStats.FilesChanged == 0 &&
		diffStats.AddedLines == 0 &&
		diffStats.RemovedLines == 0
}

func implementProgressLinkedPullRequest(issue connector.Issue) bool {
	return workAttemptPRNumber(issue) != nil
}

func implementProgressHydrationUnavailableReason(pullRequest *connector.PullRequest) string {
	reasons := make([]string, 0, 2)
	if reason := pullRequestHydrationUnavailableReason(pullRequest); reason != "" {
		reasons = append(reasons, reason)
	}
	if reason := pullRequestHydrationDegradedReason(pullRequest); reason != "" {
		reasons = append(reasons, reason)
	}
	return strings.Join(reasons, "; ")
}

func implementProgressFailedCheckDelta(previous []string, current []string) ([]string, []string) {
	previous = autoPromoteCanonicalChecks(previous)
	current = autoPromoteCanonicalChecks(current)
	added := make([]string, 0)
	removed := make([]string, 0)
	for _, check := range current {
		if !slices.Contains(previous, check) {
			added = append(added, check)
		}
	}
	for _, check := range previous {
		if !slices.Contains(current, check) {
			removed = append(removed, check)
		}
	}
	return added, removed
}

func (o *Orchestrator) warnImplementProgressHydration(issue connector.Issue, message string, err error) {
	if o == nil || o.logger == nil {
		return
	}
	attrs := []any{
		"issue_id", issue.ID,
		"identifier", issue.Identifier,
		"reason", strings.TrimSpace(message),
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logger.Warn("implement worker progress check failed open", attrs...)
}

func (o *Orchestrator) warnImplementProgressRefresh(issue connector.Issue, message string, err error) {
	if o == nil || o.logger == nil {
		return
	}
	attrs := []any{
		"issue_id", issue.ID,
		"identifier", issue.Identifier,
		"reason", strings.TrimSpace(message),
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logger.Warn("implement worker tracker refresh failed open", attrs...)
}

func (o *Orchestrator) blockImplementProgress(
	ctx context.Context,
	state *State,
	decision implementCompletionProgressDecision,
	blockedAt time.Time,
) bool {
	issue := cloneIssue(decision.Issue)
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return false
	}
	blockReason := strings.TrimSpace(decision.BlockReason)
	if blockReason == "" {
		blockReason = noProgressLimitReason
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, blockedStatusState, blockedAt, blockReason); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"no progress limit state transition failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"target_state", blockedStatusState,
				"error", err,
			)
		}
		return false
	}
	issue.State = blockedStatusState
	stageUpdatedAt := blockedAt.UTC()
	issue.StageUpdatedAt = &stageUpdatedAt
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issueID, implementProgressBlockComment(issue, decision)); err != nil && o.logger != nil {
			o.logger.Warn("no progress limit comment failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("no progress limit claim release failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	if completed, ok := state.Completed[issueID]; ok {
		completed.Issue = issue
		state.Completed[issueID] = completed
	}
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issueID] = Blocked{
		Issue:          issue,
		Reason:         blockReason,
		RecoveryReason: implementProgressRecoveryReason(decision),
		RecoveryTarget: "Rework",
		BlockedAt:      blockedAt,
		Source:         BlockedSourceProjectStatus,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      blockedAt,
		Event:   "implement_worker_no_progress_limit",
		Message: "parked " + issueLabel(issue) + " after implement progress breaker " + blockReason,
	})
	return true
}

func implementProgressRecoveryReason(decision implementCompletionProgressDecision) string {
	if humanAction := strings.TrimSpace(decision.HumanAction); humanAction != "" {
		return humanAction
	}
	return "inspect the linked PR and worker logs, then move the issue back to Rework or Todo after a human confirms the next action"
}

func implementProgressBlockComment(issue connector.Issue, decision implementCompletionProgressDecision) string {
	var b strings.Builder
	b.WriteString("Routed this issue to Blocked because the implement worker completed repeatedly without deliverable progress.")
	b.WriteString("\n\n- reason: ")
	blockReason := strings.TrimSpace(decision.BlockReason)
	if blockReason == "" {
		blockReason = noProgressLimitReason
	}
	b.WriteString(blockReason)
	if decision.NoProgressLimit > 0 {
		b.WriteString("\n- no_progress_limit: ")
		b.WriteString(strconv.Itoa(decision.NoProgressLimit))
	}
	if decision.ConsecutiveNoProgress > 0 {
		b.WriteString("\n- consecutive_no_progress_attempts: ")
		b.WriteString(strconv.Itoa(decision.ConsecutiveNoProgress))
	}
	if decision.ConsecutiveHumanAction > 0 {
		b.WriteString("\n- consecutive_human_action_attempts: ")
		b.WriteString(strconv.Itoa(decision.ConsecutiveHumanAction))
	}
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			b.WriteString("\n- pull request: ")
			b.WriteString(url)
		}
	}
	if decision.CurrentSignature.PRNumber > 0 {
		b.WriteString("\n- pr_number: ")
		b.WriteString(strconv.FormatInt(decision.CurrentSignature.PRNumber, 10))
	}
	if headSHA := strings.TrimSpace(decision.CurrentSignature.HeadSHA); headSHA != "" {
		b.WriteString("\n- current_head_sha: ")
		b.WriteString(headSHA)
	}
	if previousHeadSHA := strings.TrimSpace(decision.PreviousSignature.HeadSHA); previousHeadSHA != "" {
		b.WriteString("\n- previous_head_sha: ")
		b.WriteString(previousHeadSHA)
	}
	if failedChecks := strings.Join(decision.CurrentSignature.FailedChecks, ", "); failedChecks != "" {
		b.WriteString("\n- failed_checks: ")
		b.WriteString(failedChecks)
	}
	if added := strings.Join(decision.FailedChecksAdded, ", "); added != "" {
		b.WriteString("\n- failed_checks_added: ")
		b.WriteString(added)
	}
	if removed := strings.Join(decision.FailedChecksRemoved, ", "); removed != "" {
		b.WriteString("\n- failed_checks_removed: ")
		b.WriteString(removed)
	}
	b.WriteString("\n- workspace_diffstat: ")
	if diffStatsPresent(decision.WorkspaceDiffStats) {
		b.WriteString(strconv.Itoa(decision.WorkspaceDiffStats.FilesChanged))
		b.WriteString(" files, +")
		b.WriteString(strconv.Itoa(decision.WorkspaceDiffStats.AddedLines))
		b.WriteString("/-")
		b.WriteString(strconv.Itoa(decision.WorkspaceDiffStats.RemovedLines))
		if status := strings.TrimSpace(decision.WorkspaceDiffStats.Status); status != "" {
			b.WriteString(" (")
			b.WriteString(status)
			b.WriteString(")")
		}
	} else {
		b.WriteString("unavailable")
	}
	if humanAction := strings.TrimSpace(decision.HumanAction); humanAction != "" {
		b.WriteString("\n\nHuman action requested by the Workpad:\n\n")
		for line := range strings.SplitSeq(humanAction, "\n") {
			b.WriteString("> ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}
