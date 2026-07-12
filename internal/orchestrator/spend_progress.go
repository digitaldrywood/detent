package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	spendProgressMetadataKey = "spend_since_progress_breaker"
	spendProgressReason      = "spend_since_progress_circuit_breaker"
	spendProgressHistorySize = 200
	spendProgressCaseNoPR    = "spend_without_pr_evidence"
	spendProgressCaseStatic  = "spend_with_static_pr_evidence"
)

type spendProgressDecision struct {
	Enabled             bool
	AcceptedStateChange bool
	AcceptedReason      string
	Since               time.Time
	Spend               store.IssueSpendSince
	ConfiguredLimitUSD  float64
	LimitUSD            float64
	Effort              string
	PRFingerprint       *spendProgressPRFingerprint
	Case                string
	Block               bool
	Warning             string
}

type spendProgressRecord struct {
	AcceptedStateChange bool                        `json:"accepted_state_change,omitempty"`
	AcceptedReason      string                      `json:"accepted_reason,omitempty"`
	Since               string                      `json:"since,omitempty"`
	SpendUSD            float64                     `json:"spend_usd,omitempty"`
	Sessions            int64                       `json:"sessions,omitempty"`
	FirstSessionAt      string                      `json:"first_session_at,omitempty"`
	LastSessionAt       string                      `json:"last_session_at,omitempty"`
	ConfiguredLimitUSD  float64                     `json:"configured_limit_usd,omitempty"`
	LimitUSD            float64                     `json:"limit_usd,omitempty"`
	Effort              string                      `json:"effort,omitempty"`
	PRFingerprint       *spendProgressPRFingerprint `json:"pr_fingerprint,omitempty"`
	Case                string                      `json:"case,omitempty"`
	BlockReason         string                      `json:"block_reason,omitempty"`
	Warning             string                      `json:"warning,omitempty"`
}

