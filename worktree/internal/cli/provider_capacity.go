package cli

import (
	"context"
	"errors"
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
			for i := range reports {
				for _, backend := range backends {
					if backend.ID == reports[i].Backend {
						reports[i].LocalBackendIdentity = workflowconfig.ProviderIdentity(backend)
						break
					}
				}
			}
			reports = withProviderBilling(ctx, reports, backends, billing, usage)
			reports = withProviderUsage(reports, usage)
			return reports, providercapacity.Validate(reports)
		}
		var reports []providercapacity.Report
		var catalogErrors []error
		var selected []workflowconfig.AgentBackend
		for _, backend := range backends {
			index := slices.IndexFunc(selected, func(other workflowconfig.AgentBackend) bool { return other.ID == backend.ID })
			if index >= 0 && !runnerProviderCompatible(selected[index], backend) {
				slog.Warn("Runner provider backend identity conflicts across projects", "backend", backend.ID)
				continue
			}
			if index < 0 {
				selected = append(selected, backend)
			}
			report := providercapacity.Report{Provider: runnerProviderName(backend), Backend: backend.ID, AccountAlias: account, Availability: "unknown"}
			index = slices.IndexFunc(reports, func(existing providercapacity.Report) bool { return existing.Backend == backend.ID })
			if index >= 0 {
				report = reports[index]
			}
			catalog, err := models(ctx, backend)
			if err != nil {
				catalogErrors = append(catalogErrors, fmt.Errorf("provider models for backend %s: %w", backend.ID, err))
				continue
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
			if !slices.Contains(report.Models, "provider_default") {
				report.Models = append(report.Models, "provider_default")
			}
			report.LocalBackendIdentity = workflowconfig.ProviderIdentity(backend)
			report.ObservedAt = now().UTC()
			if index >= 0 {
				reports[index] = report
			} else {
				reports = append(reports, report)
			}
		}
		if len(reports) == 0 && len(catalogErrors) > 0 {
			return nil, fmt.Errorf("%w: %w", providercapacity.ErrCatalogUnavailable, errors.Join(catalogErrors...))
		}
		reports = withProviderBilling(ctx, reports, backends, billing, usage)
		reports = withProviderUsage(reports, usage)
		return reports, providercapacity.Validate(reports)
	}
}

func runnerProviderName(backend workflowconfig.AgentBackend) string {
	return workflowconfig.ProviderName(backend)
}
func runnerProviderCompatible(a, b workflowconfig.AgentBackend) bool {
	return workflowconfig.ProviderCompatible(a, b)
}
func runnerProviderCommandIdentity(backend workflowconfig.AgentBackend) string {
	return workflowconfig.ProviderCommandIdentity(backend)
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
		configured := workflow.AgentBackendConfigs()
		for _, backend := range configured {
			if backend.Disabled {
				continue
			}
			if !slices.ContainsFunc(backends, func(existing workflowconfig.AgentBackend) bool { return reflect.DeepEqual(existing, backend) }) {
				backends = append(backends, backend)
			}
		}
		for _, route := range workflow.AgentRouteConfigs() {
			local := slices.IndexFunc(configured, func(backend workflowconfig.AgentBackend) bool { return backend.ID == route.Backend })
			canonical := slices.IndexFunc(backends, func(backend workflowconfig.AgentBackend) bool { return backend.ID == route.Backend })
			if local >= 0 && canonical >= 0 && runnerProviderCompatible(configured[local], backends[canonical]) {
				routes = append(routes, route)
			}
		}
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
