package runner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// checkpointRefInterval bounds how much changing work a dead runner can lose.
const checkpointRefInterval = 5 * time.Minute

// checkpointRefs publishes a run's work to the item's Detent-owned git ref
// (INV-17) and remembers the newest pushed checkpoint so every Hub checkpoint
// names it. A failed push is logged and retried at the next trigger.
type checkpointRefs struct {
	publisher workspace.CheckpointRefPublisher
	execution Execution
	info      workspace.Info
	issue     workspace.Issue

	mu   sync.Mutex
	last workspace.CheckpointRef

	nudge chan struct{}
	stop  func()
}

// nativeRestorableCheckpoint returns the newest git checkpoint unless a later
// attempt published a Change version that supersedes it.
func nativeRestorableCheckpoint(recovery tracker.NativeRecovery, current *workspace.ChangeSource) *workspace.CheckpointRef {
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

func newCheckpointRefs(backend workspace.Backend, execution Execution, info workspace.Info, issue workspace.Issue, state *workspace.RecoveryState) *checkpointRefs {
	publisher, ok := backend.(workspace.CheckpointRefPublisher)
	if !ok || execution == nil {
		return nil
	}
	c := &checkpointRefs{publisher: publisher, execution: execution, info: info, issue: issue, nudge: make(chan struct{}, 1), stop: func() {}}
	if issue.Checkpoint != nil {
		c.last = *issue.Checkpoint
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
	last := c.last
	c.mu.Unlock()
	if last.CommitSHA == "" {
		return
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
	if err != nil {
		r.logWorkerEventLevel(slog.LevelWarn, req.Issue, "worker_checkpoint_ref_failed", "reason", reason, "error", err)
		return false
	}
	if pushed {
		c.last = next
		r.logWorkerEvent(req.Issue, "worker_checkpoint_ref_pushed", "reason", reason, "ref", next.Ref, "commit_sha", next.CommitSHA, "head_sha", next.HeadSHA)
	}
	return pushed
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
			if err := c.execution.Checkpoint(loopCtx, checkpoint); err != nil && loopCtx.Err() == nil {
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
