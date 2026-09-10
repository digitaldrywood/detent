package orchestrator

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrMissingOperatorMoveIssueID = errors.New("operator move issue id is required")

type OperatorMoveRequest struct {
	Tracker         connector.Connector
	Attribution     provenance.Attribution
	WriteTracker    bool
	Reason          string
	StateFieldID    int
	StateFieldValue string
	ProjectID       string
	IssueID         string
	Identifier      string
	FromState       string
	ToState         string
}

type OperatorMoveResult struct {
	err                  error
	Reconciled           bool
	BlockedCleared       bool
	ClaimCleared         bool
	RetryCleared         bool
	FailureMemoryCleared bool
}

type operatorMoveRequest struct {
	request OperatorMoveRequest
	at      time.Time
	reply   chan OperatorMoveResult
}

func (o *Orchestrator) ReconcileOperatorMove(ctx context.Context, request OperatorMoveRequest) (OperatorMoveResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request.IssueID = strings.TrimSpace(request.IssueID)
	if request.IssueID == "" {
		return OperatorMoveResult{}, ErrMissingOperatorMoveIssueID
	}
	move := operatorMoveRequest{
		request: request,
		at:      o.clockNow().UTC(),
		reply:   make(chan OperatorMoveResult, 1),
	}
	select {
	case <-ctx.Done():
		return OperatorMoveResult{}, ctx.Err()
	case <-o.done:
		return OperatorMoveResult{}, ErrStopped
	case o.operatorMoves <- move:
	}
	select {
	case <-ctx.Done():
		return OperatorMoveResult{}, ctx.Err()
	case <-o.done:
		return OperatorMoveResult{}, ErrStopped
	case result := <-move.reply:
		return result, result.err
	}
}

func (o *Orchestrator) applyOperatorMove(ctx context.Context, state *State, request OperatorMoveRequest, at time.Time) OperatorMoveResult {
	if request.WriteTracker {
		owner := o
		ownerState := state
		if request.Tracker != nil {
			owner = &Orchestrator{cfg: o.cfg, connector: request.Tracker, laneLedger: o.laneLedger, laneCoordination: o.laneCoordination, workflowMetrics: o.workflowMetrics, logger: o.logger}
			ownerState = nil
		}
		issue := connector.Issue{ID: request.IssueID, Identifier: request.Identifier, State: request.FromState}
		if current, found := findRecoveryIssue(ownerState, issue); found {
			issue = current
		} else {
			issues, err := owner.connector.FetchIssueStatesByIDs(ctx, []string{issue.ID})
			if err != nil {
				return OperatorMoveResult{err: err}
			}
			for _, current := range issues {
				if current.ID == issue.ID {
					issue = current
					break
				}
			}
		}
		reason := request.Reason
		if reason == "" {
			reason = "operator_move"
		}
		metadata := workflowLaneMetadata{StateFieldID: request.StateFieldID, StateFieldValue: request.StateFieldValue, Provenance: request.Attribution}
		if err := owner.updateIssueStateByIDStrictWithMetadata(ctx, ownerState, issue.ID, issue, request.ToState, at, reason, metadata); err != nil {
			return OperatorMoveResult{err: err}
		}
		if request.Tracker != nil {
			return OperatorMoveResult{Reconciled: true}
		}
		if state != nil && normalizeState(issue.State) != normalizeState(request.ToState) {
			if running, ok := state.Running[issue.ID]; ok {
				applyIssueStateSnapshot(&running.Issue, request.ToState, at)
				running.CompletionLane = request.ToState
				running.CompletionAcceptedAt = at
				state.Running[issue.ID] = running
			}
		}
	}
	return o.handleOperatorMove(state, request, at)
}

