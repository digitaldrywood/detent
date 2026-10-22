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
	o.parkMergeWorkerDurationExceeded(ctx, state, running, event, elapsed, progressMarker)
	return true
}

func (o *Orchestrator) parkMergeWorkerDurationExceeded(
	ctx context.Context,
	state *State,
	running Running,
	event runpkg.Completion,
	elapsed time.Duration,
	progressMarker string,
) {
	o.retryMachineCompletion(ctx, state, event, running, nextAttempt(running.Attempt), mergeWorkerDurationExceededReason)
}

func (o *Orchestrator) reconcileMergeDurationHolds(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	return nil
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