type spendProgressPRFingerprint struct {
	Number         int64  `json:"number,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
	CIStatus       string `json:"ci_status,omitempty"`
}

func (o *Orchestrator) evaluateSpendProgress(
	ctx context.Context,
	running Running,
	completedAt time.Time,
	accepted bool,
	acceptedReason string,
) spendProgressDecision {
	decision := spendProgressDecision{
		Enabled:             o != nil && o.cfg.NoProgressSpendLimitUSD > 0,
		AcceptedStateChange: accepted,
		AcceptedReason:      strings.TrimSpace(acceptedReason),
	}
	if !decision.Enabled {
		return decision
	}
	decision.ConfiguredLimitUSD = o.cfg.NoProgressSpendLimitUSD
	decision.Effort = spendProgressEffort(running)
	decision.LimitUSD = workflowconfig.EffectiveNoProgressSpendLimitUSD(decision.ConfiguredLimitUSD, decision.Effort)
	decision.PRFingerprint = spendProgressPRFingerprintFromIssue(running.Issue)
	if accepted {
		decision.Since = completedAt
		return decision
	}
	if o.progressSpend == nil {
		decision.Warning = "progress spend store unavailable"
		o.warnSpendProgress(running.Issue, decision.Warning, nil)
		return decision
	}

	attempts, err := o.recentSpendProgressAttempts(ctx, running.Issue)
	if err != nil {
		decision.Warning = err.Error()
		o.warnSpendProgress(running.Issue, "work attempt history lookup failed", err)
		return decision
	}
	if previous, ok := latestSpendProgressPRFingerprint(attempts); ok {
		if reason := spendProgressPRAdvance(previous, decision.PRFingerprint); reason != "" {
			decision.AcceptedStateChange = true
			decision.AcceptedReason = reason
			decision.Since = completedAt
			return decision
		}
	} else if decision.PRFingerprint != nil {
		decision.AcceptedStateChange = true
		decision.AcceptedReason = "pull_request_created"
		decision.Since = completedAt
		return decision
	}
	decision.Since = spendProgressBaseline(running.Issue, attempts)
	if decision.Since.IsZero() {
		decision.Since = time.Unix(0, 0).UTC()
	}
	spend, err := o.progressSpend.IssueSpendSince(ctx, store.IssueSpendSinceQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(running.Issue.ID),
		Identifier: strings.TrimSpace(running.Issue.Identifier),
		Since:      decision.Since,
	})
	if err != nil {
		decision.Warning = err.Error()
		o.warnSpendProgress(running.Issue, "spend lookup failed", err)
		return decision
	}
	decision.Spend = spend
	decision.Case = spendProgressCaseNoPR
	if decision.PRFingerprint != nil {
		decision.Case = spendProgressCaseStatic
	}
	decision.Block = !o.cfg.subscriptionBilling() && spend.CostUSD > decision.LimitUSD
	return decision
}

func spendProgressEffort(running Running) string {
	return strings.ToLower(strings.TrimSpace(running.RuntimeIdentity.ReasoningEffort.Value))
}

func spendProgressPRFingerprintFromIssue(issue connector.Issue) *spendProgressPRFingerprint {
	number := workAttemptPRNumber(issue)
	if number == nil || *number <= 0 {
		return nil
	}
	fingerprint := &spendProgressPRFingerprint{Number: *number}
	if issue.PullRequest == nil {
		return fingerprint
	}
	fingerprint.HeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	fingerprint.MergeableState = strings.ToLower(strings.TrimSpace(issue.PullRequest.MergeableState))
	fingerprint.CIStatus = strings.ToLower(strings.TrimSpace(issue.PullRequest.CIStatus))
	return fingerprint
}

func latestSpendProgressPRFingerprint(attempts []store.WorkAttempt) (*spendProgressPRFingerprint, bool) {
	for _, attempt := range attempts {
		record, ok := spendProgressRecordFromAttempt(attempt)
		if !ok || record.PRFingerprint == nil || record.PRFingerprint.Number <= 0 {
			continue
		}
		fingerprint := *record.PRFingerprint
		return &fingerprint, true
	}
	return nil, false
}

func spendProgressPRAdvance(previous *spendProgressPRFingerprint, current *spendProgressPRFingerprint) string {
	if previous == nil || current == nil {
		return ""
	}
	if previous.Number != current.Number {
		return "pull_request_created"
	}
	if previous.HeadSHA != "" && current.HeadSHA != "" && previous.HeadSHA != current.HeadSHA {
		return "pull_request_head_changed"
	}
	if previous.MergeableState == "dirty" && current.MergeableState == "clean" {
		return "pull_request_mergeable"
	}
	if spendProgressCIFailing(previous.CIStatus) && spendProgressCIPassing(current.CIStatus) {
		return "pull_request_ci_passing"
	}
	return ""
}

func spendProgressCIFailing(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "fail", "failed", "failure", "failing":
		return true
	default:
		return false
	}
}

func spendProgressCIPassing(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pass", "passed", "passing", "success", "successful":
		return true
	default:
		return false
	}
}

func (o *Orchestrator) recentSpendProgressAttempts(ctx context.Context, issue connector.Issue) ([]store.WorkAttempt, error) {
	if o == nil || o.workAttempts == nil {
		return nil, nil
	}
	return o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		Limit:      spendProgressHistorySize,
	})
}

func (o *Orchestrator) refreshSpendProgressIssue(ctx context.Context, issue connector.Issue) (connector.Issue, string) {
	refreshed := cloneIssue(issue)
	if !implementProgressLinkedPullRequest(refreshed) {
		if o == nil || o.connector == nil || strings.TrimSpace(refreshed.ID) == "" {
			return refreshed, "pull request evidence refresh unavailable"
		}
		issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{refreshed.ID})
		if err != nil {
			return refreshed, "pull request evidence refresh failed: " + err.Error()
		}
		for _, candidate := range issues {
			if strings.TrimSpace(candidate.ID) == strings.TrimSpace(refreshed.ID) {
				refreshed = mergeIssueTrackerFields(refreshed, candidate)
				break
			}
		}
	}
	if !implementProgressLinkedPullRequest(refreshed) {
		return refreshed, ""
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		return refreshed, "pull request hydrator unavailable"
	}
	hydrated, err := hydrator.HydratePullRequest(ctx, refreshed)
	if err != nil {
		return refreshed, "pull request hydration failed: " + err.Error()
	}
	if reason := implementProgressHydrationUnavailableReason(hydrated.PullRequest); reason != "" {
		return hydrated, reason
	}
	return hydrated, ""
}

func spendProgressBaseline(issue connector.Issue, attempts []store.WorkAttempt) time.Time {
	baseline := time.Time{}
	for _, candidate := range []*time.Time{issue.CreatedAt, issue.StageUpdatedAt} {
		if candidate != nil && candidate.After(baseline) {
			baseline = candidate.UTC()
		}
	}
	for _, attempt := range attempts {
		if !spendProgressAttemptAccepted(attempt) || !attempt.CompletedAt.After(baseline) {
			continue
		}
		baseline = attempt.CompletedAt.UTC()
	}
	return baseline
}

func spendProgressAttemptAccepted(attempt store.WorkAttempt) bool {
	if record, ok := spendProgressRecordFromAttempt(attempt); ok && record.AcceptedStateChange {
		return true
	}
	record, ok := implementProgressRecordFromAttempt(attempt)
	if !ok {
		return false
	}
	switch strings.TrimSpace(record.Reason) {
	case "pull_request_created_or_updated", "signature_changed":
		return true
	default:
		return false
	}
}

func spendProgressRecordFromAttempt(attempt store.WorkAttempt) (spendProgressRecord, bool) {
	var root struct {
		SpendProgress spendProgressRecord `json:"spend_since_progress_breaker"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(attempt.WorkerMetadataJSON)), &root); err != nil {
		return spendProgressRecord{}, false
	}
	record := root.SpendProgress
	if !record.AcceptedStateChange && record.LimitUSD <= 0 && strings.TrimSpace(record.BlockReason) == "" {
		return spendProgressRecord{}, false
	}
	return record, true
}

