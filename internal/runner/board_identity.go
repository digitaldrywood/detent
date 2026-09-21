package runner

import (
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/agentoverride"

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
	runtime := agentRuntime{router: r.router}
	role := runRole(RunModeImplement, issue)
	routeRole := runtime.effectiveRunRole(role)
	route, err := r.router.RouteForRole(issue, r.ctx, routeRole)
	if err != nil {
		return agentidentity.Identity{}, err
	}
	for _, backend := range cfg.AgentBackendConfigs() {
		if backend.ID != route.BackendID {
			continue
		}
		baseModel := effectiveModel(route.Model, runtime.defaultModelForRole(role))
		policy := cfg.EffectiveModelSelection()
		var selected agentSelection
		if policy.Active() && policy.BackendKinds != nil && slices.Contains(*policy.BackendKinds, backend.Kind) {
			selected = configuredAutomaticSelection(issue, baseModel, role, cfg, backend)
		} else {
			// Preview configured candidates only; dispatch validates against the
			// backend catalog before accepting issue overrides.
			override, _, err := agentoverride.FromIssueBody(issue.Description)
			if err != nil {
				override = agentoverride.Override{}
			}
			model, _ := override.ModelForRole(role)
			selected.Model = effectiveModel(model, baseModel)
			effort, field := cfg.Agent.Effort.Resolve(role)
			efforts := agentEffortCandidates(override, role, agentEffortCandidate{Field: field, Effort: effort})
			if len(efforts) > 0 {
				selected.Effort = efforts[0].Effort
			}
		}
		if selected.Err != nil {
			return agentidentity.Identity{}, selected.Err
		}
		identity := configuredRuntimeIdentity(route, backend, role, selected.Model, time.Time{})
		if selected.Effort != "" {
			identity.ReasoningEffort = agentidentity.NewValue(selected.Effort, agentidentity.ProvenanceConfigured)
		}
		return identity, nil
	}
	return agentidentity.Identity{}, ErrMissingAgentBackend
}
