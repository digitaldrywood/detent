package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) resolveNativeLanding(ctx context.Context, req RunRequest, source workspace.LandingConflictResolver, info workspace.Info, issue workspace.Issue, opts workspace.LandOptions) (workspace.LandingResolution, RunResult, error) {
	var result RunResult
	resolved, err := source.ResolveLandingConflict(ctx, info, issue, opts, func(ctx context.Context, path string, files []string) (turnErr error) {
		workflow, runtime, budgetChecker, estimator := r.runtimeSnapshot()
		selection, backend, backendConfig, err := runtime.selectRequestBackend(req, selectorContext(req.SelectorContext, workflow), RoleMerge)
		if err != nil {
			return err
		}
		policy, err := r.workerGitHubPolicy(ctx, workflow.Config, req.Issue.Identifier)
		if err != nil {
			return err
		}
		environment := r.withGoBudget(ctx, path, workerServiceEnvironment(RunModeMerge, r.serviceConnection, info, issue))
		process, cleanup, err := prepareAgentProcessRequest(ctx, AgentProcessRequest{Workspace: path, Environment: environment}, policy)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer func() {
				if cleanup != nil {
					turnErr = errors.Join(turnErr, cleanup())
				}
			}()
		}
		model := effectiveModel("", selection.Model, runtime.defaultModelForRole(RoleMerge))
		selected := resolveRequestAgentSelection(ctx, req, process, model, RoleMerge, workflow.Config, backendConfig, backend)
		if selected.Err != nil {
			return selected.Err
		}
		if cleanup != nil {
			err := cleanup()
			cleanup = nil
			if err != nil {
				return err
			}
		}
		model = effectiveModel("", selected.Model, model)
		if model == "" {
			model = "provider_default"
		}
		var projection *dispatchBudgetProjection
		if workflow.Config.Budget.EffectiveBillingMode() != config.BillingModeSubscription {
			admission, refused, err := r.checkDispatchBudget(ctx, budgetChecker, estimator, req.Issue, model, r.now())
			if err != nil {
				return err
			}
			if refused {
				result = admission
				return errors.New("merge resolution budget refused")
			}
			projection = admission.budgetProjection
		}
		if req.AcquireModelPermit != nil {
			if err := req.AcquireModelPermit(ctx); err != nil {
				return err
			}
		}
		identity := configuredRuntimeIdentity(selection, backendConfig, RoleMerge, model, r.now())
		identity.Selection = selected.Selection
		if err := req.Execution.Start(ctx, tracker.NativeExecutionIdentity{Role: RoleMerge, Backend: selection.BackendID, Model: model}); err != nil {
			return err
		}
		started := r.now()
		sessionID, sessionStarted, err := r.startSession(ctx, req, started, identity, store.AgentResumeState{}, "", "")
		if err != nil {
			return err
		}
		result.RuntimeIdentity = identity
		var turnResult AgentTurnResult
		var turns int64
		defer func() {
			if turnErr != nil {
				result.FinalState = finalStateForTurnError(turnErr)
			}
			turnErr = errors.Join(turnErr, r.finishSession(ctx, sessionID, sessionStarted, req.WorkAttemptID, req.Issue, started, r.now(), result, model, backendConfig.Kind, turns, turnResult, 0))
		}()
		if observer, ok := req.Execution.(RuntimeExecution); ok {
			if err := observer.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{LocalAttemptID: req.WorkAttemptID, Generation: req.Generation, Phase: "merging", HeartbeatAt: r.now(), Identity: identity}); err != nil {
				return err
			}
		}
		limit := r.nativeWorkTimeout(workflow.Config, backendConfig, 0)
		turnCtx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		prompt := fmt.Sprintf("Resolve only the conflicting hunks of the approved Change in this isolated integration worktree. The reviewed head is %s; HEAD is the current base. Conflict files: %s. Preserve both the reviewed intent and unrelated base changes. Follow repository instructions. Regenerate generated files from combined inputs; never hand-merge generated output. Stage the resolution with git add. Do not commit, rebase, change branches or refs, push, or implement any other issue work. The runner will verify the resolved diff and run the approved gate. This is one bounded merge-resolution turn; if it cannot be completed, leave the conflict unresolved.", opts.HeadSHA, strings.Join(files, ", "))
		provider, tier, effort := agentTurnIdentityOptions(backendConfig)
		if selected.Effort != "" {
			effort = selected.Effort
		}
		turnInfo := info
		turnInfo.Path = path
		request := AgentTurnRequest{
			Workspace:           path,
			Prompt:              prompt,
			Model:               model,
			ModelProvider:       provider,
			ServiceTier:         tier,
			ReasoningEffort:     effort,
			MaxTurns:            1,
			MaxDuration:         limit,
			Environment:         environment,
			AllowLocalBinding:   workflow.Config.Worker.EffectiveAllowLocalBinding(),
			ExtraNetworkDomains: workflow.Config.Worker.ExtraNetworkDomains,
			ExtraWritableRoots:  extraWritableRootsForWorkspace(ctx, workflow.Config.Workspace.Kind, path, true, r.logger),
			MaxRSSBytes:         r.maxAgentRSSBytes,
			RSSPollInterval:     r.rssPollInterval,
			processRSS:          r.processRSS,
			workerGitHub:        policy,
		}
		execution := r.runAgentTurn(turnCtx, backend, request, req, turnInfo, issue, workflow.Config.Agent, "", nil, started, sessionID, identity, projection, 0, model, backendConfig.Kind)
		execution.err = classifyAgentCapacityError(backend, selection, backendConfig, execution.result.RuntimeIdentity, execution.err, execution.result.RateLimits, started)
		result = execution.result
		result.TokenUSD = r.usageCostUSD(effectiveModel(result.RuntimeIdentity.ResolvedModel.Value, result.Model, model), result.Tokens.InputTokens, result.Tokens.CachedInputTokens, result.Tokens.OutputTokens, backendConfig.Kind)
		if execution.err != nil {
			result.FinalState = finalStateForTurnError(execution.err)
		}
		result.TurnStarted = execution.turnStarted
		result.TurnCount = execution.turnCount
		turns, turnResult = int64(execution.turnCount), execution.turnResult
		return errors.Join(execution.err, execution.cleanupErr)
	})
	if !resolved.Gate.StartedAt.IsZero() {
		r.recordPipeline(req, resolved.Gate.PipelineTiming("landing_validation"))
	}
	return resolved, result, err
}
