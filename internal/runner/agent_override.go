package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/agentoverride"
	"github.com/digitaldrywood/detent/internal/connector"
)

type resolvedAgentOverride struct {
	Model      string
	Effort     string
	Rejections []AgentOverrideRejection
}

func resolveAgentOverride(ctx context.Context, issue connector.Issue, workspace string, baseModel string, backend AgentBackend) resolvedAgentOverride {
	baseModel = strings.TrimSpace(baseModel)
	result := resolvedAgentOverride{Model: baseModel}
	override, found, err := agentoverride.FromIssueBody(issue.Description)
	if !found {
		return result
	}
	if err != nil {
		result.Rejections = []AgentOverrideRejection{{
			Field:  "block",
			Reason: err.Error(),
		}}
		return result
	}
	if override.Model == "" && override.Effort == "" {
		return result
	}

	provider, ok := backend.(AgentModelCatalogProvider)
	if !ok {
		return rejectUnavailableCatalog(result, override, "selected backend does not advertise a model catalog")
	}
	models, err := provider.ListModels(ctx)
	if err != nil {
		return rejectUnavailableCatalog(result, override, "model catalog unavailable: "+err.Error())
	}

	var effectiveModel AgentModel
	modelFound := false
	if override.Model != "" {
		requested, ok := findAgentModel(models, override.Model)
		if ok && strings.TrimSpace(requested.Upgrade) == "" {
			effectiveModel = requested
			modelFound = true
			result.Model = canonicalAgentModel(requested, override.Model)
		} else {
			reason := "model is not available from the selected backend"
			if ok {
				reason = fmt.Sprintf("model is retired; use %q", strings.TrimSpace(requested.Upgrade))
			}
			result.Rejections = append(result.Rejections, AgentOverrideRejection{
				Field:  "model",
				Value:  override.Model,
				Reason: reason,
			})
		}
	}

	if override.Effort == "" {
		return result
	}
	if !modelFound {
		effectiveBaseModel := baseModel
		if effectiveBaseModel == "" {
			defaultProvider, ok := backend.(AgentDefaultModelProvider)
			if !ok {
				result.Rejections = append(result.Rejections, AgentOverrideRejection{
					Field:  "effort",
					Value:  override.Effort,
					Reason: "selected backend does not advertise its effective default model",
				})
				return result
			}
			effectiveBaseModel, err = defaultProvider.DefaultModel(ctx, workspace)
			if err != nil {
				result.Rejections = append(result.Rejections, AgentOverrideRejection{
					Field:  "effort",
					Value:  override.Effort,
					Reason: "effective default model unavailable: " + err.Error(),
				})
				return result
			}
		}
		effectiveModel, modelFound = findAgentModel(models, effectiveBaseModel)
	}
	if !modelFound {
		result.Rejections = append(result.Rejections, AgentOverrideRejection{
			Field:  "effort",
			Value:  override.Effort,
			Reason: "effective model is not available in the selected backend catalog",
		})
		return result
	}
	if effort, ok := supportedAgentEffort(effectiveModel, override.Effort); ok {
		result.Effort = effort
		return result
	}

	model := canonicalAgentModel(effectiveModel, result.Model)
	result.Rejections = append(result.Rejections, AgentOverrideRejection{
		Field:  "effort",
		Value:  override.Effort,
		Reason: fmt.Sprintf("effort is not supported by model %q", model),
	})
	return result
}

func rejectUnavailableCatalog(result resolvedAgentOverride, override agentoverride.Override, reason string) resolvedAgentOverride {
	if override.Model != "" {
		result.Rejections = append(result.Rejections, AgentOverrideRejection{
			Field:  "model",
			Value:  override.Model,
			Reason: reason,
		})
	}
	if override.Effort != "" {
		result.Rejections = append(result.Rejections, AgentOverrideRejection{
			Field:  "effort",
			Value:  override.Effort,
			Reason: reason,
		})
	}
	return result
}

func findAgentModel(models []AgentModel, want string) (AgentModel, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		for _, model := range models {
			if model.Default {
				return model, true
			}
		}
		return AgentModel{}, false
	}
	for _, model := range models {
		if strings.TrimSpace(model.ID) == want || strings.TrimSpace(model.Model) == want {
			return model, true
		}
	}
	return AgentModel{}, false
}

func canonicalAgentModel(model AgentModel, fallback string) string {
	if value := strings.TrimSpace(model.Model); value != "" {
		return value
	}
	if value := strings.TrimSpace(model.ID); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func supportedAgentEffort(model AgentModel, want string) (string, bool) {
	want = strings.TrimSpace(want)
	for _, effort := range model.SupportedReasoningEfforts {
		if strings.EqualFold(strings.TrimSpace(effort), want) {
			return strings.TrimSpace(effort), true
		}
	}
	return "", false
}
