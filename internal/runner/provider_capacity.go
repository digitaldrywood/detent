package runner

import (
	"context"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

type ProviderCapacityResolver interface {
	DispatchCapacity(context.Context, RunRequest) (providercapacity.Requirement, error)
}

func (r *Runner) DispatchCapacity(ctx context.Context, req RunRequest) (providercapacity.Requirement, error) {
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runRole(req.Mode, req.Issue)
	selection, backend, backendConfig, err := runtime.selectRequestBackend(req, selectorContext(req.SelectorContext, workflow), role)
	if err != nil {
		return providercapacity.Requirement{}, err
	}
	baseModel := effectiveModel("", selection.Model, runtime.defaultModelForRole(role))
	override := resolveRequestAgentSelection(ctx, req, "", baseModel, role, workflow.Config, backendConfig, backend)
	if override.Err != nil {
		return providercapacity.Requirement{}, override.Err
	}
	model := effectiveModel("", override.Model, runtime.defaultModelForRole(role))
	if model == "" {
		model = "provider_default"
	}
	result := providercapacity.Requirement{Role: role, Backend: selection.BackendID, Model: model}
	return result, result.Validate()
}

// ProviderModelDetails projects a backend's own model catalogue onto the
// per-model detail a provider capacity report carries.
//
// The catalogue is the same one automatic model selection reads
// (AgentModelCatalogProvider): identifiers, which model the backend defaults
// to, the reasoning efforts each model supports, and the successor a retired
// model names. Only models the report already advertises are described —
// detail never widens what capacity will match — and `configuredEffort` is
// this runner's own effort for the code role, published as the model's
// default when the model supports it, because that is the effort a turn this
// runner takes would actually run at.
//
// The identifier is the canonical one the rest of dispatch uses: Model when
// the catalogue gives one, otherwise ID (`canonicalAgentModel`).
func ProviderModelDetails(provider string, reported []string, configuredEffort string, catalog []AgentModel) []providercapacity.ModelDetail {
	configuredEffort = strings.ToLower(strings.TrimSpace(configuredEffort))
	details := make([]providercapacity.ModelDetail, 0, len(catalog))
	seen := make(map[string]bool, len(catalog))
	for _, model := range catalog {
		id := canonicalAgentModel(model, "")
		if id == "" || seen[id] || !slices.Contains(reported, id) {
			continue
		}
		seen[id] = true
		detail := providercapacity.ModelDetail{
			ID:       id,
			Label:    id,
			Provider: strings.TrimSpace(provider),
			Default:  model.Default,
			// A model the catalogue names a successor for is retired for
			// dispatch (`availableSelectionModel`); a picker still shows it,
			// shelved, because an operator may have pinned it by hand.
			Legacy: strings.TrimSpace(model.Upgrade) != "",
		}
		// The ladder keeps the catalogue's order, least to most, and carries
		// each rung once: a report may not repeat one.
		rungs := make(map[string]bool, len(model.SupportedReasoningEfforts))
		for _, effort := range model.SupportedReasoningEfforts {
			effort = strings.ToLower(strings.TrimSpace(effort))
			if effort == "" || rungs[effort] {
				continue
			}
			rungs[effort] = true
			detail.ReasoningEfforts = append(detail.ReasoningEfforts, effort)
		}
		if slices.Contains(detail.ReasoningEfforts, configuredEffort) {
			detail.DefaultReasoningEffort = configuredEffort
		}
		details = append(details, detail)
	}
	return details
}
