package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	spendProgressMetadataKey = "spend_since_progress_breaker"
	spendProgressReason      = "spend_since_progress_circuit_breaker"
	spendProgressHistorySize = 200
)

type spendProgressDecision struct {
	Enabled             bool
	AcceptedStateChange bool
	AcceptedReason      string
	Since               time.Time
	Spend               store.IssueSpendSince
	LimitUSD            float64
	Block               bool
	Warning             string
}

type spendProgressRecord struct {
	AcceptedStateChange bool    `json:"accepted_state_change,omitempty"`
	AcceptedReason      string  `json:"accepted_reason,omitempty"`
	Since               string  `json:"since,omitempty"`
	SpendUSD            float64 `json:"spend_usd,omitempty"`
	Sessions            int64   `json:"sessions,omitempty"`
	FirstSessionAt      string  `json:"first_session_at,omitempty"`
	LastSessionAt       string  `json:"last_session_at,omitempty"`
	LimitUSD            float64 `json:"limit_usd,omitempty"`
	BlockReason         string  `json:"block_reason,omitempty"`
	Warning             string  `json:"warning,omitempty"`
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
	decision.LimitUSD = o.cfg.NoProgressSpendLimitUSD
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
	decision.Block = spend.CostUSD > decision.LimitUSD
	return decision
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
		LimitUSD:            decision.LimitUSD,
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
		RecoveryReason: "narrow or split the task, then identify the missing accepted progress signal before moving the issue back to Todo or Rework",
		RecoveryTarget: "Rework",
		BlockedAt:      blockedAt,
		Source:         BlockedSourceProjectStatus,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      blockedAt,
		Event:   "spend_since_progress_circuit_breaker_tripped",
		Message: fmt.Sprintf("parked %s after %s without an accepted state change", issueLabel(issue), budget.FormatUSD(decision.Spend.CostUSD)),
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
		)
	}
	return true
}

func spendProgressComment(issue connector.Issue, decision spendProgressDecision) string {
	var b strings.Builder
	b.WriteString("Routed this issue to Blocked because agent spend continued without an accepted state change.")
	b.WriteString("\n\n- reason: ")
	b.WriteString(spendProgressReason)
	b.WriteString("\n- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- spend_since_last_accepted_state_change: ")
	b.WriteString(budget.FormatUSD(decision.Spend.CostUSD))
	b.WriteString("\n- no_progress_spend_limit_usd: ")
	b.WriteString(budget.FormatUSD(decision.LimitUSD))
	b.WriteString(fmt.Sprintf("\n- sessions: %d", decision.Spend.Sessions))
	if !decision.Since.IsZero() {
		b.WriteString("\n- last_accepted_state_change_at: ")
		b.WriteString(decision.Since.UTC().Format(time.RFC3339))
	}
	if !decision.Spend.FirstSessionAt.IsZero() && !decision.Spend.LastSessionAt.IsZero() {
		b.WriteString("\n- observed_session_span: ")
		b.WriteString(decision.Spend.LastSessionAt.Sub(decision.Spend.FirstSessionAt).Round(time.Second).String())
	}
	b.WriteString("\n\nShrink the task before retrying: split or narrow the scope so the next session can produce a concrete accepted change.")
	b.WriteString("\n\nOn the next dispatch, the agent's first tool action must update the Workpad to explain which accepted progress signal was missing and what is concretely different before any other tool use.")
	return b.String()
}

func spendProgressRetryHandoff(decision spendProgressDecision) runpkg.PriorAttempt {
	return runpkg.PriorAttempt{
		Source:                  spendProgressReason,
		Reason:                  "spend exceeded the configured limit without an accepted state change",
		ExplainBeforeRetry:      true,
		MissingSignal:           "lane transition, pull request creation or merge, or a recognized PR content-signature change",
		ObservedSpendUSD:        decision.Spend.CostUSD,
		NoProgressSpendLimitUSD: decision.LimitUSD,
	}
}

func (o *Orchestrator) spendProgressPriorAttempt(ctx context.Context, issue connector.Issue) (runpkg.PriorAttempt, bool) {
	if o == nil || o.cfg.NoProgressSpendLimitUSD <= 0 || o.workAttempts == nil {
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
		Spend:    store.IssueSpendSince{CostUSD: record.SpendUSD, Sessions: record.Sessions},
		LimitUSD: record.LimitUSD,
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
