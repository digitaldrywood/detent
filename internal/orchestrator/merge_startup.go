package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func newMergeWorkerStartupTimer(duration time.Duration, expire func()) mergeWorkerStartupTimer {
	return time.AfterFunc(duration, expire)
}

func (o *Orchestrator) handleMergeWorkerStartupTimeout(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	if !errors.Is(event.Err, runpkg.ErrMergeWorkerStartupTimeout) {
		return false
	}

	completedAt := event.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = o.clockNow().UTC()
	}
	timeout := o.cfg.MergeWorkerStartupTimeout
	err := fmt.Errorf(
		"merge worker did not report startup progress within %s: %w",
		timeout,
		runpkg.ErrMergeWorkerStartupTimeout,
	)
	o.logMergeWorkerFailure(running.Issue, "runner_startup_timeout", err)
	o.recordMergeFailed(state, running.Issue, completedAt, "runner_startup_timeout", err)
	o.recordProjectAttemptOutcome(
		state,
		event.IssueID,
		completedAt,
		store.WorkAttemptTerminalFailure,
		err,
		"runner_startup_timeout",
		err.Error(),
	)
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		completedAt,
		store.WorkAttemptTerminalFailure,
		"runner_startup_timeout",
		err.Error(),
		"starting",
		"merge worker startup timed out",
	)
	attempt := nextAttempt(running.Attempt)
	if attempt > maxMergeWorkerRunnerFailures &&
		o.blockExhaustedMergeWorker(ctx, state, running, completedAt, attempt, err) {
		return true
	}
	o.scheduleRetry(
		state,
		running.Issue,
		attempt,
		completedAt,
		err.Error(),
		false,
		running.WorkerHost,
	)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   "merge_worker_startup_timeout",
		Message: "merge worker startup timed out for " + issueLabel(running.Issue) + ": " + err.Error(),
	})
	return true
}
