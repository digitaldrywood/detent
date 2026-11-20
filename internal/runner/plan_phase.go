package runner

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner/planphase"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) runPlanPhase(ctx context.Context, req RunRequest, workflow config.Workflow, runtime agentRuntime, backend workspace.Backend, info workspace.Info, issue workspace.Issue) (string, string, error) {
	if !workflow.Config.Plan.Enabled {
		return "", "", nil
	}
	execution, ok := req.Execution.(planphase.Execution)
	if !ok {
		return "", "", errors.New("native planning requires phase and step receipts")
	}
	return planphase.Run(ctx, planphase.Request{
		Execution: execution, Continuation: req.phaseContinuation, Backend: backend, Info: info, Issue: issue,
		Description: req.Issue.Description, HumanReview: gate.EffectivePlan(workflow.Config.Plan).Review == gate.PlanReviewHuman, Now: r.now,
		Unretired: UnretiredProviderSession,
		Prompt: func() (string, error) {
			return buildRunPrompt(workflow, &req, RunModePlan, PromptOptions{PlanOnly: true, WorkspacePath: info.Path, Branch: info.Branch}, workspace.MergePrepareResult{})
		},
		OpenAgent: func() (func(context.Context, string) (string, error), func(bool) error, error) {
			return r.planPhaseAgent(ctx, req, workflow, runtime, info)
		},
	})
}

func (r *Runner) planPhaseAgent(ctx context.Context, req RunRequest, workflow config.Workflow, runtime agentRuntime, info workspace.Info) (func(context.Context, string) (string, error), func(bool) error, error) {
	selection, backend, provider, err := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), runtime.effectiveRunRole(RolePlan))
	if err != nil {
		return nil, nil, err
	}
	environment := procgroup.Environment{Variables: serviceapi.RestrictedEnvironment()}
	process := AgentProcessRequest{Workspace: info.Path, Environment: environment}
	process, cleanup, err := prepareAgentProcessRequest(ctx, process, workerGitHubPolicy{})
	if err != nil {
		return nil, nil, err
	}
	base := effectiveModel("", selection.Model, runtime.defaultModelForRole(RolePlan))
	selected := resolveAgentSelection(ctx, req.Issue, process, base, RolePlan, workflow.Config, provider, backend)
	if err := errors.Join(selected.Err, cleanup()); err != nil {
		return nil, nil, err
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
	release, err := planphase.AcquireProvider(ctx, req.Execution, requirement)
	if err != nil {
		return nil, nil, err
	}
	timeout := durationFromMillis(workflow.Config.Agent.MaxTurnDurationMS)
	if timeout <= 0 {
		timeout = sessiontimeout.DefaultTurn
	}
	turn := AgentTurnRequest{Workspace: info.Path, ReadOnly: true, MaxTurns: 1, MaxDuration: timeout,
		Model: model, ModelProvider: modelProvider, ServiceTier: tier, ReasoningEffort: effort, Environment: environment}
	budget := workflow.Config.Agent.MaxTurns
	if budget <= 0 {
		budget = config.Default().Agent.MaxTurns
	}
	turns := &sourceTurnBudget{remaining: max(2, budget)}
	generate := func(ctx context.Context, prompt string) (string, error) {
		if err := req.Execution.Validate(ctx); err != nil {
			return "", err
		}
		request := turn
		request.Prompt = prompt
		return r.generateTextTurn(context.WithValue(ctx, sourceTurnBudgetKey{}, turns), backend, request, req.Execution, provider.Kind, 24<<10, nil, nil)
	}
	return generate, release, nil
}
