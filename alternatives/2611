package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	terminalAttemptWithoutWorkProductReason = "terminal_attempt_without_work_product"
	terminalAttemptRetryLimitCause          = "terminal_attempt_retry_limit"
	workspacePreparationRetryLimitCause     = "workspace_preparation_retry_limit"
)

func (o *Orchestrator) demoteTerminalAttemptRetry(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	workProductPushed bool,
	retryCause string,
	durable bool,
	runMode string,
	diffStats DiffStats,
	at time.Time,
	currentAttempts ...telemetry.WorkAttempt,
) (connector.Issue, bool, bool) {
	if o == nil || o.connector == nil ||
		normalizeState(issue.State) != normalizeState(planImplementationState) ||
		terminalAttemptHasWorkProduct(issue, workProductPushed) ||
		pullRequestHydrationBlocksProgress(issue.PullRequest) ||
		o.terminalAttemptClaimBlocksDemotion(ctx, issue, at) {
		return issue, false, false
	}
	targetState := terminalAttemptTodoState(o.cfg.ActiveStates)
	if targetState == "" {
		return issue, false, false
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
		return issue, false, false
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
	return issue, true, false
}

func terminalAttemptFailureEvidence(running Running, terminalState store.WorkAttemptTerminalState, errorClass, errorMessage string, at time.Time) telemetry.WorkAttempt {
	return telemetry.WorkAttempt{
		AttemptID:          running.WorkAttemptID,
		AttemptNumber:      running.Attempt,
		IssueID:            running.Issue.ID,
		Identifier:         running.Issue.Identifier,
		Status:             string(store.WorkAttemptStatusTerminal),
		TerminalState:      string(terminalState),
		ErrorClass:         errorClass,
		ErrorMessage:       errorMessage,
		CompletedAt:        &at,
		WorkerMetadataJSON: runningWorkAttemptMetadataJSON(running, nil),
	}
}

func retryCycleCauseForAttempt(attempt telemetry.WorkAttempt) string {
	if strings.TrimSpace(attempt.ErrorClass) == workAttemptErrorWorkspace {
		return workspacePreparationRetryLimitCause
	}
	return terminalAttemptRetryLimitCause
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
		if retry, queued := state.Retry[issueID]; queued && retry.RecoveryAttemptID > 0 {
			continue
		}
		attempt, ok := latestByIssue[issueID]
		if !ok {
			continue
		}
		legacyRESTCapacity := strings.TrimSpace(attempt.ErrorClass) == githubRESTCapacityError && terminalAttemptRetryableFailure(attempt)
		if preTurnAttempt(attempt) || strings.TrimSpace(attempt.ErrorClass) == "service_restart" || legacyRESTCapacity {
			if terminalAttemptHasWorkProduct(issue, workAttemptHasPushedProduct(attempt)) || pullRequestHydrationBlocksProgress(issue.PullRequest) || o.terminalAttemptClaimBlocksDemotion(ctx, issue, now) {
				continue
			}
			var metadata struct {
				Source string `json:"dispatch_source_state"`
			}
			if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) != nil {
				metadata.Source = ""
			}
			if updated, changed, err := o.restorePreTurnIssue(ctx, state, Running{Issue: issue, DispatchSourceState: metadata.Source, WorkProductPushed: workAttemptHasPushedProduct(attempt)}, now); err != nil {
				if o.logger != nil {
					o.logger.Warn("instance failure lane restoration failed", "issue_id", issue.ID, "error", err)
				}
			} else {
				o.releaseTerminalAttemptClaim(ctx, state, updated, now)
				if changed {
					transitions = append(transitions, updated)
				}
			}
			continue
		}
		if allowanceInfrastructureAttempt(store.WorkAttempt{
			TerminalState: store.WorkAttemptTerminalState(attempt.TerminalState), ErrorClass: attempt.ErrorClass,
			Phase: attempt.Phase, MetricsJSON: attempt.MetricsJSON, WorkerMetadataJSON: attempt.WorkerMetadataJSON,
		}) || !terminalAttemptRetryableFailure(attempt) {
			continue
		}
		updated, changed, _ := o.demoteTerminalAttemptRetry(
			ctx,
			state,
			issue,
			workAttemptHasPushedProduct(attempt),
			retryCycleCauseForAttempt(attempt),
			true,
			workAttemptRunMode(attempt),
			DiffStats{},
			now,
		)
		if changed {
			transitions = append(transitions, updated)
		}
	}
	return transitions
}

