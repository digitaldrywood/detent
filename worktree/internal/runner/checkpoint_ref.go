package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// checkpointRefInterval bounds how much changing work a dead runner can lose.
const checkpointRefInterval = 5 * time.Minute

const (
	checkpointRefFinalAttempts = 3
	checkpointRefRetryDelay    = 500 * time.Millisecond
)

// checkpointRefs publishes a run's work to the item's Detent-owned git ref
// (INV-17) and remembers the newest pushed checkpoint so every Hub checkpoint
// names it. A failed push is logged and retried at the next trigger.
type checkpointRefs struct {
	backend   workspace.Backend
	publisher workspace.CheckpointRefPublisher
	execution Execution
	info      workspace.Info
	issue     workspace.Issue

	mu       sync.Mutex
	last     workspace.CheckpointRef
	failed   bool
	saved    *tracker.NativeCheckpoint
	captured *tracker.NativeCheckpoint

	nudge chan struct{}
	stop  func()
}

// nativeRestorableCheckpoint returns the newest git checkpoint unless a later
// attempt published a Change version that supersedes it.
func nativeRestorableCheckpoint(recovery tracker.NativeRecovery, current *workspace.ChangeSource) *workspace.CheckpointRef {
	if recovery.SourceVersionID != "" {
		return nil
	}
	previous := recovery.SourceAttempt()
	if previous == nil || !previous.Checkpoint.GitRef() || previous.Checkpoint.UncertainForgeEffect() {
		return nil
	}
	if current != nil && current.Version.AttemptID != "" && current.Version.AttemptID != previous.AttemptID {
		source, version := -1, -1
		for i, attempt := range recovery.Attempts {
			switch attempt.AttemptID {
			case previous.AttemptID:
				source = i
			case current.Version.AttemptID:
				version = i
			}
		}
		if version > source {
			return nil
		}
	}
	checkpoint := previous.Checkpoint
	return &workspace.CheckpointRef{Ref: checkpoint.Ref, CommitSHA: checkpoint.CommitSHA, BaseSHA: checkpoint.BaseSHA, TreeSHA: checkpoint.TreeSHA}
}

// checkpointRefCoversChangeSource lets a non-landing run restore the newest
// git checkpoint when the Hub cannot return the Change source, instead of
// refusing work that the project repository still holds.
func checkpointRefCoversChangeSource(err error, recovery tracker.NativeRecovery, landing, fresh bool) bool {
	return !landing && !fresh && errors.Is(err, ErrNativeRecoveryRequired) && nativeRestorableCheckpoint(recovery, nil) != nil
}

func newCheckpointRefs(backend workspace.Backend, execution Execution, info workspace.Info, issue workspace.Issue, state *workspace.RecoveryState) *checkpointRefs {
	publisher, ok := backend.(workspace.CheckpointRefPublisher)
	if !ok || execution == nil {
		return nil
	}
	c := &checkpointRefs{backend: backend, publisher: publisher, execution: execution, info: info, issue: issue, nudge: make(chan struct{}, 1), stop: func() {}}
	if issue.Checkpoint != nil {
		c.last = *issue.Checkpoint
		if previous := execution.Recovery().SourceAttempt(); previous != nil && previous.Checkpoint.GitRef() {
			saved := *previous.Checkpoint
			if saved.Ref == c.last.Ref && saved.CommitSHA == c.last.CommitSHA && saved.BaseSHA == c.last.BaseSHA && saved.TreeSHA == c.last.TreeSHA {
				c.saved = &saved
			}
		}
		if state != nil {
			c.last.HeadSHA = state.HeadSHA
		}
	}
	return c
}

func (c *checkpointRefs) apply(checkpoint *tracker.NativeCheckpoint) {
	if c == nil || checkpoint.WorktreeState == "clean" || checkpoint.UncertainForgeEffect() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed && c.saved != nil {
		checkpoint.HeadSHA, checkpoint.WorkspaceDigest, checkpoint.WorktreeState = c.saved.HeadSHA, c.saved.WorkspaceDigest, c.saved.WorktreeState
		checkpoint.Storage, checkpoint.Availability = c.saved.Storage, c.saved.Availability
		checkpoint.Ref, checkpoint.CommitSHA, checkpoint.BaseSHA, checkpoint.TreeSHA = c.saved.Ref, c.saved.CommitSHA, c.saved.BaseSHA, c.saved.TreeSHA
		return
	}
	last := c.last
	if last.CommitSHA == "" {
		return
	}
	if c.captured != nil {
		checkpoint.HeadSHA, checkpoint.WorkspaceDigest, checkpoint.WorktreeState = c.captured.HeadSHA, c.captured.WorkspaceDigest, c.captured.WorktreeState
	}
	checkpoint.Storage, checkpoint.Availability = "git_ref", "available"
	checkpoint.Ref, checkpoint.CommitSHA, checkpoint.BaseSHA, checkpoint.TreeSHA = last.Ref, last.CommitSHA, last.BaseSHA, last.TreeSHA
}

