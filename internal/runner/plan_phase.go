package runner

import (
	"context"
	"errors"
	"github.com/digitaldrywood/detent/internal/runner/runworkspace"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner/planphase"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	planConfig := gate.EffectivePlan(workflow.Config.Plan)
	req.phaseContinuation.Plan.ResolveLegacyReview(planConfig, req.Issue.Labels)
	review := planConfig.Review
	return planphase.Run(ctx, planphase.Request{
		Execution: execution, Continuation: req.phaseContinuation, Backend: backend, Info: info, Issue: issue,
		Description: req.Issue.Description, HumanReview: review != gate.PlanReviewAutomated, AutomatedReview: review != gate.PlanReviewHuman, Now: r.now,
		Prompt: func() (string, error) {
			return buildRunPrompt(workflow, &req, RunModePlan, PromptOptions{PlanOnly: true, WorkspacePath: info.Path, Branch: info.Branch}, workspace.MergePrepareResult{})
		},
		OpenAgent: func() (func(context.Context, string) (string, error), error) {
			return r.planPhaseAgent(ctx, req, workflow, runtime, info)
		},
	})
}

func (r *Runner) planPhaseAgent(ctx context.Context, req RunRequest, workflow config.Workflow, runtime agentRuntime, info workspace.Info) (func(context.Context, string) (string, error), error) {
	req.Mode, req.ResumeState = RunModePlan, store.AgentResumeState{}
	selection, backend, provider, err := runtime.selectRequestBackend(req, runworkspace.SelectorContext(req.SelectorContext, workflow), RolePlan)
	if err != nil {
		return nil, err
	}
	environment := procgroup.Environment{Variables: serviceapi.RestrictedEnvironment()}
	process := AgentProcessRequest{Workspace: info.Path, Environment: environment}
	process, cleanup, err := prepareAgentProcessRequest(ctx, process, workerGitHubPolicy{})
	if err != nil {
		return nil, err
	}
	base := effectiveModel("", selection.Model, runtime.defaultModelForRole(RolePlan))
	selected := resolveAttemptAgentSelection(ctx, req, process, base, RolePlan, workflow.Config, provider, backend)
	if err := errors.Join(selected.Err, cleanup()); err != nil {
		return nil, err
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
	if err := req.Execution.(phasedProviderExecution).SetPhaseProvider(ctx, requirement); err != nil {
		return nil, err
	}
	timeout := durationFromMillis(workflow.Config.Agent.MaxTurnDurationMS)
	if timeout <= 0 {
		timeout = sessiontimeout.DefaultTurn
	}
	turn := AgentTurnRequest{Workspace: info.Path, ReadOnly: true, MaxTurns: 1, MaxDuration: timeout,
		Model: model, ModelProvider: modelProvider, ServiceTier: tier, ReasoningEffort: effort, Environment: environment}
	generate := func(ctx context.Context, prompt string) (string, error) {
		if err := req.Execution.Validate(ctx); err != nil {
			return "", err
		}
		request := turn
		request.Prompt = prompt
		return r.generateTextTurn(ctx, backend, request, req.Execution, provider.Kind, 24<<10, nil, nil)
	}
	return generate, nil
}

type phasedProviderExecution interface {
	StartPhased(context.Context, tracker.NativeExecutionIdentity) error
	SetPhaseProvider(context.Context, providercapacity.Requirement) error
}
