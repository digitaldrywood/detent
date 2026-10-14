package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
)

func runnerProviderReports(ctx context.Context, cfg globalconfig.Config, account string, now func() time.Time, models func(context.Context, workflowconfig.AgentBackend) ([]runner.AgentModel, error)) func() ([]providercapacity.Report, error) {
	account = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(strings.TrimSpace(account)))
	account = strings.Trim(account, "_.-")
	if len(account) > 64 {
		account = account[:64]
	}
	if account == "" {
		account = "runner"
	}
	workflow := workflowconfig.Default().WithAgentDefaults(cfg.Global.Agents, cfg.Global.Budget)
	return func() ([]providercapacity.Report, error) {
		if cfg.Client.ProviderCapacityFile != "" {
			return providercapacity.Load(cfg.Client.ProviderCapacityFile)
		}
		var reports []providercapacity.Report
		for _, backend := range workflow.AgentBackendConfigs() {
			if backend.Disabled {
				continue
			}
			provider := backend.Provider
			if provider == "" {
				switch backend.Kind {
				case workflowconfig.AgentBackendCodex:
					provider = "openai"
				case workflowconfig.AgentBackendClaudeCode:
					provider = "anthropic"
				default:
					provider = backend.Kind
				}
			}
			report := providercapacity.Report{Provider: provider, Backend: backend.ID, AccountAlias: account, Availability: "unknown"}
			catalog, err := models(ctx, backend)
			if err != nil {
				return nil, fmt.Errorf("provider models for backend %s: %w", backend.ID, err)
			}
			for _, model := range catalog {
				for _, id := range []string{model.ID, model.Model} {
					if id != "" && !slices.Contains(report.Models, id) {
						report.Models = append(report.Models, id)
					}
				}
			}
			if len(catalog) == 0 {
				for _, route := range workflow.AgentRouteConfigs() {
					if route.Backend == backend.ID && route.Model != "" && !slices.Contains(report.Models, route.Model) {
						report.Models = append(report.Models, route.Model)
					}
				}
			}
			report.Models = append(report.Models, "provider_default")
			report.ObservedAt = now().UTC()
			reports = append(reports, report)
		}
		return reports, providercapacity.Validate(reports)
	}
}

func runnerProviderModels(ctx context.Context, cfg workflowconfig.AgentBackend) ([]runner.AgentModel, error) {
	backend, err := buildAgentBackend(cfg)
	if err != nil {
		return nil, err
	}
	if provider, ok := backend.(runner.AgentModelCatalogProvider); ok {
		return provider.ListModels(ctx, runner.AgentProcessRequest{})
	}
	return nil, nil
}
