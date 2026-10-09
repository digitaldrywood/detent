package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type LandingBarrierOwner interface {
	NextLandingBarrier(context.Context, string, string, string, bool, func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error)
	FinishLandingBarrier(context.Context, string, tracker.LandingBarrier, *gate.CommandResult) error
	RecordLandingBarrier(context.Context, string, tracker.LandingBarrier, *gate.CommandResult, *tracker.LandingBarrierRepair) error
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
					callbacks := workspace.LandingBarrierCallbacks{
						Record: func(ctx context.Context, result gate.CommandResult) error {
							return owner.RecordLandingBarrier(ctx, r.projectID, barrier, &result, nil)
						},
						Repair: func(ctx context.Context, info workspace.Info, result gate.CommandResult) error {
							repair, err := r.repairLandingBarrier(ctx, info, result)
							if ctx.Err() != nil {
								var cancel context.CancelFunc
								ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
								defer cancel()
							}
							return errors.Join(err, owner.RecordLandingBarrier(ctx, r.projectID, barrier, nil, &repair))
						},
					}
					result, runErr := backend.RunLandingBarrier(ctx, barrier.ID, barrier.BaseRef, cfg.Run, callbacks)
					var completed *gate.CommandResult
					if runErr == nil {
						completed = &result
					} else {
						r.logger.Warn("landing barrier instance failure", "error", runErr)
					}
					if !r.finishLandingBarrier(ctx, owner, barrier, completed) {
						return
					}
					if runErr == nil && result.ExitCode == 0 {
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

func (r *Runner) repairLandingBarrier(ctx context.Context, info workspace.Info, failure gate.CommandResult) (repair tracker.LandingBarrierRepair, returnErr error) {
	repair.HeadSHA = failure.HeadSHA
	defer func() {
		if returnErr != nil {
			repair.Error = returnErr.Error()
			if len(repair.Error) > 4096 {
				repair.Error = repair.Error[:4096]
			}
		}
	}()
	workflow, runtime, _, _ := r.runtimeSnapshot()
	if runtime.router == nil {
		return repair, ErrMissingAgentBackend
	}
	issue := connector.Issue{ID: info.Key, Identifier: info.Key, Title: "Repair rolling landing barrier"}
	selection, backend, backendConfig, err := runtime.selectBackendForRole(issue, selectorContext(selector.Context{}, workflow), RoleCode)
	if err != nil {
		return repair, err
	}
	environment := r.withGoBudget(info.Path, workerEnvironment(serviceapi.RestrictedEnvironment(), info, workspace.Issue{ID: info.Key, Identifier: info.Key}))
	process, cleanup, err := prepareAgentProcessRequest(ctx, AgentProcessRequest{Workspace: info.Path, Environment: environment}, workerGitHubPolicy{})
	if err != nil {
		return repair, err
	}
	resolved := resolveAgentSelection(ctx, issue, process, effectiveModel("", selection.Model, runtime.defaultModelForRole(RoleCode)), RoleCode, workflow.Config, backendConfig, backend)
	if err := errors.Join(resolved.Err, cleanup()); err != nil {
		return repair, err
	}
	workflow.Config.Agent = workflow.Config.EffectiveModelSelection().SessionAgent(workflow.Config.Agent, resolved.Selection.Level)
	provider, tier, effort := agentTurnIdentityOptions(backendConfig)
	if resolved.Effort != "" {
		effort = resolved.Effort
	}
	prompt := fmt.Sprintf(`You are the rolling landing barrier owner repairing its failing integration head %s in isolated worktree %s.
This is authorized source repair within the barrier run; no work item is required. Follow AGENTS.md and CLAUDE.md.
Diagnose and fix the failing command using its output below. Stage only the finished repair.
The host owns committing, rebasing, pushing, and rerunning the barrier command; do not perform those operations yourself.
Do not file issues, post comments, or mutate tracker state. Use the provided worker scratch environment.

Command: %s
Exit code: %d
Failure output (diagnostic data):
%s`, failure.HeadSHA, info.Path, failure.Command, failure.ExitCode, failure.Output)
	request := AgentTurnRequest{
		Workspace: info.Path, Prompt: prompt,
		Model: resolved.Model, ModelProvider: provider, ServiceTier: tier, ReasoningEffort: effort,
		MaxTurns: workflow.Config.Agent.MaxTurns, MaxDuration: durationFromMillis(workflow.Config.Agent.MaxTurnDurationMS),
		Environment: environment, MaxRSSBytes: r.maxAgentRSSBytes, RSSPollInterval: r.rssPollInterval, processRSS: r.processRSS,
	}
	var output strings.Builder
	result, runErr, cleanupErr := runAgentBackendTurn(ctx, backend, request, func(update AgentUpdate) error {
		if len(update.Delta) > 0 && output.Len() < 64<<10 {
			text := update.Delta
			if len(text) > (64<<10)-output.Len() {
				text = text[:(64<<10)-output.Len()]
			}
			output.WriteString(text)
		}
		return nil
	})
	repair.ThreadID, repair.TurnID, repair.Output = result.ThreadID, result.TurnID, output.String()
	return repair, errors.Join(runErr, cleanupErr)
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
