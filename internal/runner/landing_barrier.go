package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type LandingBarrierOwner interface {
	NextLandingBarrier(context.Context, string, string, string, bool, func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error)
	FinishLandingBarrier(context.Context, string, tracker.LandingBarrier, *gate.CommandResult) error
}

type landingBarrierCurrency interface {
	LandingBarrierCurrent(context.Context, string, string, string) bool
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
	repairEvidence := gate.PipelineEvidence{}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workflow, _, _, _ := r.runtimeSnapshot()
		cfg := gate.Effective(workflow.Config.Gate)
		if cfg.LandingMode == gate.LandingRollingBarrier && release == nil {
			acquired, err := backend.AcquireLandingBarrierRunner(ctx)
			switch {
			case errors.Is(err, instancelock.ErrHeld):
				r.logger.Info("landing barrier runner held elsewhere; retrying", "error", err)
			case err != nil:
				r.logger.Warn("landing barrier runner unavailable", "error", err)
				return
			default:
				release = acquired
			}
		}
		if cfg.LandingMode == gate.LandingRollingBarrier && release != nil {
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
					if timing := result.PipelineTiming("barrier"); timing.Valid() {
						r.logger.Info("pipeline timing", "barrier", barrier.ID, "timing", timing)
					}
					var completed *gate.CommandResult
					if runErr == nil {
						if !repairEvidence.IsZero() {
							pipeline := gate.PipelineEvidence{}
							if result.Pipeline != nil {
								pipeline = *result.Pipeline
							}
							pipeline.Timings, pipeline.Dropped = gate.MergePipeline(pipeline.Timings, repairEvidence.Timings, max(pipeline.Dropped, repairEvidence.Dropped))
							result.Pipeline = &pipeline
						}
						completed = &result
					} else {
						r.logger.Warn("landing barrier instance failure", "error", runErr)
					}
					if !r.finishLandingBarrier(ctx, owner, barrier, completed) {
						return
					}
					if completed != nil {
						repairEvidence = gate.PipelineEvidence{}
					}
					if completed != nil && completed.ExitCode != 0 && !repaired[completed.HeadSHA] {
						repaired[completed.HeadSHA] = true
						current := func() bool { return true }
						if currency, ok := owner.(landingBarrierCurrency); ok {
							current = func() bool { return currency.LandingBarrierCurrent(ctx, r.projectID, barrier.Repository, barrier.ID) }
						}
						repairCtx := gate.WithPipelineRecorder(ctx, "repair", func(timing gate.PipelineTiming) {
							r.logger.Info("pipeline timing", "barrier", barrier.ID, "timing", timing)
							repairEvidence.Timings, repairEvidence.Dropped = gate.MergePipeline(repairEvidence.Timings, []gate.PipelineTiming{timing}, repairEvidence.Dropped)
						})
						r.repairLandingBarrier(repairCtx, backend, barrier, *completed, cfg.Run, current)
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
	barrierFailedPrefix      = "detent-barrier-failed:"
)

func (r *Runner) repairLandingBarrier(ctx context.Context, backend workspace.LandingBarrierWorkspace, barrier tracker.LandingBarrier, result gate.CommandResult, command string, current func() bool) {
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
	failed := barrierFailures(result.Output)
	if head != result.HeadSHA {
		current, _, err := repairer.VerifyLandingBarrierRepair(ctx, path, "", command, failed)
		if err != nil {
			r.logger.Warn("landing barrier repair verification failed", "head", head, "error", err)
			return
		}
		if current.ExitCode == 0 {
			r.logger.Info("landing barrier failures already pass on the current head", "red_head", result.HeadSHA, "head", head)
			return
		}
	}
	scratch, err := os.MkdirTemp("", "detent-barrier-repair-")
	if err != nil {
		r.logger.Warn("landing barrier repair preparation failed", "error", err)
		return
	}
	defer func() {
		if err := os.RemoveAll(scratch); err != nil {
			r.logger.Warn("landing barrier repair scratch cleanup failed", "error", err)
		}
	}()
	superseded := func() bool {
		if current() {
			return false
		}
		r.logger.Info("landing barrier repair superseded by a newer barrier", "head", head, "barrier", barrier.ID)
		return true
	}
	if superseded() {
		return
	}
	culprit := r.landingBarrierCulprit(ctx, repairer, path, barrier.GreenHead, head, command, failed, superseded)
	if superseded() {
		return
	}
	turnCtx, cancel := context.WithTimeout(ctx, barrierRepairDuration)
	defer cancel()
	r.logger.Info("landing barrier repair started", "head", head, "culprit", culprit, "backend", selection.BackendID)
	if _, err := agent.RunTurn(turnCtx, AgentTurnRequest{Workspace: path, TempDir: scratch, Prompt: barrierRepairPrompt(result, failed, culprit), MaxDuration: barrierRepairDuration}, nil); err != nil {
		r.logger.Warn("landing barrier repair turn failed", "head", head, "error", err)
	}
	if r.publishVerifiedBarrierRepair(ctx, repairer, path, barrier.BaseRef, head, command, failed, "repair") || culprit == "" || superseded() {
		return
	}
	if err := repairer.RevertLandingBarrierCommit(ctx, path, head, culprit); err != nil {
		r.logger.Warn("landing barrier culprit revert failed", "head", head, "culprit", culprit, "error", err)
		return
	}
	r.publishVerifiedBarrierRepair(ctx, repairer, path, barrier.BaseRef, head, command, failed, "revert")
}

func (r *Runner) publishVerifiedBarrierRepair(ctx context.Context, repairer workspace.LandingBarrierRepairWorkspace, path, base, head, command string, failed []string, kind string) bool {
	verified, changed, err := repairer.VerifyLandingBarrierRepair(ctx, path, head, command, failed)
	switch {
	case err != nil:
		r.logger.Warn("landing barrier "+kind+" verification failed", "head", head, "error", err)
		return false
	case !changed:
		r.logger.Info("landing barrier "+kind+" made no change", "head", head)
		return false
	case verified.ExitCode != 0:
		r.logger.Warn("landing barrier "+kind+" did not fix the failing checks", "head", head, "exit_code", verified.ExitCode, "failed", failed)
		return false
	}
	published, err := repairer.PublishLandingBarrierRepair(ctx, path, base, head)
	if err != nil {
		r.logger.Warn("landing barrier "+kind+" publication failed", "head", head, "error", err)
		return false
	}
	r.logger.Info("landing barrier "+kind+" published", "head", head, kind, published)
	return published != ""
}

func (r *Runner) landingBarrierCulprit(ctx context.Context, repairer workspace.LandingBarrierRepairWorkspace, path, green, head, command string, failed []string, superseded func() bool) string {
	if green == "" || len(failed) == 0 {
		return ""
	}
	commits, err := repairer.LandingBarrierCommits(ctx, path, green, head)
	if err != nil || len(commits) == 0 {
		r.logger.Warn("landing barrier culprit search unavailable", "green", green, "head", head, "error", err)
		return ""
	}
	defer func() {
		if err := repairer.CheckoutLandingBarrierCommit(ctx, path, head); err != nil {
			r.logger.Warn("landing barrier culprit search cleanup failed", "head", head, "error", err)
		}
	}()
	low, high := 0, len(commits)-1
	for low < high {
		if superseded() {
			return ""
		}
		mid := (low + high) / 2
		if err := repairer.CheckoutLandingBarrierCommit(ctx, path, commits[mid]); err != nil {
			r.logger.Warn("landing barrier culprit search failed", "commit", commits[mid], "error", err)
			return ""
		}
		checked, _, err := repairer.VerifyLandingBarrierRepair(ctx, path, "", command, failed)
		if err != nil {
			r.logger.Warn("landing barrier culprit search failed", "commit", commits[mid], "error", err)
			return ""
		}
		if checked.ExitCode != 0 {
			high = mid
		} else {
			low = mid + 1
		}
	}
	r.logger.Info("landing barrier culprit found", "culprit", commits[low], "green", green, "head", head, "searched", len(commits))
	return commits[low]
}

func barrierFailures(output string) []string {
	var failed []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, barrierFailedPrefix) && len(strings.Fields(line)) > 2 {
			failed = append(failed, line)
		}
	}
	return failed
}