func (o *Orchestrator) handleOperatorMove(state *State, request OperatorMoveRequest, at time.Time) OperatorMoveResult {
	issueID := strings.TrimSpace(request.IssueID)
	fromState := strings.TrimSpace(request.FromState)
	toState := strings.TrimSpace(request.ToState)
	if state == nil || issueID == "" || normalizeState(fromState) != normalizeState(blockedStatusState) ||
		normalizeState(toState) == normalizeState(blockedStatusState) || !o.operatorMoveTargetConfigured(toState) {
		return OperatorMoveResult{}
	}

	issue := connector.Issue{ID: issueID, Identifier: strings.TrimSpace(request.Identifier), State: fromState}
	if current, ok := findRecoveryIssue(state, issue); ok {
		issue = current
	}
	updateIssueStateSnapshots(state, issueID, issue, toState, at)
	delete(state.AutoPromoteDecisions, issueID)

	result := OperatorMoveResult{Reconciled: true}
	if blocked, ok := state.Blocked[issueID]; ok && blocked.Source == BlockedSourceProjectStatus {
		delete(state.Blocked, issueID)
		result.BlockedCleared = true
	}
	if result.BlockedCleared {
		if claimed, ok := state.Claimed[issueID]; ok {
			if _, running := state.Running[issueID]; running {
				applyIssueStateSnapshot(&claimed.Issue, toState, at)
				state.Claimed[issueID] = claimed
			} else {
				delete(state.Claimed, issueID)
				result.ClaimCleared = true
			}
		}
		if _, ok := state.Retry[issueID]; ok {
			delete(state.Retry, issueID)
			result.RetryCleared = true
		}
		if _, ok := state.InstantFailures[issueID]; ok {
			delete(state.InstantFailures, issueID)
			result.FailureMemoryCleared = true
		}
		if _, ok := state.RepeatedFailures[issueID]; ok {
			delete(state.RepeatedFailures, issueID)
			result.FailureMemoryCleared = true
		}
		if clearProjectFailureBreakerIssue(&state.FailureBreaker, issueID) {
			result.FailureMemoryCleared = true
		}
	}

	recordStateEvent(state, telemetry.ActivityEvent{
		At:      at,
		Event:   "operator_kanban_move_reconciled",
		Message: operatorMoveEventMessage(issue, fromState, toState, result),
	})
	if o.logger != nil {
		o.logger.Info(
			"operator kanban move reconciled",
			"project_id", strings.TrimSpace(request.ProjectID),
			"issue_id", issueID,
			"identifier", issue.Identifier,
			"from_state", fromState,
			"to_state", toState,
			"reason", "operator_kanban_move",
			"runtime_block_cleared", result.BlockedCleared,
			"claim_cleared", result.ClaimCleared,
			"retry_cleared", result.RetryCleared,
			"failure_memory_cleared", result.FailureMemoryCleared,
		)
	}
	return result
}

func (o *Orchestrator) operatorMoveTargetConfigured(state string) bool {
	return stateIn(state, o.cfg.ActiveStates) || stateIn(state, o.cfg.ObservedStates) || stateIn(state, o.cfg.TerminalStates)
}

func clearProjectFailureBreakerIssue(breaker *ProjectFailureBreaker, issueID string) bool {
	if breaker == nil || strings.TrimSpace(issueID) == "" {
		return false
	}
	if breaker.PreTurn {
		if breaker.CanaryIssueID == strings.TrimSpace(issueID) {
			breaker.CanaryIssueID = ""
			return true
		}
		return false
	}
	changed := false
	for class, failures := range breaker.Failures {
		kept := failures[:0]
		for _, failure := range failures {
			if strings.TrimSpace(failure.IssueID) == issueID {
				changed = true
				continue
			}
			kept = append(kept, failure)
		}
		if len(kept) == 0 {
			delete(breaker.Failures, class)
			continue
		}
		breaker.Failures[class] = kept
	}
	if strings.TrimSpace(breaker.CanaryIssueID) == issueID {
		breaker.CanaryIssueID = ""
		changed = true
	}
	if !changed {
		return false
	}
	if !breaker.Active() {
		return true
	}
	failures := breaker.Failures[breaker.Class]
	if len(failures) < normalizeFailureBreakerConfig(breaker.Config).SameClassLimit {
		breaker.Class = ""
		breaker.Count = 0
		breaker.FirstFailureAt = time.Time{}
		breaker.TrippedAt = time.Time{}
		breaker.ResumeAt = time.Time{}
		breaker.CanaryIssueID = ""
		return true
	}
	breaker.Count = len(failures)
	breaker.FirstFailureAt = failures[0].At
	breaker.TrippedAt = failures[len(failures)-1].At
	breaker.ResumeAt = breaker.TrippedAt.Add(breaker.Config.Cooldown)
	return true
}

func operatorMoveEventMessage(issue connector.Issue, fromState string, toState string, result OperatorMoveResult) string {
	message := "reconciled " + issueLabel(issue) + " after operator Kanban move from " + fromState + " to " + toState
	if result.BlockedCleared {
		message += "; cleared stale project-status runtime block"
	}
	return message
}
