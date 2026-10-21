package runner

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

type ProviderCapacityResolver interface {
	DispatchCapacity(context.Context, RunRequest) (providercapacity.Requirement, error)
}

func (r *Runner) DispatchCapacity(_ context.Context, req RunRequest) (providercapacity.Requirement, error) {
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runRole(req.Mode, req.Issue)
	selection, backend, backendConfig, err := runtime.selectRequestBackend(req, selectorContext(req.SelectorContext, workflow), role)
	if err != nil {
		return providercapacity.Requirement{}, err
	}
	baseModel := effectiveModel("", selection.Model, runtime.defaultModelForRole(role))
	model := baseModel
	// Capacity is resolved before a claim or workspace exists. Use the same
	// configured selection as the worker and the provider's advertised models;
	// catalog discovery belongs to the attempt, after workspace preparation.
	policy := workflow.Config.EffectiveModelSelection()
	automatic := policy.Active() && policy.BackendKinds != nil && slices.Contains(*policy.BackendKinds, backendConfig.Kind)
	override, _, overrideErr := selectionIssueOverride(req.Issue, role)
	explicitModel, _ := override.ModelForRole(role)
	var models []string
	for _, report := range req.ProviderReports {
		if report.Backend == selection.BackendID {
			models = report.Models
			break
		}
	}
	if !hasResumeIdentity(req) {
		if automatic {
			if _, ok := backend.(AgentModelCatalogProvider); !ok {
				return providercapacity.Requirement{}, errors.New("automatic model selection requires a backend model catalog; configure an eligible backend or disable the policy")
			}
			requested := configuredAutomaticSelection(req.Issue, baseModel, role, workflow.Config, backendConfig)
			if requested.Err != nil {
				return providercapacity.Requirement{}, requested.Err
			}
			// Remove unavailable issue models using the existing provider report;
			// the attempt publishes the rejection after catalog validation.
			for models != nil && explicitModel != "" && !slices.Contains(models, explicitModel) {
				_, field := override.ModelForRole(role)
				override = clearAgentOverrideField(override, field)
				explicitModel, _ = override.ModelForRole(role)
				requested = configuredOverrideSelection(req.Issue, baseModel, role, workflow.Config, backendConfig, override)
			}
			model = requested.Model
			if models != nil && !slices.Contains(models, model) && baseModel == "" && explicitModel == "" && policy.Unavailable != nil && *policy.Unavailable == "fallback" && policy.FallbackOrder != nil {
				for _, candidate := range *policy.FallbackOrder {
					if fallback := policy.Model(candidate); slices.Contains(models, fallback) {
						model = fallback
						break
					}
				}
			}
		} else if overrideErr == nil && explicitModel != "" && (models == nil || slices.Contains(models, explicitModel)) {
			if _, ok := backend.(AgentModelCatalogProvider); ok {
				model = explicitModel
			}
		}
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = "provider_default"
	}
	result := providercapacity.Requirement{Role: role, Backend: selection.BackendID, Model: model}
	return result, result.Validate()
}

// ProviderCapacityExecution exposes the existing reservation to model selection,
// so dispatch and attempt validation use the same advertised model scope.
type ProviderCapacityExecution interface {
	ProviderCapacity() *providercapacity.Reservation
}

func capacitySelectionBackend(req RunRequest, backend AgentBackend) AgentBackend {
	execution, ok := req.Execution.(ProviderCapacityExecution)
	if !ok {
		return backend
	}
	reservation := execution.ProviderCapacity()
	if reservation == nil {
		return backend
	}
	provider, ok := backend.(AgentModelCatalogProvider)
	if !ok {
		return backend
	}
	catalog := &capacityModelCatalog{AgentBackend: backend, provider: provider, models: reservation.Report.Models}
	if defaults, ok := backend.(AgentDefaultModelProvider); ok {
		return &capacityDefaultModelCatalog{capacityModelCatalog: catalog, AgentDefaultModelProvider: defaults}
	}
	return catalog
}

type capacityModelCatalog struct {
	AgentBackend
	provider AgentModelCatalogProvider
	models   []string
}

func (c *capacityModelCatalog) ListModels(ctx context.Context, process AgentProcessRequest) ([]AgentModel, error) {
	models, err := c.provider.ListModels(ctx, process)
	if err != nil {
		return nil, err
	}
	available := make([]AgentModel, 0, len(models))
	for _, model := range models {
		if slices.Contains(c.models, canonicalAgentModel(model, model.ID)) {
			available = append(available, model)
		}
	}
	return available, nil
}

type capacityDefaultModelCatalog struct {
	*capacityModelCatalog
	AgentDefaultModelProvider
}
