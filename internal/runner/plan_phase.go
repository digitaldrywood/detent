package runner

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner/planphase"
	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) runPlanPhase(ctx context.Context, req RunRequest, workflow config.Workflow, runtime agentRuntime, backend workspace.Backend, info workspace.Info, issue workspace.Issue, checker BudgetChecker, estimator DispatchEstimator) (string, string, RunResult, error) {
	if !workflow.Config.Plan.Enabled {
		return "", "", RunResult{}, errors.New("native plan state requires planning to be enabled")
	}
	execution, ok := req.Execution.(planphase.Execution)
	if !ok {
		return "", "", RunResult{}, errors.New("native planning requires phase and step receipts")
	}
	planConfig := gate.EffectivePlan(workflow.Config.Plan)
	review := planConfig.Review
	var admission RunResult
	plan, wait, err := planphase.Run(ctx, planphase.Request{
		Execution: execution, Continuation: req.phaseContinuation, Backend: backend, Info: info, Issue: issue,
		PublishedSource: req.publishedPhaseSource,
		Description:     req.Issue.Description, HumanReview: review != gate.PlanReviewAutomated, AutomatedReview: review != gate.PlanReviewHuman, Now: r.now,
		Prompt: func() (string, error) {
			return buildRunPrompt(workflow, &req, RunModePlan, PromptOptions{PlanOnly: true, WorkspacePath: info.Path, Branch: info.Branch}, workspace.MergePrepareResult{})
		},
		OpenAgent: func() (func(context.Context, string) (string, error), error) {
			var generate func(context.Context, string) (string, error)
			var err error
			generate, admission, err = r.planPhaseAgent(ctx, req, workflow, runtime, info, checker, estimator)
			return generate, err
		},
	})
	return plan, wait, admission, err
}

func (req RunRequest) setPhaseProvider(ctx context.Context, requirement providercapacity.Requirement) error {
	execution, ok := req.Execution.(phasedProviderExecution)
	if !ok {
		return ErrExecutionAuthorityUnavailable
	}
	return execution.SetPhaseProvider(ctx, requirement)
}

func (r *Runner) planPhaseAgent(ctx context.Context, req RunRequest, workflow config.Workflow, runtime agentRuntime, info workspace.Info, checker BudgetChecker, estimator DispatchEstimator) (func(context.Context, string) (string, error), RunResult, error) {
	req.Mode, req.ResumeState = RunModePlan, store.AgentResumeState{}
	selection, backend, provider, err := runtime.selectRequestBackend(req, runworkspace.SelectorContext(req.SelectorContext, workflow), RolePlan)
	if err != nil {
		return nil, RunResult{}, err
	}
	environment := procgroup.Environment{Variables: serviceapi.RestrictedEnvironment()}
	process := AgentProcessRequest{Workspace: info.Path, Environment: environment}
	process, cleanup, err := prepareAgentProcessRequest(ctx, process, workerGitHubPolicy{})
	if err != nil {
		return nil, RunResult{}, err
	}
	base := effectiveModel("", selection.Model, runtime.defaultModelForRole(RolePlan))
	selected := resolveAttemptAgentSelection(ctx, req, process, base, RolePlan, workflow.Config, provider, backend)
	if err := errors.Join(selected.Err, cleanup()); err != nil {
		return nil, RunResult{}, err
	}
	modelProvider, tier, effort := agentTurnIdentityOptions(provider)
	if selected.Effort != "" {
		effort = selected.Effort
	}
	model := effectiveModel("", selected.Model, base)
	requirement := providercapacity.Requirement{Role: RolePlan, Backend: selection.BackendID, Model: model}
	if requirement.Model == "" {
		requirement.Model = "provider_default"
	}
	var projection *dispatchBudgetProjection
	if workflow.Config.Budget.EffectiveBillingMode() != config.BillingModeSubscription {
		admission, refused, err := r.checkDispatchBudget(ctx, checker, estimator, req.Issue, requirement.Model, r.now())
		if err != nil {
			return nil, RunResult{}, err
		}
		if refused {
			return nil, admission, errors.New("planner budget refused")
		}
		projection = admission.budgetProjection
	}
	if err := req.setPhaseProvider(ctx, requirement); err != nil {
		return nil, RunResult{}, err
	}
	agentConfig := workflow.Config.EffectiveModelSelection().SessionAgent(workflow.Config.Agent, selected.Selection.Level)
	timeout := durationFromMillis(workflow.Config.Agent.MaxTurnDurationMS)
	if timeout <= 0 {
		timeout = sessiontimeout.DefaultTurn
	}
	turn := AgentTurnRequest{Workspace: info.Path, ReadOnly: true, MaxTurns: 1, MaxDuration: timeout,
		Model: model, ModelProvider: modelProvider, ServiceTier: tier, ReasoningEffort: effort, Environment: environment,
		MaxRSSBytes: r.maxAgentRSSBytes, RSSPollInterval: r.rssPollInterval, processRSS: r.processRSS}
	var totalTokens int64
	var totalCost float64
	generate := func(ctx context.Context, prompt string) (string, error) {
		if err := req.Execution.Validate(ctx); err != nil {
			return "", err
		}
		request := turn
		request.Prompt = prompt
		var turnTokens AgentTokenUsage
		text, err := r.generateTextTurn(ctx, backend, request, req.Execution, provider.Kind, 24<<10, func(update AgentUpdate) error {
			if update.Type != AgentUpdateTokenUsage {
				return nil
			}
			turnTokens = update.Tokens
			update.Tokens.TotalTokens += totalTokens
			if err := r.enforceSessionTokenCeiling(agentConfig, req.Issue, update); err != nil {
				return err
			}
			return r.enforceSessionBudgetProjection(projection, totalCost, requirement.Model, provider.Kind, update)
		})
		totalTokens += turnTokens.TotalTokens
		if projection != nil {
			totalCost += r.usageCostUSD(requirement.Model, turnTokens.InputTokens, turnTokens.CachedInputTokens, turnTokens.OutputTokens, provider.Kind)
		}
		return text, err
	}
	return generate, RunResult{}, nil
}

type phasedProviderExecution interface {
	StartPhased(context.Context, tracker.NativeExecutionIdentity) error
	SetPhaseProvider(context.Context, providercapacity.Requirement) error
}
