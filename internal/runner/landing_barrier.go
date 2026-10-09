package runner

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type LandingBarrierOwner interface {
	NextLandingBarrier(context.Context, string, string, string, bool, func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error)
	FinishLandingBarrier(context.Context, string, tracker.LandingBarrier, *gate.CommandResult) error
}

func (r *Runner) RunLandingBarriers(ctx context.Context, owner LandingBarrierOwner) {
	backend, ok := r.workspace.(workspace.LandingBarrierWorkspace)
	if !ok {
		return
	}
	var release func() error
	defer func() {
		if release != nil {
			if err := release(); err != nil {
				r.logger.Warn("landing barrier runner release failed", "error", err)
			}
		}
	}()
	recoverClaim := true
	repaired := map[string]bool{}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workflow, _, _, _ := r.runtimeSnapshot()
		cfg := gate.Effective(workflow.Config.Gate)
		if cfg.LandingMode == gate.LandingRollingBarrier {
			if release == nil {
				var err error
				release, err = backend.AcquireLandingBarrierRunner(ctx)
				if err != nil {
					r.logger.Warn("landing barrier runner unavailable", "error", err)
					return
				}
			}
			descriptor, err := config.ResolvePolicy(workflow)
			if err == nil {
				barrier, started, claimErr := owner.NextLandingBarrier(ctx, r.projectID, backend.LandingRepository(ctx), descriptor.ID, recoverClaim, backend.LandingBarrierHead)
				if claimErr == nil {
					recoverClaim = false
				}
				if claimErr != nil {
					r.logger.Warn("landing barrier claim failed", "error", claimErr)
				}
				if started {
					result, runErr := backend.RunLandingBarrier(ctx, barrier.ID, barrier.BaseRef, cfg.Run)
					var completed *gate.CommandResult
					if runErr == nil {
						completed = &result
					} else {
						r.logger.Warn("landing barrier instance failure", "error", runErr)
					}
					if !r.finishLandingBarrier(ctx, owner, barrier, completed) {
						return
					}
					if completed != nil && completed.ExitCode != 0 && !repaired[completed.HeadSHA] {
						repaired[completed.HeadSHA] = true
						r.repairLandingBarrier(ctx, backend, barrier, *completed)
						continue
					}
					if runErr == nil {
						continue
					}
				}
			} else {
				r.logger.Warn("landing barrier policy unavailable", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) finishLandingBarrier(ctx context.Context, owner LandingBarrierOwner, barrier tracker.LandingBarrier, result *gate.CommandResult) bool {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		reportCtx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			reportCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
		}
		err := owner.FinishLandingBarrier(reportCtx, r.projectID, barrier, result)
		if err == nil {
			return ctx.Err() == nil
		}
		r.logger.Warn("landing barrier publication failed", "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

const (
	barrierRepairDuration    = 45 * time.Minute
	barrierRepairOutputBytes = 24 << 10
)

func (r *Runner) repairLandingBarrier(ctx context.Context, backend workspace.LandingBarrierWorkspace, barrier tracker.LandingBarrier, result gate.CommandResult) {
	repairer, ok := backend.(workspace.LandingBarrierRepairWorkspace)
	if !ok {
		return
	}
	_, runtime, _, _ := r.runtimeSnapshot()
	selection, err := runtime.router.Route(connector.Issue{}, selector.Context{})
	if err != nil {
		r.logger.Warn("landing barrier repair unavailable", "error", err)
		return
	}
	agent, ok := runtime.backends[selection.BackendID]
	if !ok {
		r.logger.Warn("landing barrier repair unavailable", "error", fmt.Errorf("%w: %s", ErrMissingAgentBackend, selection.BackendID))
		return
	}
	path, head, release, err := repairer.PrepareLandingBarrierRepair(ctx, barrier.ID, barrier.BaseRef)
	if err != nil {
		r.logger.Warn("landing barrier repair preparation failed", "error", err)
		return
	}
	defer func() {
		if err := release(); err != nil {
			r.logger.Warn("landing barrier repair cleanup failed", "error", err)
		}
	}()
	if head != result.HeadSHA {
		r.logger.Info("landing barrier repair skipped; base moved", "red_head", result.HeadSHA, "head", head)
		return
	}
	scratch, err := os.MkdirTemp("", "detent-barrier-repair-")
	if err != nil {
		r.logger.Warn("landing barrier repair preparation failed", "error", err)
		return
	}
	defer os.RemoveAll(scratch)
	turnCtx, cancel := context.WithTimeout(ctx, barrierRepairDuration)
	defer cancel()
	r.logger.Info("landing barrier repair started", "head", head, "backend", selection.BackendID)
	if _, err := agent.RunTurn(turnCtx, AgentTurnRequest{Workspace: path, TempDir: scratch, Prompt: barrierRepairPrompt(result), MaxDuration: barrierRepairDuration}, nil); err != nil {
		r.logger.Warn("landing barrier repair turn failed", "head", head, "error", err)
	}
	published, err := repairer.PublishLandingBarrierRepair(ctx, path, barrier.BaseRef, head)
	switch {
	case err != nil:
		r.logger.Warn("landing barrier repair publication failed", "head", head, "error", err)
	case published == "":
		r.logger.Info("landing barrier repair made no change", "head", head)
	default:
		r.logger.Info("landing barrier repair published", "head", head, "repair", published)
	}
}

func barrierRepairPrompt(result gate.CommandResult) string {
	output := result.Output
	if len(output) > barrierRepairOutputBytes {
		output = output[len(output)-barrierRepairOutputBytes:]
	}
	return fmt.Sprintf(`The integration barrier command failed on commit %s of this repository.

Command: %s
Exit code: %d

Fix the cause of the failures with the smallest correct change to this checkout. Re-run the failing checks to confirm they pass. Commit the fix with a conventional commit message. Do not push, do not change unrelated code, and do not weaken or skip a check to make it pass. If the failure comes from the environment rather than the code, make no commit.

Failing output (tail):
%s`, result.HeadSHA, result.Command, result.ExitCode, output)
}