func (r *Runner) pushCheckpointRef(ctx context.Context, c *checkpointRefs, req RunRequest, reason string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointTimeout)
	defer cancel()
	next, pushed, err := c.publisher.PushCheckpointRef(pushCtx, c.info, c.issue, c.last, c.execution.Validate)
	c.failed = err != nil
	if err != nil {
		r.logWorkerEventLevel(slog.LevelWarn, req.Issue, "worker_checkpoint_ref_failed", "reason", reason, "error", err)
		return false
	}
	if pushed {
		c.last = next
		state := r.workspaceRecoveryState(c.backend, pushCtx, c.info, c.issue, "checkpoint_ref")
		if state != nil {
			checkpoint := executionCheckpoint(state)
			c.captured = &checkpoint
		}
		r.logWorkerEvent(req.Issue, "worker_checkpoint_ref_pushed", "reason", reason, "ref", next.Ref, "commit_sha", next.CommitSHA, "head_sha", next.HeadSHA)
	}
	return pushed
}

// finishCheckpointRef retries the stage-end push so a transient failure does
// not leave the newest work only on this machine.
func (r *Runner) finishCheckpointRef(ctx context.Context, c *checkpointRefs, req RunRequest) {
	for attempt := range checkpointRefFinalAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * checkpointRefRetryDelay)
		}
		r.pushCheckpointRef(ctx, c, req, "stage_finished")
		if c == nil || !c.lastPushFailed() {
			return
		}
	}
}

func (c *checkpointRefs) lastPushFailed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}

func (c *checkpointRefs) requestPush() {
	if c == nil {
		return
	}
	select {
	case c.nudge <- struct{}{}:
	default:
	}
}

// start pushes after worktree-changing turns and on the interval floor until
// stopped. The final push belongs to afterExecution.
func (r *Runner) startCheckpointRefs(ctx context.Context, c *checkpointRefs, req RunRequest, backend workspace.Backend) {
	if c == nil {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		ticker := time.NewTicker(checkpointRefInterval)
		defer ticker.Stop()
		for {
			reason := "periodic"
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			case <-c.nudge:
				reason = "turn_completed"
			}
			if !r.pushCheckpointRef(loopCtx, c, req, reason) {
				continue
			}
			state := r.workspaceRecoveryState(backend, loopCtx, c.info, c.issue, "checkpoint_ref")
			checkpoint := executionCheckpointPreservingPublication(c.execution, state)
			c.apply(&checkpoint)
			if err := recordCheckpointRef(loopCtx, c, c.execution, checkpoint); err != nil && loopCtx.Err() == nil {
				r.logWorkerEventLevel(slog.LevelWarn, req.Issue, "worker_checkpoint_ref_unrecorded", "error", err)
			}
		}
	}()
}

func (c *checkpointRefs) halt() {
	if c != nil {
		c.stop()
	}
}

func sameMachineWorkspaceRetained(backend workspace.Backend, recovery tracker.NativeRecovery, issue workspace.Issue) bool {
	previous := recovery.SourceAttempt()
	if previous == nil || previous.MachineID == "" || previous.MachineID != recovery.Lease.MachineID {
		return false
	}
	local, ok := backend.(interface {
		Existing(workspace.Issue) (workspace.Info, error)
	})
	if !ok {
		return false
	}
	_, err := local.Existing(issue)
	return err == nil
}

func recordCheckpointRef(ctx context.Context, c *checkpointRefs, execution Execution, checkpoint tracker.NativeCheckpoint) error {
	err := execution.Checkpoint(ctx, checkpoint)
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.failed = true
		} else if checkpoint.GitRef() {
			saved := checkpoint
			c.saved = &saved
		}
	}
	return err
}