func spendProgressMetadata(decision spendProgressDecision) map[string]any {
	if !decision.Enabled {
		return nil
	}
	record := spendProgressRecord{
		AcceptedStateChange: decision.AcceptedStateChange,
		AcceptedReason:      decision.AcceptedReason,
		SpendUSD:            decision.Spend.CostUSD,
		Sessions:            decision.Spend.Sessions,
		ConfiguredLimitUSD:  decision.ConfiguredLimitUSD,
		LimitUSD:            decision.LimitUSD,
		Effort:              decision.Effort,
		PRFingerprint:       decision.PRFingerprint,
		Case:                decision.Case,
		Warning:             strings.TrimSpace(decision.Warning),
	}
	if !decision.Since.IsZero() {
		record.Since = decision.Since.UTC().Format(time.RFC3339Nano)
	}
	if !decision.Spend.FirstSessionAt.IsZero() {
		record.FirstSessionAt = decision.Spend.FirstSessionAt.UTC().Format(time.RFC3339Nano)
	}
	if !decision.Spend.LastSessionAt.IsZero() {
		record.LastSessionAt = decision.Spend.LastSessionAt.UTC().Format(time.RFC3339Nano)
	}
	if decision.Block {
		record.BlockReason = spendProgressReason
	}
	return map[string]any{spendProgressMetadataKey: record}
}

func mergeWorkAttemptMetadata(groups ...map[string]any) map[string]any {
	var merged map[string]any
	for _, group := range groups {
		for key, value := range group {
			if merged == nil {
				merged = map[string]any{}
			}
			merged[key] = value
		}
	}
	return merged
}

func implementAcceptedStateChange(running Running, decision implementCompletionProgressDecision) (bool, string) {
	if accepted, reason := dispatchAcceptedStateChange(running); accepted {
		return true, reason
	}
	switch strings.TrimSpace(decision.Reason) {
	case "pull_request_created_or_updated", "signature_changed":
		return true, decision.Reason
	default:
		return false, ""
	}
}

func dispatchAcceptedStateChange(running Running) (bool, string) {
	source := strings.TrimSpace(running.DispatchSourceState)
	target := strings.TrimSpace(running.DispatchTargetState)
	if source != "" && target != "" && !strings.EqualFold(source, target) {
		return true, "lane_transition"
	}
	return false, ""
}

