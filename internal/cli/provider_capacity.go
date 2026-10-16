package cli

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
)

func runnerProviderReports(ctx context.Context, configuration func() globalconfig.Config, account string, now func() time.Time, models func(context.Context, workflowconfig.AgentBackend) ([]runner.AgentModel, error), billing func(context.Context, workflowconfig.AgentBackend) (string, error), usage ...*runner.ProviderUsage) func() ([]providercapacity.Report, error) {
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
	return func() ([]providercapacity.Report, error) {
		cfg := configuration()
		backends, routes := runnerProviderConfigs(ctx, cfg)
		if cfg.Client.ProviderCapacityFile != "" {
			reports, err := providercapacity.Load(cfg.Client.ProviderCapacityFile)
			if err != nil {
				return nil, err
			}
			reports = withProviderBilling(ctx, reports, backends, billing, usage)
			reports = withProviderUsage(reports, usage)
			return reports, providercapacity.Validate(reports)
		}
		var reports []providercapacity.Report
		for _, backend := range backends {
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
			for _, route := range routes {
				if !route.Disabled && route.Backend == backend.ID && route.Model != "" && !slices.Contains(report.Models, route.Model) {
					report.Models = append(report.Models, route.Model)
				}
			}
			report.Models = append(report.Models, "provider_default")
			report.ObservedAt = now().UTC()
			reports = append(reports, report)
		}
		reports = withProviderBilling(ctx, reports, backends, billing, usage)
		reports = withProviderUsage(reports, usage)
		return reports, providercapacity.Validate(reports)
	}
}

func runnerProviderConfigs(ctx context.Context, cfg globalconfig.Config) ([]workflowconfig.AgentBackend, []workflowconfig.AgentRoute) {
	var workflows []workflowconfig.Config
	if len(cfg.Projects) == 0 {
		workflows = append(workflows, workflowconfig.Default().WithAgentDefaults(cfg.Global.Agents, cfg.Global.Budget))
	}
	for _, selected := range project.ManagerConfigFromGlobal(cfg).Projects {
		workflow, err := project.LoadWorkflowContext(ctx, selected)
		if err != nil {
			slog.Warn("Runner provider project definition unavailable", "project", selected.ID, "error", err)
			continue
		}
		workflows = append(workflows, workflow.Config)
	}
	var backends []workflowconfig.AgentBackend
	var routes []workflowconfig.AgentRoute
	for _, workflow := range workflows {
		for _, backend := range workflow.AgentBackendConfigs() {
			if backend.Disabled {
				continue
			}
			if !slices.ContainsFunc(backends, func(existing workflowconfig.AgentBackend) bool { return reflect.DeepEqual(existing, backend) }) {
				backends = append(backends, backend)
			}
		}
		routes = append(routes, workflow.AgentRouteConfigs()...)
	}
	return backends, routes
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

func withProviderUsage(reports []providercapacity.Report, usage []*runner.ProviderUsage) []providercapacity.Report {
	for i := range reports {
		if len(usage) > 0 && usage[0] != nil {
			reports[i].Billing = usage[0].Billing(reports[i].Backend)
			if windows := usage[0].Windows(reports[i].Backend); len(windows) > 0 {
				reports[i].UsageWindows = windows
			}
		}
		reports[i].Billing = reports[i].BillingBasis()
		if reports[i].Billing == "api_key" {
			reports[i].UsageWindows = nil
		}
	}
	return reports
}

func runnerProviderBilling(ctx context.Context, cfg workflowconfig.AgentBackend) (string, error) {
	backend, err := buildAgentBackend(cfg)
	if err != nil {
		return "unknown", err
	}
	if provider, ok := backend.(runner.AgentBillingProvider); ok {
		return provider.BillingBasis(ctx, runner.AgentProcessRequest{})
	}
	return "unknown", nil
}

func withProviderBilling(ctx context.Context, reports []providercapacity.Report, backends []workflowconfig.AgentBackend, billing func(context.Context, workflowconfig.AgentBackend) (string, error), usage []*runner.ProviderUsage) []providercapacity.Report {
	if billing == nil {
		return reports
	}
	for i := range reports {
		reports[i].Billing = "unknown"
		for _, backend := range backends {
			if backend.ID != reports[i].Backend || backend.Disabled {
				continue
			}
			observed, err := billing(ctx, backend)
			if err == nil && providercapacity.ValidBilling(observed) && observed != "" {
				reports[i].Billing = observed
			}
			break
		}
		if len(usage) > 0 {
			usage[0].ObserveBilling(reports[i].Backend, reports[i].Billing)
		}
	}
	return reports
}