func barrierRepairPrompt(result gate.CommandResult, failed []string, culprit string) string {
	output := result.Output
	if len(output) > barrierRepairOutputBytes {
		output = output[len(output)-barrierRepairOutputBytes:]
	}
	rerun := "Re-run the failing checks to confirm they pass."
	if len(failed) > 0 {
		rerun = fmt.Sprintf("Only these checks failed:\n%s\n\nWhile iterating, re-run only them with: %s='<the lines above>' %s\nDo not run the full command; the runner re-runs exactly these failures before publishing your commit.", strings.Join(failed, "\n"), workspace.LandingBarrierFailedEnv, result.Command)
	}
	if culprit != "" {
		rerun += fmt.Sprintf("\n\nThe failures first appear in commit %s; inspect it with git show %s. Fix forward on the current checkout; if no correct fix exists, make no commit and the runner reverts that commit instead.", culprit, culprit)
	}
	return fmt.Sprintf(`The integration barrier command failed on commit %s of this repository.

Command: %s
Exit code: %d

Fix the cause of the failures with the smallest correct change to this checkout. %s Commit the fix with a conventional commit message. Do not push, do not change unrelated code, and do not weaken or skip a check to make it pass. If the failure comes from the environment rather than the code, make no commit.

Failing output (tail):
%s`, result.HeadSHA, result.Command, result.ExitCode, rerun, output)
}
