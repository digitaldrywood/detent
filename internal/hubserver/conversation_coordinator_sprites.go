package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) coordinatorSpritePoolStatus(ctx context.Context, scope hubscope.NativeScope, view spritePoolView, connector string, project tracker.ProjectID) (any, error) {
	secret, err := s.readAccessSecretStatus(ctx, scope)
	if err != nil {
		return nil, err
	}
	validation := "No Sprites organization token is set"
	if secret.Present {
		if s.config.SecretKeys == nil {
			validation = "The Hub secret store is unavailable"
		} else if _, err := s.checkProjectSprites(ctx, scope); err != nil {
			validation = "The Sprites organization could not be validated; check the connector, billing and provider availability"
			if errors.Is(err, errSpritesTokenRejected) || errors.Is(err, errSpritesBilling) {
				validation = err.Error()
			}
		} else {
			validation = "Sprites organization token validated"
		}
	}
	if err := s.describeSpritePoolMembers(ctx, scope, view.Members); err != nil {
		return nil, err
	}
	members := make([]map[string]any, 0, len(view.Members))
	connected := 0
	for _, member := range view.Members {
		item := map[string]any{"name": member.Name, "state": member.State, "status": member.Status, "runner_id": member.RunnerID, "connected": false, "provider_readiness": "unavailable"}
		if len(member.WaitingFor) > 0 {
			item["waiting_for"] = member.WaitingFor
		}
		if member.RunnerID != "" && member.State == "enrolled" {
			r, err := s.spritePoolRunner(ctx, scope, member.RunnerID)
			if err != nil {
				return nil, err
			}
			if r.Scope == "organization" {
				item["isolation_tier"] = r.IsolationTier
				item["problems"] = r.Problems
				item["backend_isolation"] = r.BackendIsolation
				item["connection_health"] = r.ConnectionHealth
				item["problems"] = r.Problems
				item["last_heartbeat_at"] = r.LastHeartbeatAt
				item["health"] = r.Health
				item["reported_capacity"] = r.ReportedCapacity
				item["connected"] = r.State == "active" && r.ConnectionHealth == "online"
				if item["connected"] == true {
					connected++
				}
				providers := make([]map[string]string, 0, len(r.ProviderCapacity))
				for _, provider := range r.ProviderCapacity {
					providers = append(providers, map[string]string{"provider": provider.Provider, "backend": provider.Backend, "availability": provider.Availability, "state": provider.State})
				}
				if len(providers) > 0 {
					item["provider_readiness"] = providers
				}
			}
		}
		members = append(members, item)
	}
	setupScope := scope
	setupScope.Project = project
	declared, err := s.projectRunnerSetupDeclaration(ctx, setupScope)
	if err != nil {
		return nil, err
	}
	return map[string]any{"share_idle_capacity": view.ShareIdleCapacity, "runner_setup_declared": declared, "token": secret, "token_validation": validation, "connector_url": connector, "isolation_tier": view.IsolationTier, "isolation_tier_unavailable_reason": view.IsolationTierUnavailableReason, "min_runners": view.MinRunners, "max_runners": view.MaxRunners, "idle_seconds": view.IdleSeconds, "revision": view.Revision, "bootstrap_configured": strings.TrimSpace(view.Bootstrap) != "", "connected_runners": connected, "members": members, "login_guide": "https://github.com/digitaldrywood/detent/blob/develop/docs/sprite-runners.md#6-sign-in-to-your-agents"}, nil
}

func (s *Service) projectRunnerSetupDeclaration(ctx context.Context, scope hubscope.NativeScope) (*bool, error) {
	onboarding, err := s.projectOnboarding(ctx, scope)
	if err != nil {
		return nil, err
	}
	var declared *bool
	for _, entry := range onboarding.Runners {
		if entry.Runner.State != "active" || entry.Runner.ConnectionHealth != "online" || entry.LocalChecks == nil || entry.LocalChecks.Checkout != "passed" || entry.LocalChecks.RunnerSetupDeclared == nil {
			continue
		}
		declared = entry.LocalChecks.RunnerSetupDeclared
		if !*declared {
			break
		}
	}
	return declared, nil
}
