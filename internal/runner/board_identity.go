package runner

import (
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

// BoardIdentityResolver previews configured identities with a router shared by
// all cards in a project snapshot. It never starts a backend or consults its catalog.
type BoardIdentityResolver struct {
	cfg    config.Config
	router *Router
	ctx    selector.Context
}

func NewBoardIdentityResolver(cfg config.Config, ctx selector.Context) (*BoardIdentityResolver, error) {
	router, err := NewRouter(routesFromConfig(cfg.AgentRouteConfigs()))
	if err != nil {
		return nil, err
	}
	return &BoardIdentityResolver{cfg: cfg, router: router, ctx: selectorContext(ctx, config.Workflow{Config: cfg})}, nil
}

// Identity resolves each issue independently; attempt identity remains authoritative.
func (r *BoardIdentityResolver) Identity(issue connector.Issue) (agentidentity.Identity, error) {
	cfg := r.cfg
	route, err := r.router.RouteForRole(issue, r.ctx, RoleCode)
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
