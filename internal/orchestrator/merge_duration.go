package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) handleMergeWorkerDurationExceeded(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	if !errors.Is(event.Err, runpkg.ErrMergeWorkerDurationExceeded) {
		return false
	}
	if o.completeLatestTerminalMergeWorkerResult(ctx, state, event, running) {
		return true
	}

	completedAt := event.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = o.clockNow().UTC()
	}
	elapsed := completedAt.Sub(running.StartedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	progressMarker := mergeWorkerProgressMarker(running)
	if o.logger != nil {
		o.logger.Warn(
			"merge_worker_duration_exceeded",
			"issue_id", running.Issue.ID,
			"identifier", running.Issue.Identifier,
			"attempt", running.Attempt,
			"elapsed", elapsed,
			"configured_ceiling", o.cfg.MergeWorkerMaxDuration,
			"last_progress_marker", progressMarker,
			"last_progress_at", running.LastEventAt,
		)
	}

	o.recordProjectAttemptOutcome(
		state,
		event.IssueID,
		completedAt,
		store.WorkAttemptTerminalTimedOut,
		event.Err,
		workAttemptErrorMergeDuration,
		event.Err.Error(),
	)
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		completedAt,
		store.WorkAttemptTerminalTimedOut,
		workAttemptErrorMergeDuration,
		event.Err.Error(),
		"timed_out",
		"merge worker exceeded its wall-clock ceiling",
	)
	o.logMergeWorkerFailure(running.Issue, mergeWorkerDurationExceededReason, event.Err)
	o.recordMergeFailed(state, running.Issue, completedAt, mergeWorkerDurationExceededReason, event.Err)
	o.parkMergeWorkerDurationExceeded(ctx, state, running, completedAt, elapsed, progressMarker)
	return true
}

func (o *Orchestrator) parkMergeWorkerDurationExceeded(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
	elapsed time.Duration,
	progressMarker string,
) {
	issueID := strings.TrimSpace(running.Issue.ID)
	issue := cloneIssue(running.Issue)
	transitionErr := o.updateIssueStateByIDStrictWithMetadata(
		ctx,
		state,
		issueID,
		issue,
		blockedStatusState,
		completedAt,
		mergeWorkerDurationExceededReason,
		workflowLaneMetadata{},
	)
	if transitionErr != nil && o.logger != nil {
		o.logger.Error(
			"merge worker duration state transition failed",
			"issue_id", issueID,
			"identifier", issue.Identifier,
			"target_state", blockedStatusState,
			"error", transitionErr,
		)
	}

	issue.BlockerReason = mergeWorkerDurationExceededReason
	blockedAt := completedAt.UTC()
	issue.StageUpdatedAt = &blockedAt
	source := BlockedSourceMergeDuration
	if transitionErr == nil {
		issue.State = blockedStatusState
		source = BlockedSourceProjectStatus
	}
	if o.connector != nil {
		comment := mergeWorkerDurationExceededComment(
			running,
			elapsed,
			o.cfg.MergeWorkerMaxDuration,
			progressMarker,
		)
		if err := o.connector.CreateComment(ctx, issueID, comment); err != nil && o.logger != nil {
			o.logger.Warn(
				"merge worker duration comment failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"error", err,
			)
		}
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	delete(state.Completed, issueID)
	delete(state.InstantFailures, issueID)
	delete(state.RepeatedFailures, issueID)
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issueID] = Blocked{
		Issue:          issue,
		Reason:         mergeWorkerDurationExceededReason,
		RecoveryReason: string(BlockedRecoveryReasonHumanBlocker),
		RecoveryTarget: autoPromoteMergingState,
		BlockedAt:      completedAt,
		Source:         source,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   mergeWorkerDurationExceededReason,
		Message: "parked " + issueLabel(issue) + " after its merge worker exceeded the wall-clock ceiling",
	})
}

func (o *Orchestrator) reconcileMergeDurationHolds(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	byID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		byID[strings.TrimSpace(issue.ID)] = issue
	}
	transitioned := map[string]struct{}{}
	for issueID, blocked := range state.Blocked {
		if blocked.Source != BlockedSourceMergeDuration {
			continue
		}
		issue, ok := byID[issueID]
		if !ok {
			continue
		}
		if workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
			delete(state.Blocked, issueID)
			continue
		}
		if normalizeState(issue.State) != normalizeState(autoPromoteMergingState) {
			if normalizeState(issue.State) == normalizeState(blockedStatusState) {
				blocked.Issue = cloneIssue(issue)
				blocked.Issue.BlockerReason = mergeWorkerDurationExceededReason
				blocked.Source = BlockedSourceProjectStatus
				state.Blocked[issueID] = blocked
				transitioned[issueID] = struct{}{}
			} else {
				delete(state.Blocked, issueID)
			}
			continue
		}
		blocked.Issue = mergeIssueTrackerFields(blocked.Issue, issue)
		state.Blocked[issueID] = blocked
		if err := o.updateIssueStateByIDStrictWithMetadata(
			ctx,
			state,
			issueID,
			issue,
			blockedStatusState,
			now,
			mergeWorkerDurationExceededReason,
			workflowLaneMetadata{},
		); err != nil {
			if o.logger != nil {
				o.logger.Warn(
					"merge worker duration state transition retry failed",
					"issue_id", issueID,
					"identifier", issue.Identifier,
					"target_state", blockedStatusState,
					"error", err,
				)
			}
			continue
		}
		blocked.Issue = cloneIssue(issue)
		blocked.Issue.State = blockedStatusState
		blocked.Issue.BlockerReason = mergeWorkerDurationExceededReason
		blocked.Source = BlockedSourceProjectStatus
		state.Blocked[issueID] = blocked
		transitioned[issueID] = struct{}{}
	}
	return transitioned
}

func mergeWorkerDurationExceededComment(
	running Running,
	elapsed time.Duration,
	ceiling time.Duration,
	progressMarker string,
) string {
	var body strings.Builder
	body.WriteString("Detent cancelled this merge worker after it exceeded the configured hard wall-clock ceiling and parked the issue in Blocked for human attention.")
	body.WriteString("\n\n- reason: ")
	body.WriteString(mergeWorkerDurationExceededReason)
	body.WriteString("\n- elapsed: ")
	body.WriteString(elapsed.String())
	body.WriteString("\n- configured_ceiling: ")
	body.WriteString(ceiling.String())
	body.WriteString("\n- last_progress_marker: ")
	body.WriteString(progressMarker)
	if !running.LastEventAt.IsZero() {
		body.WriteString("\n- last_progress_at: ")
		body.WriteString(running.LastEventAt.UTC().Format(time.RFC3339))
	}
	if running.Attempt > 0 {
		body.WriteString("\n- attempt: ")
		body.WriteString(strconv.Itoa(running.Attempt))
	}
	body.WriteString("\n\nInspect the worker and pull request state, then move the issue back to Merging when it is safe to retry.")
	return body.String()
}

func mergeWorkerProgressMarker(running Running) string {
	if marker := strings.TrimSpace(running.LastEvent); marker != "" {
		return marker
	}
	if running.TurnCount > 0 {
		return fmt.Sprintf("turn_%d", running.TurnCount)
	}
	if strings.TrimSpace(running.SessionID) != "" {
		return "session_started"
	}
	return "none"
}
