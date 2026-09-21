package runner

import (
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

// ConfiguredBoardIdentity previews configuration without starting a backend or
// consulting its catalog. Attempt identity remains authoritative after dispatch.
func ConfiguredBoardIdentity(cfg config.Config, issue connector.Issue, ctx selector.Context) (agentidentity.Identity, error) {
	router, err := NewRouter(routesFromConfig(cfg.AgentRouteConfigs()))
	if err != nil {
		return agentidentity.Identity{}, err
	}
	route, err := router.RouteForRole(issue, selectorContext(ctx, config.Workflow{Config: cfg}), RoleCode)
	if err != nil {
		return agentidentity.Identity{}, err
	}
	for _, backend := range cfg.AgentBackendConfigs() {
		if backend.ID != route.BackendID {
			continue
		}
		selected := configuredAutomaticSelection(issue, route.Model, RoleCode, cfg, backend)
		if selected.Err != nil {
			return agentidentity.Identity{}, selected.Err
		}
		identity := configuredRuntimeIdentity(route, backend, RoleCode, selected.Model, time.Time{})
		identity.ReasoningEffort = agentidentity.NewValue(selected.Effort, agentidentity.ProvenanceConfigured)
		return identity, nil
	}
	return agentidentity.Identity{}, ErrMissingAgentBackend
}
