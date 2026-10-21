package orchestrator

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func (o *Orchestrator) checkpointValidator(issueID string, attemptID int64, generation uint64) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		runtime := o.latestRuntimeState.Load()
		if runtime == nil {
			return runner.ErrExecutionAuthorityUnavailable
		}
		running, ok := runtime.Running[issueID]
		if !ok || running.WorkAttemptID != attemptID || running.Generation != generation || running.Mode != runner.RunModeImplement {
			return runner.ErrExecutionAuthorityUnavailable
		}
		if o.workAttempts == nil || o.connector == nil {
			return runner.ErrExecutionAuthorityUnavailable
		}
		attempt, err := o.workAttempts.WorkAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if attempt.IssueID != issueID || attempt.Status != store.WorkAttemptStatusActive {
			return runner.ErrExecutionAuthorityUnavailable
		}
		issue, err := o.refreshCompletionLane(ctx, running)
		if err != nil {
			return err
		}
		if normalizeState(issue.State) != normalizeState(running.Issue.State) {
			return fmt.Errorf("%w: checkpoint lane changed", runner.ErrExecutionAuthorityUnavailable)
		}
		current := o.latestRuntimeState.Load()
		if current == nil {
			return runner.ErrExecutionAuthorityUnavailable
		}
		owned, ok := current.Running[issueID]
		if !ok || owned.WorkAttemptID != attemptID || owned.Generation != generation || owned.Mode != runner.RunModeImplement {
			return runner.ErrExecutionAuthorityUnavailable
		}
		if !attempt.LeaseExpiresAt.After(o.clockNow()) {
			// Runtime ownership and the current lane, not a missed heartbeat, decide
			// whether this live worker may checkpoint. Renew through the heartbeat
			// writer, which only updates active attempts.
			if owned.progress != nil {
				owned.progress.mu.Lock()
				defer owned.progress.mu.Unlock()
				if owned.progress.closed {
					return runner.ErrExecutionAuthorityUnavailable
				}
			}
			heartbeat := o.runningWorkAttemptHeartbeat(nil, owned.withProgress(), o.clockNow())
			if err := recordWorkAttemptHeartbeat(ctx, o.workAttempts, heartbeat); err != nil {
				return err
			}
			if owned.progress != nil {
				owned.progress.persisted.Store(&heartbeat)
			}
			// Ownership can change while storage is blocked.
			current = o.latestRuntimeState.Load()
			if current == nil {
				return runner.ErrExecutionAuthorityUnavailable
			}
			latest, ok := current.Running[issueID]
			if !ok || latest.WorkAttemptID != attemptID || latest.Generation != generation || latest.Mode != runner.RunModeImplement {
				return runner.ErrExecutionAuthorityUnavailable
			}
		}
		return ctx.Err()
	}
}
