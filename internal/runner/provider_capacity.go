package runner

import (
	"context"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

type ProviderCapacityResolver interface {
	DispatchCapacity(context.Context, RunRequest) (providercapacity.Requirement, error)
}

func (r *Runner) DispatchCapacity(ctx context.Context, req RunRequest) (providercapacity.Requirement, error) {
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runRole(req.Mode, req.Issue)
	selection, backend, _, err := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), runtime.effectiveRunRole(role))
	if err != nil {
		return providercapacity.Requirement{}, err
	}
	effort, field := workflow.Config.Agent.Effort.Resolve(role)
	override := resolveAgentOverride(ctx, req.Issue, "", selection.Model, role, agentEffortCandidate{Field: field, Effort: effort}, backend)
	model := effectiveModel("", override.Model, runtime.defaultModelForRole(role))
	if model == "" {
		model = "provider_default"
	}
	result := providercapacity.Requirement{Role: role, Backend: selection.BackendID, Model: model}
	return result, result.Validate()
}
