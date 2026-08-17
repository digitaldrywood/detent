package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const terminalAttemptWithoutWorkProductReason = "terminal_attempt_without_work_product"

func (o *Orchestrator) demoteTerminalAttemptRetry(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	workProductPushed bool,
	at time.Time,
) (connector.Issue, bool) {
	if o == nil || o.connector == nil ||
		normalizeState(issue.State) != normalizeState(planImplementationState) ||
		terminalAttemptHasWorkProduct(issue, workProductPushed) ||
		pullRequestHydrationBlocksProgress(issue.PullRequest) ||
		o.terminalAttemptClaimBlocksDemotion(ctx, issue, at) {
		return issue, false
	}
	targetState := terminalAttemptTodoState(o.cfg.ActiveStates)
	if targetState == "" {
		return issue, false
	}
	if err := o.updateIssueState(ctx, state, issue, targetState, at, terminalAttemptWithoutWorkProductReason); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"terminal attempt retry demotion failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"from_state", issue.State,
				"target_state", targetState,
				"error", err,
			)
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      at,
			Event:   "terminal_attempt_retry_demotion_failed",
			Message: "failed to move " + issueLabel(issue) + " to " + targetState + " after a terminal attempt without pushed work",
		})
		return issue, false
	}

	fromState := issue.State
	issue.State = targetState
	if claimed, ok := state.Claimed[issue.ID]; ok {
		claimed.Issue = cloneIssue(issue)
		state.Claimed[issue.ID] = claimed
	}
	if retry, ok := state.Retry[issue.ID]; ok {
		retry.Issue = cloneIssue(issue)
		state.Retry[issue.ID] = retry
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      at,
		Event:   "terminal_attempt_retry_demoted",
		Message: "moved " + issueLabel(issue) + " from " + fromState + " to " + targetState + " after a terminal attempt without pushed work",
	})
	if o.logger != nil {
		o.logger.Info(
			"terminal attempt retry demoted",
			"issue_id", issue.ID,
			"identifier", issue.Identifier,
			"from_state", fromState,
			"target_state", targetState,
			"reason", terminalAttemptWithoutWorkProductReason,
		)
	}
	return issue, true
}

func (o *Orchestrator) releaseTerminalAttemptClaim(ctx context.Context, state *State, issue connector.Issue, at time.Time) {
	if err := o.abandonClaim(ctx, issue.ID); err != nil {
		if o.logger != nil {
			o.logger.Warn("terminal attempt claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      at,
			Event:   "claim_release_failed",
			Message: "claim lease release failed for " + issueLabel(issue) + " after terminal attempt: " + err.Error(),
		})
	}
	delete(state.Claimed, issue.ID)
}

func (o *Orchestrator) reconcileTerminalAttemptRetryStates(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) []connector.Issue {
	if state == nil || len(issues) == 0 || len(state.WorkAttempts) == 0 {
		return nil
	}
	latestByIssue := latestTerminalAttemptsByIssue(state.WorkAttempts)
	transitions := make([]connector.Issue, 0)
	seen := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}
		if _, ok := seen[issueID]; ok {
			continue
		}
		seen[issueID] = struct{}{}
		if normalizeState(issue.State) != normalizeState(planImplementationState) {
			continue
		}
		if _, running := state.Running[issueID]; running {
			continue
		}
		attempt, ok := latestByIssue[issueID]
		if !ok || !terminalAttemptRetryableFailure(attempt) {
			continue
		}
		updated, changed := o.demoteTerminalAttemptRetry(
			ctx,
			state,
			issue,
			workAttemptHasPushedProduct(attempt),
			now,
		)
		if changed {
			transitions = append(transitions, updated)
		}
	}
	return transitions
}

func (o *Orchestrator) terminalAttemptClaimBlocksDemotion(ctx context.Context, issue connector.Issue, now time.Time) bool {
	if !o.cfg.Claiming.Enabled {
		return false
	}
	owner := o.claimOwner()
	if owner == "" || o.cfg.Claiming.LeaseField == "" || o.fieldClaimMissingOwnerField() {
		return true
	}
	return !o.claimable(ctx, issue, owner, now)
}

func latestTerminalAttemptsByIssue(attempts []telemetry.WorkAttempt) map[string]telemetry.WorkAttempt {
	latest := make(map[string]telemetry.WorkAttempt)
	for _, attempt := range attempts {
		if !strings.EqualFold(strings.TrimSpace(attempt.Status), string(store.WorkAttemptStatusTerminal)) {
			continue
		}
		issueID := strings.TrimSpace(attempt.IssueID)
		if issueID == "" {
			continue
		}
		current, ok := latest[issueID]
		if ok && !workAttemptCompletedAfter(attempt, current) {
			continue
		}
		latest[issueID] = attempt
	}
	return latest
}

func workAttemptCompletedAfter(left telemetry.WorkAttempt, right telemetry.WorkAttempt) bool {
	if left.CompletedAt != nil && right.CompletedAt != nil && !left.CompletedAt.Equal(*right.CompletedAt) {
		return left.CompletedAt.After(*right.CompletedAt)
	}
	if left.CompletedAt != nil && right.CompletedAt == nil {
		return true
	}
	if left.CompletedAt == nil && right.CompletedAt != nil {
		return false
	}
	return left.AttemptID > right.AttemptID
}

func terminalAttemptRetryableFailure(attempt telemetry.WorkAttempt) bool {
	if strings.TrimSpace(attempt.ErrorClass) == forgeUnavailableErrorClass {
		return false
	}
	return terminalAttemptStateRetryDemotable(store.WorkAttemptTerminalState(strings.ToLower(strings.TrimSpace(attempt.TerminalState))))
}

func terminalAttemptStateRetryDemotable(state store.WorkAttemptTerminalState) bool {
	switch state {
	case store.WorkAttemptTerminalFailure,
		store.WorkAttemptTerminalTimedOut,
		store.WorkAttemptTerminalAbandoned,
		store.WorkAttemptTerminalCapacity:
		return true
	default:
		return false
	}
}

func terminalAttemptTodoState(activeStates []string) string {
	for _, state := range activeStates {
		if normalizeState(state) == "todo" {
			return displayStateName(state)
		}
	}
	return ""
}

func terminalAttemptHasWorkProduct(issue connector.Issue, pushed bool) bool {
	return pushed || implementProgressLinkedPullRequest(issue)
}

func workAttemptHasPushedProduct(attempt telemetry.WorkAttempt) bool {
	return attempt.PRNumber != nil && *attempt.PRNumber > 0 || workAttemptMetadataHasPushedProduct(attempt.WorkerMetadataJSON)
}

func workAttemptMetadataHasPushedProduct(raw string) bool {
	var metadata struct {
		WorkProductPushed bool `json:"work_product_pushed"`
	}
	return json.Unmarshal([]byte(raw), &metadata) == nil && metadata.WorkProductPushed
}