func (o *Orchestrator) blockSpendProgress(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	decision spendProgressDecision,
	blockedAt time.Time,
) bool {
	issue = cloneIssue(issue)
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return false
	}
	if err := o.updateIssueStateByID(ctx, state, issueID, issue, blockedStatusState, blockedAt, spendProgressReason); err != nil {
		if o.logger != nil {
			o.logger.Warn("spend progress circuit breaker state transition failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
		}
		return false
	}
	issue.State = blockedStatusState
	stageUpdatedAt := blockedAt.UTC()
	issue.StageUpdatedAt = &stageUpdatedAt
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issueID, spendProgressComment(issue, decision)); err != nil && o.logger != nil {
			o.logger.Warn("spend progress circuit breaker comment failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("spend progress circuit breaker claim release failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	if state.PriorAttempts == nil {
		state.PriorAttempts = map[string]runpkg.PriorAttempt{}
	}
	state.PriorAttempts[issueID] = spendProgressRetryHandoff(decision)
	if completed, ok := state.Completed[issueID]; ok {
		completed.Issue = issue
		state.Completed[issueID] = completed
	}
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issueID] = Blocked{
		Issue:          issue,
		Reason:         spendProgressReason,
		RecoveryReason: spendProgressRecoveryReason(decision),
		RecoveryTarget: "Rework",
		BlockedAt:      blockedAt,
		Source:         BlockedSourceProjectStatus,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      blockedAt,
		Event:   "spend_since_progress_circuit_breaker_tripped",
		Message: fmt.Sprintf("parked %s after %s: %s", issueLabel(issue), budget.FormatUSD(decision.Spend.CostUSD), spendProgressCaseSummary(decision.Case)),
	})
	if o.logger != nil {
		o.logger.Error("spend since progress circuit breaker tripped",
			"event", "spend_since_progress_circuit_breaker_tripped",
			"issue_id", issueID,
			"issue_identifier", issue.Identifier,
			"spend_usd", decision.Spend.CostUSD,
			"limit_usd", decision.LimitUSD,
			"sessions", decision.Spend.Sessions,
			"since", decision.Since,
			"case", decision.Case,
			"effort", decision.Effort,
		)
	}
	return true
}

func spendProgressComment(issue connector.Issue, decision spendProgressDecision) string {
	var b strings.Builder
	b.WriteString("Routed this issue to Blocked because ")
	b.WriteString(spendProgressCaseSummary(decision.Case))
	b.WriteString(".")
	b.WriteString("\n\n- reason: ")
	b.WriteString(spendProgressReason)
	b.WriteString("\n- case: ")
	b.WriteString(decision.Case)
	b.WriteString("\n- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- spend_since_last_accepted_progress: ")
	b.WriteString(budget.FormatUSD(decision.Spend.CostUSD))
	b.WriteString("\n- no_progress_spend_limit_usd: ")
	b.WriteString(budget.FormatUSD(decision.LimitUSD))
	if decision.Effort != "" {
		b.WriteString("\n- effective_effort: ")
		b.WriteString(decision.Effort)
		b.WriteString("\n- configured_base_limit_usd: ")
		b.WriteString(budget.FormatUSD(decision.ConfiguredLimitUSD))
	}
	b.WriteString(fmt.Sprintf("\n- sessions: %d", decision.Spend.Sessions))
	if decision.PRFingerprint != nil {
		b.WriteString("\n- pr_number: ")
		b.WriteString(strconv.FormatInt(decision.PRFingerprint.Number, 10))
		if decision.PRFingerprint.HeadSHA != "" {
			b.WriteString("\n- pr_head_sha: ")
			b.WriteString(decision.PRFingerprint.HeadSHA)
		}
		if decision.PRFingerprint.MergeableState != "" {
			b.WriteString("\n- pr_mergeable_state: ")
			b.WriteString(decision.PRFingerprint.MergeableState)
		}
		if decision.PRFingerprint.CIStatus != "" {
			b.WriteString("\n- pr_ci_status: ")
			b.WriteString(decision.PRFingerprint.CIStatus)
		}
	}
	if !decision.Since.IsZero() {
		b.WriteString("\n- last_accepted_progress_at: ")
		b.WriteString(decision.Since.UTC().Format(time.RFC3339))
	}
	if !decision.Spend.FirstSessionAt.IsZero() && !decision.Spend.LastSessionAt.IsZero() {
		b.WriteString("\n- observed_session_span: ")
		b.WriteString(decision.Spend.LastSessionAt.Sub(decision.Spend.FirstSessionAt).Round(time.Second).String())
	}
	if decision.Case == spendProgressCaseStatic {
		b.WriteString("\n\nThe linked PR fingerprint stayed static during this spend window. Check merge-train capacity, Merging serialization, and dispatch priority before narrowing the task; this pattern can indicate throughput starvation rather than missing implementation work.")
	} else {
		b.WriteString("\n\nShrink the task before retrying: split or narrow the scope so the next session can produce a concrete accepted change or linked PR evidence.")
	}
	b.WriteString("\n\nOn the next dispatch, the agent's first tool action must update the Workpad to explain which accepted progress signal was missing and what is concretely different before any other tool use.")
	return b.String()
}