func workAttemptRunMode(attempt telemetry.WorkAttempt) string {
	var metadata struct {
		RunMode string `json:"run_mode"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) == nil && strings.TrimSpace(metadata.RunMode) != "" {
		return strings.TrimSpace(metadata.RunMode)
	}
	return RunModeImplement
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
	// A plan is a work product without a PR. Abandoning its local completion
	// cannot revoke an operator's subsequent implementation handoff.
	if workAttemptRunMode(attempt) == RunModePlan && strings.EqualFold(strings.TrimSpace(attempt.TerminalState), string(store.WorkAttemptTerminalAbandoned)) {
		return false
	}
	errorClass := strings.TrimSpace(attempt.ErrorClass)
	if errorClass == backendcapacity.ErrorClass || errorClass == forgeUnavailableErrorClass || errorClass == workspaceBranchHoldErrorClass || errorClass == "worker_github_budget_monitor_unavailable" || errorClass == workerGitHubTokenResolutionErrorClass {
		return false
	}
	if errorClass == githubRESTCapacityError {
		_, durable := githubRESTWaitMetadataFromAttempt(store.WorkAttempt{
			TerminalState:      store.WorkAttemptTerminalState(strings.ToLower(strings.TrimSpace(attempt.TerminalState))),
			ErrorClass:         errorClass,
			WorkerMetadataJSON: attempt.WorkerMetadataJSON,
		})
		if durable {
			return false
		}
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

func (o *Orchestrator) retryMachineFailure(ctx context.Context, state *State, issue connector.Issue, attempt int, at time.Time, detail, host string) error {
	if state == nil || o.connector == nil {
		return errors.New("retry machine failure requires state and connector")
	}
	if o.terminalAttemptClaimBlocksDemotion(ctx, issue, at) {
		return errors.New("current lease owns the retry transition")
	}
	target := issue.State
	if normalizeState(target) == normalizeState(blockedStatusState) || !stateIn(target, o.cfg.ActiveStates) {
		target = normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState
		if normalizeState(issue.State) == normalizeState(blockedStatusState) && !dependencyAutoUnblockStartedSignal(issue) {
			target = terminalAttemptTodoState(o.cfg.ActiveStates)
		}
		if !stateIn(target, o.cfg.ActiveStates) {
			target = terminalAttemptTodoState(o.cfg.ActiveStates)
		}
	}
	if reader, ok := o.connector.(connector.WorkflowStateReader); ok {
		states, err := reader.WorkflowStates(ctx)
		if err != nil {
			return fmt.Errorf("read retry workflow: %w", err)
		}
		var allowed bool
		preferred := target
		target, allowed = nativeRetryLane(states, issue.State, issue.State)
		if !allowed {
			target, allowed = nativeRetryLane(states, issue.State, preferred)
		}
		if !allowed {
			for _, candidate := range states {
				target, allowed = nativeRetryLane(states, issue.State, candidate.Name)
				if allowed {
					break
				}
			}
		}
		if !allowed {
			return errors.New("no dispatchable retry lane is reachable")
		}
	}
	if target == "" {
		return errors.New("no dispatchable retry lane is configured")
	}
	if normalizeState(target) != normalizeState(issue.State) {
		if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, target, at, terminalAttemptWithoutWorkProductReason, workflowLaneMetadata{}); err != nil {
			return fmt.Errorf("write retry lane: %w", err)
		}
	}
	issue.State = target
	retry := Retry{Issue: cloneIssue(issue), Attempt: max(attempt, 1), DueAt: at.Add(o.machineRetryDelay(attempt)), Error: o.operatorText(detail), WorkerHost: host}
	if o.workflowMetrics != nil {
		data, err := json.Marshal(retry)
		if err != nil {
			return fmt.Errorf("encode machine retry: %w", err)
		}
		if _, err := o.workflowMetrics.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID: o.workflowMetricsProjectID(), IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL,
			PhaseType: store.WorkflowPhaseTypeRecovery, PhaseName: "machine_retry", Status: "queued", StartedAt: at, FinishedAt: at, MetadataJSON: string(data),
		}); err != nil {
			return fmt.Errorf("persist machine retry: %w", err)
		}
	}
	o.releaseTerminalAttemptClaim(ctx, state, issue, at)
	delete(state.Blocked, issue.ID)
	delete(state.Completed, issue.ID)
	if state.Retry == nil {
		state.Retry = make(map[string]Retry)
	}
	state.Retry[issue.ID] = retry
	return nil
}

func (o *Orchestrator) machineRetryDelay(attempt int) time.Duration {
	delay := o.retryDelay(attempt, false)
	if delay > 0 {
		offset, err := rand.Int(rand.Reader, big.NewInt(int64(delay/8)+1))
		if err == nil {
			delay -= time.Duration(offset.Int64())
		}
	}
	return delay
}

func (o *Orchestrator) restoreMachineRetry(ctx context.Context, state *State, issue connector.Issue) {
	if _, exists := state.Retry[issue.ID]; exists {
		return
	}
	if _, running := state.Running[issue.ID]; running {
		return
	}
	if explicitHumanAction(issue) != "" || state.Blocked[issue.ID].Source == BlockedSourceOperatorStop {
		return
	}
	timeline, ok := o.issueWorkflowTimeline(ctx, issue)
	if !ok {
		return
	}
	for i := len(timeline.Events) - 1; i >= 0; i-- {
		event := timeline.Events[i]
		if event.PhaseType == store.WorkflowPhaseTypeLane {
			return
		}
		if event.PhaseType != store.WorkflowPhaseTypeRecovery || event.PhaseName != "machine_retry" {
			continue
		}
		var retry Retry
		if json.Unmarshal([]byte(event.MetadataJSON), &retry) != nil || retry.Issue.ID != issue.ID || normalizeState(retry.Issue.State) != normalizeState(issue.State) || retry.Attempt < 1 || retry.DueAt.IsZero() {
			return
		}
		if o.workAttempts != nil {
			active, err := o.workAttempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: o.cfg.Project.ID})
			if err != nil {
				return
			}
			for _, attempt := range active {
				if attempt.IssueID == issue.ID {
					return
				}
			}
			latest, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{ProjectID: o.cfg.Project.ID, IssueID: issue.ID, Limit: 1})
			if err != nil {
				return
			}
			if len(latest) > 0 && !latest[0].StartedAt.Before(event.StartedAt) {
				return
			}
		}
		retry.Issue = cloneIssue(issue)
		state.Retry[issue.ID] = retry
		return
	}
}