func spendProgressCaseSummary(progressCase string) string {
	if progressCase == spendProgressCaseStatic {
		return "spend continued while a linked PR existed but could not merge"
	}
	return "spend continued without any PR evidence"
}

func spendProgressRecoveryReason(decision spendProgressDecision) string {
	if decision.Case == spendProgressCaseStatic {
		return "inspect merge-train capacity, Merging serialization, and dispatch priority before moving the issue back to Rework"
	}
	return "narrow or split the task, then identify why no linked PR evidence was produced before moving the issue back to Todo or Rework"
}

func spendProgressRetryHandoff(decision spendProgressDecision) runpkg.PriorAttempt {
	missingSignal := "lane transition, pull request creation, or a recognized PR fingerprint advancement"
	if decision.Case == spendProgressCaseStatic {
		missingSignal = "new PR head commit, dirty-to-clean mergeability, failing-to-passing CI, or merge-train capacity that lets the linked PR advance"
	}
	return runpkg.PriorAttempt{
		Source:                  spendProgressReason,
		Reason:                  spendProgressCaseSummary(decision.Case),
		ExplainBeforeRetry:      true,
		MissingSignal:           missingSignal,
		ObservedSpendUSD:        decision.Spend.CostUSD,
		NoProgressSpendLimitUSD: decision.LimitUSD,
	}
}

func (o *Orchestrator) spendProgressPriorAttempt(ctx context.Context, issue connector.Issue) (runpkg.PriorAttempt, bool) {
	if o == nil || o.cfg.subscriptionBilling() || o.cfg.NoProgressSpendLimitUSD <= 0 || o.workAttempts == nil {
		return runpkg.PriorAttempt{}, false
	}
	attempts, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		Limit:      1,
	})
	if err != nil || len(attempts) == 0 {
		return runpkg.PriorAttempt{}, false
	}
	record, ok := spendProgressRecordFromAttempt(attempts[0])
	if !ok || strings.TrimSpace(record.BlockReason) != spendProgressReason {
		return runpkg.PriorAttempt{}, false
	}
	return spendProgressRetryHandoff(spendProgressDecision{
		Spend:              store.IssueSpendSince{CostUSD: record.SpendUSD, Sessions: record.Sessions},
		ConfiguredLimitUSD: record.ConfiguredLimitUSD,
		LimitUSD:           record.LimitUSD,
		Effort:             record.Effort,
		PRFingerprint:      record.PRFingerprint,
		Case:               record.Case,
	}), true
}

func (o *Orchestrator) warnSpendProgress(issue connector.Issue, message string, err error) {
	if o == nil || o.logger == nil {
		return
	}
	attrs := []any{"issue_id", issue.ID, "identifier", issue.Identifier, "reason", strings.TrimSpace(message)}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logger.Warn("spend since progress breaker failed open", attrs...)
}

func priorAttemptPresent(prior runpkg.PriorAttempt) bool {
	return strings.TrimSpace(prior.Source) != "" || strings.TrimSpace(prior.Reason) != "" || prior.ExplainBeforeRetry || prior.Validator.Submitted
}
