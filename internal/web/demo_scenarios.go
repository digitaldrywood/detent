package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/projectcolor"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	DemoScenarioHeader  = "X-Detent-Demo-Scenario"
	DemoModeScreenshots = "screenshots"
	DemoClockFrozen     = "frozen"
	DemoClockPlay       = "play"

	demoPrimaryProjectID   = "dogfood"
	demoPrimaryProjectName = "detent-core"
)

var demoBaseTime = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

var errDemoScenarioResponseWritten = errors.New("demo scenario response written")

type DemoConfig struct {
	Mode  string
	Clock string
}

type DemoScenarioManifest struct {
	ID             string            `json:"id"`
	Route          string            `json:"route"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Viewport       DemoViewport      `json:"viewport"`
	ScreenshotName string            `json:"screenshot_name"`
	WaitSelector   string            `json:"wait_selector"`
	KeySelectors   []string          `json:"key_selectors,omitempty"`
}

type DemoViewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type demoScenarioSet struct {
	clock     string
	manifest  []DemoScenarioManifest
	scenarios map[string]demoScenario
}

type demoScenario struct {
	ID           string
	Route        string
	Method       string
	WaitSelector string
	Page         string
	Variant      string
	ProjectID    string
	KanbanMode   string
	Status       int
}

type demoScenariosResponse struct {
	GeneratedAt time.Time              `json:"generated_at"`
	Header      string                 `json:"header"`
	Clock       string                 `json:"clock"`
	Scenarios   []DemoScenarioManifest `json:"scenarios"`
}

func newDemoScenarioSet(cfg DemoConfig) *demoScenarioSet {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode != DemoModeScreenshots {
		return nil
	}
	clock := strings.ToLower(strings.TrimSpace(cfg.Clock))
	if clock != DemoClockPlay {
		clock = DemoClockFrozen
	}
	defs := demoScenarioDefinitions()
	scenarios := make(map[string]demoScenario, len(defs))
	manifest := make([]DemoScenarioManifest, 0, len(defs))
	for _, def := range defs {
		method := strings.TrimSpace(def.Method)
		if method == "" {
			method = http.MethodGet
		}
		def.Method = method
		scenarios[def.ID] = def
		if demoScenarioInManifest(def) {
			manifest = append(manifest, DemoScenarioManifest{
				ID:             def.ID,
				Route:          def.Route,
				Method:         method,
				Headers:        map[string]string{DemoScenarioHeader: def.ID},
				Viewport:       DemoViewport{Width: 1440, Height: 1100},
				ScreenshotName: def.ID + ".png",
				WaitSelector:   def.WaitSelector,
				KeySelectors:   demoKeySelectors(def),
			})
		}
	}
	return &demoScenarioSet{
		clock:     clock,
		manifest:  manifest,
		scenarios: scenarios,
	}
}

func demoScenarioInManifest(scenario demoScenario) bool {
	return scenario.Route != "/events"
}

func demoScenarioDefinitions() []demoScenario {
	return []demoScenario{
		{ID: "fleet-empty-first-snapshot", Route: "/", WaitSelector: "#snapshot", Page: "fleet", Variant: "empty"},
		{ID: "fleet-healthy-parallel-work", Route: "/", WaitSelector: "#snapshot", Page: "fleet", Variant: "healthy"},
		{ID: "fleet-overloaded-rate-limited", Route: "/", WaitSelector: "#snapshot", Page: "fleet", Variant: "overloaded"},
		{ID: "fleet-draining-shutdown", Route: "/", WaitSelector: "#snapshot", Page: "fleet", Variant: "draining"},
		{ID: "fleet-dense-multiproject", Route: "/", WaitSelector: "#snapshot", Page: "fleet", Variant: "dense"},
		{ID: "fleet-degraded-telemetry", Route: "/", WaitSelector: "#snapshot", Page: "fleet", Variant: "degraded"},
		{ID: "github-api-healthy", Route: "/", WaitSelector: "#github-api-health", Page: "fleet", Variant: "github-api-healthy"},
		{ID: "github-api-warning", Route: "/", WaitSelector: "#github-api-health", Page: "fleet", Variant: "github-api-warning"},
		{ID: "github-api-secondary-backoff", Route: "/", WaitSelector: "#github-api-health", Page: "fleet", Variant: "github-api-secondary-backoff"},
		{ID: "github-api-primary-exhausted", Route: "/", WaitSelector: "#github-api-health", Page: "fleet", Variant: "github-api-primary-exhausted"},
		{ID: "fleet-kanban-multiproject", Route: "/kanban", WaitSelector: "#fleet-kanban", Page: "fleet-kanban", Variant: "dense-kanban", KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "project-active-overview", Route: "/projects/dogfood", WaitSelector: "#snapshot", Page: "project", Variant: "healthy", ProjectID: demoPrimaryProjectID},
		{ID: "project-paused-overview", Route: "/projects/mobile-client", WaitSelector: "#snapshot", Page: "project", Variant: "paused", ProjectID: "mobile-client"},
		{ID: "project-empty-overview", Route: "/projects/agent-lab", WaitSelector: "#snapshot", Page: "project", Variant: "project-empty", ProjectID: "agent-lab"},
		{ID: "project-hot-path", Route: "/projects/billing-api", WaitSelector: "#snapshot", Page: "project", Variant: "hot-path", ProjectID: "billing-api"},
		{ID: "project-not-found", Route: "/projects/missing-project", WaitSelector: "body", Page: "project", Variant: "not-found", ProjectID: "missing-project", Status: http.StatusNotFound},
		{ID: "kanban-full-integration", Route: "/projects/dogfood/kanban", WaitSelector: "#project-kanban", Page: "kanban", Variant: "healthy", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-startup-loading", Route: "/projects/dogfood/kanban", WaitSelector: "#project-kanban", Page: "kanban", Variant: "startup-loading", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-read-only", Route: "/projects/dogfood/kanban", WaitSelector: "#project-kanban", Page: "kanban", Variant: "healthy", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "kanban-empty-lanes", Route: "/projects/agent-lab/kanban", WaitSelector: "#project-kanban", Page: "kanban", Variant: "project-empty", ProjectID: "agent-lab", KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-dense-overflow", Route: "/projects/dogfood/kanban", WaitSelector: "#project-kanban", Page: "kanban", Variant: "dense-kanban", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-transition-blocked", Route: "/projects/dogfood/kanban", WaitSelector: "#project-kanban", Page: "kanban", Variant: "transition-blocked", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-terminal-states", Route: "/projects/dogfood/kanban", WaitSelector: "#project-kanban", Page: "kanban", Variant: "terminal", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "runs-active-work", Route: "/projects/dogfood/runs", WaitSelector: "#snapshot", Page: "runs", Variant: "healthy", ProjectID: demoPrimaryProjectID},
		{ID: "runs-tracker-refresh-gap", Route: "/projects/dogfood/runs", WaitSelector: "#snapshot", Page: "runs", Variant: "tracker-refresh-gap", ProjectID: demoPrimaryProjectID},
		{ID: "runs-idle", Route: "/projects/agent-lab/runs", WaitSelector: "#snapshot", Page: "runs", Variant: "project-empty", ProjectID: "agent-lab"},
		{ID: "runs-backoff-heavy", Route: "/projects/dogfood/runs", WaitSelector: "#snapshot", Page: "runs", Variant: "backoff-heavy", ProjectID: demoPrimaryProjectID},
		{ID: "runs-blocked-heavy", Route: "/projects/billing-api/runs", WaitSelector: "#snapshot", Page: "runs", Variant: "blocked-heavy", ProjectID: "billing-api"},
		{ID: "runs-long-content", Route: "/projects/dogfood/runs", WaitSelector: "#snapshot", Page: "runs", Variant: "long-content", ProjectID: demoPrimaryProjectID},
		{ID: "diagnostics-healthy", Route: "/projects/dogfood/diagnostics", WaitSelector: "#snapshot", Page: "diagnostics", Variant: "healthy", ProjectID: demoPrimaryProjectID},
		{ID: "diagnostics-budget-refusals", Route: "/projects/dogfood/diagnostics", WaitSelector: "#snapshot", Page: "diagnostics", Variant: "budget-refusals", ProjectID: demoPrimaryProjectID},
		{ID: "diagnostics-rate-limit-pressure", Route: "/projects/dogfood/diagnostics", WaitSelector: "#snapshot", Page: "diagnostics", Variant: "overloaded", ProjectID: demoPrimaryProjectID},
		{ID: "diagnostics-no-history", Route: "/projects/agent-lab/diagnostics", WaitSelector: "#snapshot", Page: "diagnostics", Variant: "no-history", ProjectID: "agent-lab"},
		{ID: "diagnostics-degraded", Route: "/projects/dogfood/diagnostics", WaitSelector: "#snapshot", Page: "diagnostics", Variant: "degraded", ProjectID: demoPrimaryProjectID},
		{ID: "settings-loaded-fleet", Route: "/settings", WaitSelector: "main", Page: "settings", Variant: "healthy"},
		{ID: "settings-empty-registry", Route: "/settings", WaitSelector: "main", Page: "settings", Variant: "settings-empty"},
		{ID: "settings-long-paths", Route: "/settings", WaitSelector: "main", Page: "settings", Variant: "settings-long-paths"},
		{ID: "settings-project-context", Route: "/projects/dogfood/configuration", WaitSelector: "main", Page: "settings", Variant: "healthy", ProjectID: demoPrimaryProjectID},
		{ID: "settings-missing-runtime-values", Route: "/settings", WaitSelector: "main", Page: "settings", Variant: "settings-missing"},
		{ID: "reports-empty-ledger", Route: "/reports", WaitSelector: "main", Page: "reports", Variant: "reports-empty"},
		{ID: "reports-normal-window", Route: "/reports", WaitSelector: "main", Page: "reports", Variant: "healthy"},
		{ID: "reports-high-spend-day", Route: "/reports", WaitSelector: "main", Page: "reports", Variant: "hot-path"},
		{ID: "reports-model-heavy", Route: "/reports", WaitSelector: "main", Page: "reports", Variant: "model-heavy"},
		{ID: "reports-filtered-project", Route: "/reports", WaitSelector: "main", Page: "reports", Variant: "filtered-project", ProjectID: demoPrimaryProjectID},
		{ID: "reports-invalid-date-range", Route: "/reports", WaitSelector: "body", Page: "reports", Variant: "invalid-date-range", Status: http.StatusBadRequest},
		{ID: "onboarding-tracker-choice", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "tracker"},
		{ID: "onboarding-github-credentials", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "credentials"},
		{ID: "onboarding-project-selection", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "project"},
		{ID: "onboarding-agent-config", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "agent"},
		{ID: "onboarding-write-summary", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "write"},
		{ID: "onboarding-validation-errors", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "validation-errors"},
		{ID: "onboarding-write-exists", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "write-exists"},
		{ID: "onboarding-write-success", Route: "/onboarding", WaitSelector: "#onboarding-step", Page: "onboarding", Variant: "write-success"},
		{ID: "api-kanban-move-dialog", Route: "/api/v1/kanban/move", Method: http.MethodGet, WaitSelector: "#kanban-dialog-content", Page: "api", Variant: "kanban-move-dialog", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-move-preselected-target", Route: "/api/v1/kanban/move", Method: http.MethodGet, WaitSelector: "#kanban-dialog-content", Page: "api", Variant: "kanban-move-dialog", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-move-missing-target", Route: "/api/v1/kanban/move", Method: http.MethodGet, WaitSelector: "#kanban-dialog-content", Page: "api", Variant: "kanban-move-missing-target", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-move-read-only", Route: "/api/v1/kanban/move", Method: http.MethodGet, WaitSelector: "#kanban-dialog-content", Page: "api", Variant: "kanban-read-only", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "api-kanban-move-success", Route: "/api/v1/kanban/move", Method: http.MethodPost, WaitSelector: "#kanban-feedback", Page: "api", Variant: "kanban-move-success", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-move-transition-blocked", Route: "/api/v1/kanban/move", Method: http.MethodPost, WaitSelector: "#kanban-feedback", Page: "api", Variant: "kanban-transition-blocked", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-move-connector-failure", Route: "/api/v1/kanban/move", Method: http.MethodPost, WaitSelector: "#kanban-feedback", Page: "api", Variant: "connector-failure", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-comment-issue-dialog", Route: "/api/v1/kanban/comment", Method: http.MethodGet, WaitSelector: "#kanban-dialog-content", Page: "api", Variant: "kanban-comment-issue", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-comment-pr-dialog", Route: "/api/v1/kanban/comment", Method: http.MethodGet, WaitSelector: "#kanban-dialog-content", Page: "api", Variant: "kanban-comment-pr", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-comment-invalid-target", Route: "/api/v1/kanban/comment", Method: http.MethodGet, WaitSelector: "#kanban-dialog-content", Page: "api", Variant: "kanban-comment-invalid-target", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-comment-success", Route: "/api/v1/kanban/comment", Method: http.MethodPost, WaitSelector: "#kanban-feedback", Page: "api", Variant: "kanban-comment-success", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-comment-empty-body", Route: "/api/v1/kanban/comment", Method: http.MethodPost, WaitSelector: "#kanban-feedback", Page: "api", Variant: "kanban-comment-empty-body", ProjectID: demoPrimaryProjectID},
		{ID: "api-kanban-comment-connector-failure", Route: "/api/v1/kanban/comment", Method: http.MethodPost, WaitSelector: "#kanban-feedback", Page: "api", Variant: "connector-failure", ProjectID: demoPrimaryProjectID},
		{ID: "api-refresh-accepted", Route: "/api/v1/refresh", Method: http.MethodPost, WaitSelector: "body", Page: "api", Variant: "refresh-accepted"},
		{ID: "api-refresh-unavailable", Route: "/api/v1/refresh", Method: http.MethodPost, WaitSelector: "body", Page: "api", Variant: "refresh-unavailable", Status: http.StatusServiceUnavailable},
		{ID: "api-state-full-snapshot", Route: "/api/v1/state", WaitSelector: "body", Page: "api", Variant: "healthy"},
		{ID: "api-state-no-snapshot", Route: "/api/v1/state", WaitSelector: "body", Page: "api", Variant: "empty"},
		{ID: "api-state-draining", Route: "/api/v1/state", WaitSelector: "body", Page: "api", Variant: "draining"},
		{ID: "api-state-project-scoped", Route: "/api/v1/projects/dogfood/state", WaitSelector: "body", Page: "api", Variant: "healthy", ProjectID: demoPrimaryProjectID},
		{ID: "api-timeseries-populated", Route: "/api/v1/timeseries?window=10m&bucket=1m", WaitSelector: "body", Page: "api", Variant: "healthy"},
		{ID: "api-timeseries-empty", Route: "/api/v1/timeseries?window=10m&bucket=1m", WaitSelector: "body", Page: "api", Variant: "project-empty"},
		{ID: "api-timeseries-invalid-query", Route: "/api/v1/timeseries?window=nope", WaitSelector: "body", Page: "api", Variant: "invalid-query", Status: http.StatusBadRequest},
		{ID: "api-usage-populated", Route: "/api/v1/usage?by=day", WaitSelector: "body", Page: "api", Variant: "healthy"},
		{ID: "api-usage-empty", Route: "/api/v1/usage?by=day", WaitSelector: "body", Page: "api", Variant: "reports-empty"},
		{ID: "api-usage-invalid-range", Route: "/api/v1/usage?by=day&from=2026-06-16&to=2026-06-15", WaitSelector: "body", Page: "api", Variant: "invalid-date-range", Status: http.StatusBadRequest},
		{ID: "api-issue-running", Route: "/api/v1/digitaldrywood/detent-core%235260", WaitSelector: "body", Page: "api", Variant: "healthy"},
		{ID: "api-issue-queued", Route: "/api/v1/digitaldrywood/docs-site%235270", WaitSelector: "body", Page: "api", Variant: "healthy"},
		{ID: "api-issue-blocked", Route: "/api/v1/digitaldrywood/billing-api%235280", WaitSelector: "body", Page: "api", Variant: "healthy"},
		{ID: "api-issue-not-found", Route: "/api/v1/digitaldrywood/detent-core%239999", WaitSelector: "body", Page: "api", Variant: "healthy", Status: http.StatusNotFound},
		{ID: "health-running-demo", Route: "/health", WaitSelector: "body", Page: "api", Variant: "healthy"},
		{ID: "events-frozen", Route: "/events", WaitSelector: "#snapshot", Page: "api", Variant: "healthy"},
		{ID: "events-play", Route: "/events", WaitSelector: "#snapshot", Page: "api", Variant: "play"},
	}
}

func demoKeySelectors(def demoScenario) []string {
	switch def.Page {
	case "fleet":
		return []string{"#snapshot", "[aria-label=\"Dashboard health\"]", "[aria-label=\"Project overview\"]"}
	case "fleet-kanban":
		return []string{"#fleet-kanban", "[data-project-kanban-card]", "[data-project-kanban-visibility-menu]"}
	case "kanban":
		return []string{"#project-kanban", "[data-kanban-card]", "[data-project-kanban-visibility-menu]"}
	case "reports":
		return []string{"main", "[aria-label=\"Usage reports\"]"}
	case "settings":
		return []string{"main", "[aria-label=\"Settings\"]"}
	case "onboarding":
		return []string{"#onboarding-step"}
	default:
		return []string{def.WaitSelector}
	}
}

func (d *demoScenarioSet) scenario(r *http.Request) (demoScenario, bool, bool) {
	if d == nil || r == nil {
		return demoScenario{}, false, false
	}
	id := strings.TrimSpace(r.Header.Get(DemoScenarioHeader))
	if id == "" {
		return demoScenario{}, false, false
	}
	scenario, ok := d.scenarios[id]
	return scenario, ok, !ok
}

func (s *Server) demoScenarioOrError(c echo.Context) (demoScenario, bool, error) {
	if s.demo == nil {
		return demoScenario{}, false, nil
	}
	scenario, ok, unknown := s.demo.scenario(c.Request())
	if unknown {
		if err := c.JSON(http.StatusNotFound, errorResponse("demo_scenario_not_found", "Demo scenario not found")); err != nil {
			return demoScenario{}, false, err
		}
		return demoScenario{}, false, errDemoScenarioResponseWritten
	}
	return scenario, ok, nil
}

func (s *Server) apiDemoScenarios(c echo.Context) error {
	if s.demo == nil {
		return c.JSON(http.StatusNotFound, errorResponse("demo_scenarios_unavailable", "Demo scenarios are not enabled"))
	}
	return c.JSON(http.StatusOK, demoScenariosResponse{
		GeneratedAt: demoBaseTime,
		Header:      DemoScenarioHeader,
		Clock:       s.demo.clock,
		Scenarios:   append([]DemoScenarioManifest(nil), s.demo.manifest...),
	})
}

func (s *Server) demoDashboard(c echo.Context, scenario demoScenario) error {
	data := s.demoDashboardData(c.Request().Context(), scenario)
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	return render(c, templates.Dashboard(data))
}

func (s *Server) demoFleetKanban(c echo.Context, scenario demoScenario) error {
	data := s.demoDashboardData(c.Request().Context(), scenario)
	data.ActiveNav = "kanban"
	data.Title = instancePageTitle(s.instanceName(), "Kanban - Detent")
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	return render(c, templates.ProjectKanbanPage(data))
}

func (s *Server) demoProjectDashboard(c echo.Context, scenario demoScenario, view string) error {
	if scenario.Status == http.StatusNotFound || scenario.Variant == "not-found" {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	data, ok := s.demoProjectDashboardData(c.Request().Context(), scenario)
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	switch view {
	case "kanban":
		data.ActiveNav = "kanban"
		data.Title = s.projectPageTitle(data, "Kanban")
		return render(c, templates.ProjectKanbanPage(data))
	case "runs":
		data.ActiveNav = "runs"
		data.Title = s.projectPageTitle(data, "Runs")
		return render(c, templates.ProjectRunsPage(data))
	case "diagnostics":
		data.ActiveNav = "diagnostics"
		data.Title = s.projectPageTitle(data, "Diagnostics")
		return render(c, templates.ProjectDiagnosticsPage(data))
	case "configuration":
		settingsData := s.demoSettingsData(c.Request().Context(), scenario, scenario.ProjectID)
		settingsData.ActiveNav = "configuration"
		settingsData.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
		return render(c, templates.Settings(settingsData))
	default:
		return render(c, templates.Dashboard(data))
	}
}

func (s *Server) demoReports(c echo.Context, scenario demoScenario) error {
	if scenario.Variant == "invalid-date-range" {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_date_range", "from must be on or before to"))
	}
	if scenario.Variant == "reports-empty" {
		data := s.demoEmptyReportsData(c.Request().Context(), scenario)
		data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
		return render(c, templates.Reports(data))
	}
	projectID := ""
	if scenario.Variant == "filtered-project" {
		projectID = scenario.ProjectID
	}
	data, err := s.reportsData(c.Request().Context(), time.Time{}, time.Time{}, projectID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse("usage_reports_failed", "Usage reports failed"))
	}
	data.GeneratedAt = demoBaseTime
	data.Projects = demoProjectsForVariant(scenario.Variant)
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	return render(c, templates.Reports(data))
}

func (s *Server) demoSettings(c echo.Context, scenario demoScenario, selectedProjectID string) error {
	data := s.demoSettingsData(c.Request().Context(), scenario, selectedProjectID)
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	return render(c, templates.Settings(data))
}

func (s *Server) demoOnboarding(c echo.Context, scenario demoScenario) error {
	return render(c, templates.OnboardingPage(s.demoOnboardingData(scenario)))
}

func (s *Server) demoDashboardData(ctx context.Context, scenario demoScenario) templates.DashboardData {
	instanceName := s.instanceName()
	snapshot := demoSnapshotForScenario(scenario)
	return templates.DashboardData{
		Title:           instancePageTitle(instanceName, "Detent"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Build:           s.build,
		ConnectorName:   s.connector.Name(),
		DashboardURL:    s.dashboardURL,
		Snapshot:        snapshot,
		Projects:        demoProjectsForVariant(scenario.Variant),
		Kanban:          demoKanbanData(scenario, ""),
		Assets:          s.assets.templatePaths(),
		ActiveNav:       "fleet",
	}
}

func (s *Server) demoProjectDashboardData(ctx context.Context, scenario demoScenario) (templates.DashboardData, bool) {
	projectID := strings.TrimSpace(scenario.ProjectID)
	if projectID == "" {
		projectID = demoPrimaryProjectID
	}
	snapshot := demoSnapshotForScenario(scenario)
	projects := demoProjectsForVariant(scenario.Variant)
	project, ok := demoProjectByID(projects, projectID)
	if !ok {
		return templates.DashboardData{}, false
	}
	scoped := projectScopedSnapshotForProject(snapshot, telemetry.Project{
		ID:          project.ID,
		DisplayName: project.Name,
		URL:         project.URL,
		Color:       project.Color,
	})
	instanceName := s.instanceName()
	return templates.DashboardData{
		Title:           instancePageTitle(instanceName, project.Name+" - Detent"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Build:           s.build,
		ConnectorName:   s.connector.Name(),
		DashboardURL:    s.dashboardURL,
		Snapshot:        scoped,
		Projects:        projects,
		Kanban:          demoKanbanData(scenario, project.ID),
		Assets:          s.assets.templatePaths(),
		ActiveNav:       "project",
		ProjectID:       project.ID,
		ProjectName:     project.Name,
		ProjectPaused:   project.Paused,
	}, true
}

func demoKanbanData(scenario demoScenario, projectID string) templates.KanbanData {
	mode := strings.TrimSpace(scenario.KanbanMode)
	if mode == "" {
		mode = workflowconfig.KanbanModeIntegration
	}
	if projectID == "" {
		mode = workflowconfig.KanbanModeReadOnly
	}
	states := []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework", "Merging", "Done", "Cancelled"}
	return templates.KanbanData{
		Mode:               mode,
		ProjectID:          projectID,
		States:             states,
		TerminalStates:     []string{"Done", "Cancelled"},
		AllowedTransitions: demoKanbanTransitions(states),
	}
}

func (s *Server) demoKanbanMoveSuccess(c echo.Context, scenario demoScenario) error {
	req, _, _ := parseKanbanMoveRequest(c)
	if req.projectID == "" {
		req.projectID = strings.TrimSpace(scenario.ProjectID)
	}
	if req.projectID == "" {
		req.projectID = demoPrimaryProjectID
	}
	if req.issueID == "" {
		req.issueID = "demo-backlog"
	}
	if req.targetState == "" {
		req.targetState = "Todo"
	}
	message := "Moved card to " + req.targetState + "."

	projectScenario := scenario
	projectScenario.Page = "kanban"
	projectScenario.ProjectID = req.projectID
	projectScenario.KanbanMode = workflowconfig.KanbanModeIntegration
	data, ok := s.demoProjectDashboardData(c.Request().Context(), projectScenario)
	if !ok {
		return kanbanFeedback(c, http.StatusOK, message)
	}
	applyDemoKanbanMove(&data.Snapshot, req.projectID, req.issueID, req.targetState)
	if !req.drag {
		data.Kanban.Feedback = message
		data.Kanban.FeedbackKind = "success"
	}

	if c.Request().Header.Get("HX-Request") != "true" {
		return c.JSON(http.StatusOK, map[string]any{"ok": true, "message": message})
	}
	c.Response().Header().Set("HX-Trigger", kanbanDialogSucceeded)
	c.Response().Header().Set("HX-Retarget", kanbanProjectBoardTarget)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return render(c, templates.ProjectKanbanSnapshot(data))
}

func applyDemoKanbanMove(snapshot *telemetry.Snapshot, projectID string, issueID string, targetState string) {
	applySnapshotKanbanIssues(snapshot, func(issue *telemetry.Issue) {
		if issue == nil || !sameKanbanIssue(*issue, projectID, issueID, snapshot.Project.ID) {
			return
		}
		issue.State = targetState
	})
}

func demoKanbanTransitions(states []string) map[string][]string {
	transitions := map[string][]string{
		"Backlog":      {"Todo", "Cancelled"},
		"Todo":         {"In Progress", "Blocked", "Cancelled"},
		"In Progress":  {"Blocked", "Human Review", "Rework", "Cancelled"},
		"Blocked":      {"Todo", "In Progress", "Rework", "Cancelled"},
		"Human Review": {"Rework", "Merging", "Blocked", "Cancelled"},
		"Rework":       {"In Progress", "Human Review", "Blocked", "Cancelled"},
		"Merging":      {"Done", "Blocked", "Rework", "Cancelled"},
		"Done":         {},
		"Cancelled":    {"Backlog"},
	}
	out := make(map[string][]string, len(states))
	for _, state := range states {
		out[state] = append([]string(nil), transitions[state]...)
	}
	return out
}

func (s *Server) demoSettingsData(ctx context.Context, scenario demoScenario, selectedProjectID string) templates.SettingsData {
	instanceName := s.instanceName()
	projects := demoSettingsProjects(scenario.Variant)
	sidebarProjects := demoProjectsForVariant(scenario.Variant)
	projectID, projectName := "", ""
	if selectedProjectID != "" {
		if project, ok := demoProjectByID(sidebarProjects, selectedProjectID); ok {
			projectID = project.ID
			projectName = project.Name
		}
	}
	runtime := templates.SettingsRuntime{
		DBPath:        "/tmp/detent-screenshots/detent.db",
		LogPath:       "/tmp/detent-screenshots/detent.log",
		ServerAddress: "127.0.0.1:0",
	}
	globalPath := "/tmp/detent-screenshots/global.yaml"
	if scenario.Variant == "settings-long-paths" {
		globalPath = "/Users/example/Library/Application Support/Detent/screenshot-demo/very/long/configuration/root/global.yaml"
		runtime.DBPath = "/Users/example/Library/Application Support/Detent/screenshot-demo/very/long/runtime/database/detent.db"
		runtime.LogPath = "/Users/example/Library/Logs/Detent/screenshot-demo/very/long/runtime/detent.log"
	}
	if scenario.Variant == "settings-missing" {
		runtime = templates.SettingsRuntime{}
		globalPath = ""
	}
	return templates.SettingsData{
		Title:           instancePageTitle(instanceName, "Detent settings"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Global: templates.SettingsGlobal{
			ConfigPath: globalPath,
			PathRule:   string(globalconfig.PathRuleFlag),
		},
		Projects:        projects,
		Runtime:         runtime,
		Assets:          s.assets.templatePaths(),
		SidebarProjects: sidebarProjects,
		ActiveNav:       "settings",
		ProjectID:       projectID,
		ProjectName:     projectName,
	}
}

func demoSettingsProjects(variant string) []templates.SettingsProject {
	if variant == "settings-empty" {
		return nil
	}
	projects := []templates.SettingsProject{
		{ID: demoPrimaryProjectID, WorkflowPath: "/tmp/detent-screenshots/WORKFLOW.md", Workdir: "/tmp/detent-screenshots/source/detent-core", WorktreeRoot: "/tmp/detent-screenshots/workspaces/detent-core", Weight: 120, Priority: 100, TrackerKind: "memory", TrackerProject: "digitaldrywood/detent-core", DependencyAutoUnblock: "enabled: Blocked -> Todo when terminal_or_merged"},
		{ID: "docs-site", WorkflowPath: "/tmp/detent-screenshots/WORKFLOW.md", Workdir: "/tmp/detent-screenshots/source/docs-site", WorktreeRoot: "/tmp/detent-screenshots/workspaces/docs-site", Weight: 90, Priority: 80, TrackerKind: "memory", TrackerProject: "digitaldrywood/docs-site", DependencyAutoUnblock: "disabled: n/a -> n/a when terminal_or_merged"},
		{ID: "billing-api", WorkflowPath: "/tmp/detent-screenshots/WORKFLOW.md", Workdir: "/tmp/detent-screenshots/source/billing-api", WorktreeRoot: "/tmp/detent-screenshots/workspaces/billing-api", Weight: 80, Priority: 95, TrackerKind: "memory", TrackerProject: "digitaldrywood/billing-api", DependencyAutoUnblock: "enabled: Blocked -> Rework when terminal_or_merged"},
		{ID: "mobile-client", WorkflowPath: "/tmp/detent-screenshots/WORKFLOW.md", Workdir: "/tmp/detent-screenshots/source/mobile-client", WorktreeRoot: "/tmp/detent-screenshots/workspaces/mobile-client", Weight: 40, Priority: 20, Paused: true, TrackerKind: "memory", TrackerProject: "digitaldrywood/mobile-client", DependencyAutoUnblock: "disabled: n/a -> n/a when terminal_or_merged"},
	}
	if variant == "settings-long-paths" {
		for i := range projects {
			projects[i].WorkflowPath = "/Users/example/Library/Application Support/Detent/screenshot-demo/very/long/project/" + projects[i].ID + "/workflow/WORKFLOW.md"
			projects[i].Workdir = "/Users/example/Development/digitaldrywood/products/detent/screenshot-demo/source/" + projects[i].ID + "/with/a/deep/path"
			projects[i].WorktreeRoot = "/Users/example/Development/digitaldrywood/products/detent/screenshot-demo/worktrees/" + projects[i].ID + "/nested/workspaces"
		}
	}
	if variant == "settings-missing" {
		for i := range projects {
			projects[i].WorkflowPath = ""
			projects[i].Workdir = ""
			projects[i].WorktreeRoot = ""
			projects[i].TrackerProject = ""
		}
	}
	return projects
}

func (s *Server) demoEmptyReportsData(ctx context.Context, scenario demoScenario) templates.ReportsData {
	instanceName := s.instanceName()
	empty := templates.UsageReportData{By: "day"}
	return templates.ReportsData{
		Title:           instancePageTitle(instanceName, "Detent reports"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		ConnectorName:   s.connector.Name(),
		GeneratedAt:     demoBaseTime,
		Day:             empty,
		Project:         templates.UsageReportData{By: "project"},
		Issue:           templates.UsageReportData{By: "issue"},
		PR:              templates.UsageReportData{By: "pr"},
		Model:           templates.UsageReportData{By: "model"},
		Assets:          s.assets.templatePaths(),
		Projects:        demoProjectsForVariant(scenario.Variant),
		ActiveNav:       "reports",
	}
}

func (s *Server) demoOnboardingData(scenario demoScenario) templates.OnboardingData {
	form := templates.OnboardingForm{
		Step:                         onboardingStepTracker,
		TrackerKind:                  workflowconfig.TrackerGitHub,
		Endpoint:                     "https://api.github.test",
		APIKey:                       "$GITHUB_TOKEN",
		ProjectSlug:                  "digitaldrywood/detent-core",
		Repo:                         "digitaldrywood/detent-core",
		WorkspaceRoot:                "/tmp/detent-screenshots/workspaces",
		MaxConcurrentAgents:          "6",
		MaxTurns:                     "24",
		PollingIntervalMS:            "60000",
		MergingConcurrency:           "1",
		DispatchPriorityState:        "Merging\nRework\nTodo",
		DispatchPriorityLabel:        "stage:s6\nobservability",
		DependencyAutoUnblockEnabled: "true",
	}
	var errors []string
	result := templates.OnboardingResult{}
	switch scenario.Variant {
	case "credentials":
		form.Step = onboardingStepCredentials
	case "project":
		form.Step = onboardingStepProject
	case "agent":
		form.Step = onboardingStepAgent
	case "write":
		form.Step = onboardingStepWrite
	case "validation-errors":
		form.Step = onboardingStepProject
		form.Endpoint = "not a url"
		form.Repo = "digitaldrywood"
		errors = []string{"endpoint must be an absolute HTTP URL", "repo must look like owner/name", "max agents must be a positive integer"}
	case "write-exists":
		form.Step = onboardingStepWrite
		result = templates.OnboardingResult{Kind: "exists", Message: "WORKFLOW.md already exists."}
	case "write-success":
		form.Step = onboardingStepWrite
		result = templates.OnboardingResult{Kind: "success", Message: "Wrote WORKFLOW.md."}
	default:
		form.Step = onboardingStepTracker
	}
	instanceName := s.instanceName()
	return templates.OnboardingData{
		Title:           instancePageTitle(instanceName, "Detent onboarding"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		WorkflowPath:    "WORKFLOW.md",
		Step:            form.Step,
		Form:            form,
		Errors:          errors,
		Result:          result,
		Assets:          s.assets.templatePaths(),
		Polling:         templates.PollingData{MinIntervalMS: minPollingIntervalMS},
	}
}

func demoProjectByID(projects []templates.ProjectSmallMultiple, id string) (templates.ProjectSmallMultiple, bool) {
	for _, project := range projects {
		if strings.TrimSpace(project.ID) == strings.TrimSpace(id) {
			return project, true
		}
	}
	return templates.ProjectSmallMultiple{}, false
}

func demoSnapshotForScenario(scenario demoScenario) telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	switch scenario.Variant {
	case "empty":
		snapshot = demoEmptySnapshot()
		snapshot.GeneratedAt = time.Time{}
		snapshot.Project = telemetry.Project{}
		snapshot.Projects = nil
	case "project-empty", "reports-empty", "settings-empty", "no-history":
		snapshot = demoEmptySnapshot()
	case "overloaded", "backoff-heavy":
		snapshot = demoOverloadedSnapshot()
	case "draining":
		snapshot = demoDrainingSnapshot()
	case "degraded":
		snapshot = demoDegradedSnapshot()
	case "budget-refusals":
		snapshot = demoBudgetRefusalsSnapshot()
	case "blocked-heavy":
		snapshot = demoBlockedHeavySnapshot()
	case "long-content":
		snapshot = demoLongContentSnapshot()
	case "dense", "dense-kanban":
		snapshot = demoDenseSnapshot()
	case "hot-path", "model-heavy", "filtered-project":
		snapshot = demoHotPathSnapshot()
	case "tracker-refresh-gap":
		snapshot = demoTrackerRefreshGapSnapshot()
	case "startup-loading":
		snapshot = demoStartupLoadingSnapshot()
	case "github-api-healthy":
		snapshot = demoGitHubAPIHealthySnapshot()
	case "github-api-warning":
		snapshot = demoGitHubAPIWarningSnapshot()
	case "github-api-secondary-backoff":
		snapshot = demoGitHubAPISecondaryBackoffSnapshot()
	case "github-api-primary-exhausted":
		snapshot = demoGitHubAPIPrimaryExhaustedSnapshot()
	}
	if scenario.Variant == "terminal" {
		snapshot.BoardIssues = append(snapshot.BoardIssues, demoIssue(demoPrimaryProjectID, "demo-cancelled", "digitaldrywood/detent-core#5259", "Cancelled alternate dashboard theme", "Cancelled", 48))
	}
	if scenario.ProjectID != "" && scenario.ProjectID != demoPrimaryProjectID && scenario.Variant == "project-empty" {
		snapshot.Projects = demoProjectSnapshots(demoProjectsForVariant("project-empty"))
	}
	return snapshot
}

func demoHealthySnapshot() telemetry.Snapshot {
	now := demoBaseTime
	dayMax := 42.0
	issueMax := 8.0
	leaseRenewed := now.Add(-90 * time.Second)
	leaseExpires := now.Add(9 * time.Minute)
	nextRefresh := now.Add(45 * time.Second)
	lastRefresh := now.Add(-15 * time.Second)
	snapshot := telemetry.Snapshot{
		GeneratedAt:  now,
		Project:      telemetry.Project{DisplayName: "multiple projects"},
		Instance:     telemetry.Instance{Name: "detent-demo-screenshots", GitHubLogin: "detent-bot", AuthorizationScope: "repo, read:project", AuthorizationConfigured: true},
		Projects:     demoProjectSnapshots(demoProjectsForVariant("healthy")),
		DashboardURL: "http://localhost:0",
		Shutdown:     telemetry.Shutdown{Status: "running"},
		Refresh:      telemetry.Refresh{PollIntervalSeconds: 60, LastRefreshAt: &lastRefresh, NextRefreshAt: &nextRefresh},
		Counts:       telemetry.Counts{Running: 3, Queue: 3, Blocked: 2, Completed: 4},
		Events: []telemetry.ActivityEvent{
			{
				At:      now.Add(-3 * time.Minute),
				Event:   "workspace_reap_succeeded",
				Message: "workspace cleanup succeeded for digitaldrywood/mobile-client#5243 reason=cancelled worktrees=1 branches=1 processes=0",
			},
		},
		BoardIssues: []telemetry.Issue{
			demoIssue(demoPrimaryProjectID, "demo-backlog", "digitaldrywood/detent-core#5250", "Backlog observability fixture intake", "Backlog", 72),
			demoIssue(demoPrimaryProjectID, "demo-todo", "digitaldrywood/detent-core#5251", "Add screenshot manifest smoke test", "Todo", 9),
			demoIssue("agent-lab", "agent-lab-todo", "digitaldrywood/agent-lab#111", "Try secondary runner routing", "Todo", 11),
		},
		Pipeline: []telemetry.Issue{
			demoPipelineIssue(demoPrimaryProjectID, "demo-review", "digitaldrywood/detent-core#5290", "Review deterministic chart colors", "Human Review", 5290, "success", "clean", 2),
			demoPipelineIssue(demoPrimaryProjectID, "demo-rework", "digitaldrywood/detent-core#5291", "Address visual diff finding", "Rework", 5291, "failure", "dirty", 12),
			demoPipelineIssue("release-train", "demo-merging", "digitaldrywood/release-train#5292", "Merge release readiness bundle", "Merging", 5292, "pending", "clean", 1),
			demoPipelineIssue(demoPrimaryProjectID, "demo-done-pr", "digitaldrywood/detent-core#5293", "Ship completed PR lane fixture", "Done", 5293, "success", "clean", 24),
		},
		Running: []telemetry.Running{
			{
				Issue:           demoIssue(demoPrimaryProjectID, "demo-running-core", "digitaldrywood/detent-core#5260", "Implement page-addressable screenshot scenarios", "In Progress", 1),
				WorkerHost:      "demo-worker-a",
				ProcessIdentity: "pid-5260",
				WorkspacePath:   "/tmp/detent-screenshots/workspaces/detent-core/5260",
				SessionID:       "thread-demo-core-5260",
				TurnCount:       7,
				StartedAt:       now.Add(-34 * time.Minute),
				LastEventAt:     demoTimePtr(now.Add(-2 * time.Minute)),
				LastEvent:       "agent_message",
				LastMessage:     "Rendered manifest and route smoke checks.",
				RecentEvents: []telemetry.ActivityEvent{
					{At: now.Add(-4 * time.Minute), Event: "tool_call", Message: "go test ./internal/web"},
					{At: now.Add(-2 * time.Minute), Event: "agent_message", Message: "Rendered manifest and route smoke checks."},
				},
				RuntimeSeconds: 2040,
				DiffAdded:      812,
				DiffRemoved:    96,
				DiffFiles:      9,
				DiffStatus:     "ok",
				Tokens:         telemetry.Tokens{Input: 38240, Output: 12840, Total: 51080, RuntimeSeconds: 2040},
			},
			{
				Issue:           demoIssue("docs-site", "demo-running-docs", "digitaldrywood/docs-site#5261", "Write direct loading documentation examples", "In Progress", 2),
				WorkerHost:      "demo-worker-b",
				ProcessIdentity: "pid-5261",
				WorkspacePath:   "/tmp/detent-screenshots/workspaces/docs-site/5261",
				SessionID:       "thread-demo-docs-5261",
				TurnCount:       4,
				StartedAt:       now.Add(-18 * time.Minute),
				LastEventAt:     demoTimePtr(now.Add(-90 * time.Second)),
				LastEvent:       "token_usage",
				LastMessage:     "21,450 total tokens (15,200 in, 6,250 out)",
				RuntimeSeconds:  1080,
				DiffAdded:       210,
				DiffRemoved:     18,
				DiffFiles:       3,
				DiffStatus:      "ok",
				Tokens:          telemetry.Tokens{Input: 15200, Output: 6250, Total: 21450, RuntimeSeconds: 1080},
			},
			{
				Issue:           demoIssue("infra-platform", "demo-running-infra", "digitaldrywood/infra-platform#5262", "Verify isolated runtime paths on ephemeral ports", "In Progress", 3),
				WorkerHost:      "demo-worker-c",
				ProcessIdentity: "pid-5262",
				WorkspacePath:   "/tmp/detent-screenshots/workspaces/infra-platform/5262",
				SessionID:       "thread-demo-infra-5262",
				TurnCount:       5,
				StartedAt:       now.Add(-21 * time.Minute),
				LastEventAt:     demoTimePtr(now.Add(-3 * time.Minute)),
				LastEvent:       "diff_stats",
				LastMessage:     "5 files changed, +164 -22",
				RuntimeSeconds:  1260,
				DiffAdded:       164,
				DiffRemoved:     22,
				DiffFiles:       5,
				DiffStatus:      "ok",
				Tokens:          telemetry.Tokens{Input: 21100, Output: 8100, Total: 29200, RuntimeSeconds: 1260},
			},
		},
		Queue: []telemetry.Queued{
			demoQueued("docs-site", "demo-queued-docs", "digitaldrywood/docs-site#5270", "Capture reports screenshots", 1, now.Add(6*time.Minute), "waiting for weighted fair share slot"),
			demoQueued("billing-api", "demo-queued-billing", "digitaldrywood/billing-api#5271", "Add budget refusal screenshot fixture", 2, now.Add(14*time.Minute), "previous attempt exceeded budget cap"),
			demoQueued("mobile-client", "demo-queued-mobile", "digitaldrywood/mobile-client#5272", "Exercise compact lane overflow on mobile", 3, now.Add(28*time.Minute), "project paused until release train clears"),
		},
		Blocked: []telemetry.Blocked{
			demoBlocked("billing-api", "demo-blocked-billing", "digitaldrywood/billing-api#5280", "Dependency issue waiting on ledger migration", "Depends on digitaldrywood/billing-api#5200", "Todo", 10),
			demoBlocked(demoPrimaryProjectID, "demo-blocked-hook", "digitaldrywood/detent-core#5281", "Workspace hook error needs operator input", "after_create hook exited 2", "operator", 5),
		},
		Completed: []telemetry.Completed{
			demoCompleted(demoPrimaryProjectID, "demo-complete-core", "digitaldrywood/detent-core#5240", "Complete dashboard density pass", "Done", 91, "gpt-5-codex", 64000),
			demoCompleted("docs-site", "demo-complete-docs", "digitaldrywood/docs-site#5241", "Publish screenshot capture guide", "Human Review", 57, "gpt-5-codex", 38000),
			demoCompleted("release-train", "demo-complete-release", "digitaldrywood/release-train#5242", "Prepare release note bundle", "Merging", 73, "gpt-5", 52000),
			demoCompleted("mobile-client", "demo-complete-mobile", "digitaldrywood/mobile-client#5243", "Cancel stale mobile board experiment", "Cancelled", 11, "gpt-5-mini", 9000),
		},
		Budget: telemetry.Budget{
			Enabled:           true,
			PerDayMaxUSD:      &dayMax,
			PerIssueMaxUSD:    &issueMax,
			CurrentSpendUSD:   18.72,
			ProjectedCostUSD:  5.44,
			ProjectedSpendUSD: 24.16,
			PeriodStart:       now.Truncate(24 * time.Hour),
			PeriodEnd:         now.Truncate(24 * time.Hour).Add(24 * time.Hour),
			SpendPoints:       demoBudgetSpendPoints(now, 18.72),
			Days:              demoBudgetDays(),
		},
		RateLimits: &telemetry.RateLimits{
			LimitID:       "codex-demo",
			LimitName:     "Codex demo pool",
			Primary:       &telemetry.RateLimitBucket{Remaining: 8200, Used: 1800, Limit: 10000, ResetAt: demoTimePtr(now.Add(48 * time.Minute)), ResetInSeconds: 2880},
			Secondary:     &telemetry.RateLimitBucket{Remaining: 460, Used: 40, Limit: 500, ResetAt: demoTimePtr(now.Add(8 * time.Minute)), ResetInSeconds: 480},
			Credits:       &telemetry.RateLimitBucket{HasCredits: true, Balance: "healthy"},
			GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 4320, Used: 680, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520},
			GitHubREST:    &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: demoTimePtr(now.Add(46 * time.Minute)), ResetInSeconds: 2760},
			GraphQLCost:   &telemetry.GraphQLCost{TotalQueries: 88, TotalCost: 680, Contributors: []telemetry.GraphQLCostContributor{{QueryType: "project_items", Count: 30, Cost: 300}, {QueryType: "pull_requests", Count: 18, Cost: 180}}},
			RESTUsage:     &telemetry.RESTUsage{TotalRequests: 122, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 82, Remaining: 4878, Limit: 5000}, {EndpointFamily: "pull requests", Count: 40, Remaining: 4878, Limit: 5000}}},
		},
		Tokens:          telemetry.Tokens{Input: 74540, Output: 27190, Total: 101730, RuntimeSeconds: 4380},
		Throughput:      telemetry.TokenThroughput{TokensPerSecond: 23.5, WindowSeconds: 600, Tokens: 14100},
		LifetimeTotals:  telemetry.LifetimeTotals{Available: true, InputTokens: 4410000, OutputTokens: 1390000, TotalTokens: 5800000, RuntimeSeconds: 242000, Sessions: 182, Runs: 37},
		CycleTime:       demoCycleTime(now),
		WorkflowMetrics: demoWorkflowMetrics(now),
		TokenTrend:      demoTokenTrend(now),
	}
	for i := range snapshot.Running {
		snapshot.Running[i].LeaseRenewedAt = &leaseRenewed
		snapshot.Running[i].LeaseExpiresAt = &leaseExpires
	}
	return snapshot
}

func demoEmptySnapshot() telemetry.Snapshot {
	now := demoBaseTime
	lastRefresh := now.Add(-15 * time.Second)
	nextRefresh := now.Add(time.Minute)
	return telemetry.Snapshot{
		GeneratedAt:     now,
		Project:         telemetry.Project{DisplayName: "multiple projects"},
		Instance:        telemetry.Instance{Name: "detent-demo-screenshots", GitHubLogin: "detent-bot", AuthorizationScope: "repo, read:project", AuthorizationConfigured: true},
		Projects:        demoProjectSnapshots(demoProjectsForVariant("project-empty")),
		DashboardURL:    "http://localhost:0",
		Shutdown:        telemetry.Shutdown{Status: "running"},
		Refresh:         telemetry.Refresh{PollIntervalSeconds: 60, Status: telemetry.RefreshStatusReady, LastRefreshAt: &lastRefresh, NextRefreshAt: &nextRefresh},
		LifetimeTotals:  telemetry.LifetimeTotals{Available: true},
		CycleTime:       telemetry.CycleTimeReport{Available: false, DegradedReason: "no completed sessions in the selected window"},
		WorkflowMetrics: demoEmptyWorkflowMetrics(now),
	}
}

func demoStartupLoadingSnapshot() telemetry.Snapshot {
	now := demoBaseTime
	nextRefresh := now.Add(20 * time.Second)
	refresh := telemetry.Refresh{PollIntervalSeconds: 60, Status: telemetry.RefreshStatusInitializing, NextRefreshAt: &nextRefresh}
	snapshot := telemetry.Snapshot{
		GeneratedAt:    now,
		Project:        telemetry.Project{DisplayName: "multiple projects"},
		Instance:       telemetry.Instance{Name: "detent-demo-screenshots", GitHubLogin: "detent-bot", AuthorizationScope: "repo, read:project", AuthorizationConfigured: true},
		Projects:       demoProjectSnapshots(demoProjectsForVariant("project-empty")),
		DashboardURL:   "http://localhost:0",
		Shutdown:       telemetry.Shutdown{Status: "running"},
		Refresh:        refresh,
		LifetimeTotals: telemetry.LifetimeTotals{Available: true},
	}
	for i := range snapshot.Projects {
		snapshot.Projects[i].Refresh = refresh
	}
	return snapshot
}

func demoOverloadedSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	dayMax := 42.0
	snapshot.Queue = append(snapshot.Queue,
		demoQueued(demoPrimaryProjectID, "demo-queued-overload-1", "digitaldrywood/detent-core#5273", "Retry overloaded visual comparison job", 4, now.Add(38*time.Minute), "secondary rate limit in effect"),
		demoQueued("infra-platform", "demo-queued-overload-2", "digitaldrywood/infra-platform#5274", "Re-run isolated port smoke test", 5, now.Add(50*time.Minute), "rate-limit retry budget is low"),
	)
	snapshot.Counts.Queue = len(snapshot.Queue)
	snapshot.Budget.CurrentSpendUSD = 39.85
	snapshot.Budget.ProjectedCostUSD = 4.9
	snapshot.Budget.ProjectedSpendUSD = 44.75
	snapshot.Budget.PerDayMaxUSD = &dayMax
	snapshot.RateLimits = &telemetry.RateLimits{
		LimitID:       "codex-demo-pressure",
		LimitName:     "Codex demo pool",
		Primary:       &telemetry.RateLimitBucket{Remaining: 260, Used: 9740, Limit: 10000, ResetAt: demoTimePtr(now.Add(21 * time.Minute)), ResetInSeconds: 1260},
		Secondary:     &telemetry.RateLimitBucket{Remaining: 4, Used: 496, Limit: 500, ResetAt: demoTimePtr(now.Add(9 * time.Minute)), ResetInSeconds: 540},
		Credits:       &telemetry.RateLimitBucket{HasCredits: true, Balance: "low"},
		GitHubGraphQL: &telemetry.RateLimitBucket{Remaining: 95, Used: 4905, Limit: 5000, ResetAt: demoTimePtr(now.Add(34 * time.Minute)), ResetInSeconds: 2040},
		GitHubREST:    &telemetry.RateLimitBucket{Remaining: 430, Used: 4570, Limit: 5000, ResetAt: demoTimePtr(now.Add(18 * time.Minute)), ResetInSeconds: 1080},
		GraphQLCost:   &telemetry.GraphQLCost{TotalQueries: 410, TotalCost: 4905, Contributors: []telemetry.GraphQLCostContributor{{QueryType: "project_items", Count: 180, Cost: 2700}, {QueryType: "review_threads", Count: 90, Cost: 1260}, {QueryType: "rate_limit_probe", Count: 40, Cost: 480}}},
		RESTUsage:     &telemetry.RESTUsage{TotalRequests: 4570, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 4100, Remaining: 430, Limit: 5000}, {EndpointFamily: "check runs", Count: 470, Remaining: 430, Limit: 5000}}},
	}
	return snapshot
}

func demoGitHubAPIHealthySnapshot() telemetry.Snapshot {
	return demoHealthySnapshot()
}

func demoGitHubAPIWarningSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.RateLimits.GitHubREST = &telemetry.RateLimitBucket{Remaining: 240, Used: 4760, Limit: 5000, ResetAt: demoTimePtr(now.Add(24 * time.Minute)), ResetInSeconds: 1440}
	snapshot.RateLimits.GitHubGraphQL = &telemetry.RateLimitBucket{Remaining: 3200, Used: 1800, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520}
	snapshot.RateLimits.RESTUsage = &telemetry.RESTUsage{TotalRequests: 4760, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 3900, Remaining: 240, Limit: 5000}, {EndpointFamily: "pull requests", Count: 860, Remaining: 240, Limit: 5000}}}
	return snapshot
}

func demoGitHubAPISecondaryBackoffSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	backoffUntil := now.Add(5 * time.Minute)
	snapshot.RateLimits.GitHubREST = &telemetry.RateLimitBucket{Remaining: 4878, Used: 122, Limit: 5000, ResetAt: demoTimePtr(now.Add(46 * time.Minute)), ResetInSeconds: 2760}
	snapshot.RateLimits.GitHubGraphQL = &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520}
	snapshot.RateLimits.RESTUsage = &telemetry.RESTUsage{
		TotalRequests: 122,
		RateLimited:   true,
		BackoffUntil:  &backoffUntil,
		Contributors: []telemetry.RESTUsageContributor{
			{EndpointFamily: "pull requests", Count: 2, RetryAfterMS: (5 * time.Minute).Milliseconds(), RateLimited: true, LastStatus: 429, Remaining: 4878, Limit: 5000},
			{EndpointFamily: "check runs", Count: 1, RateLimited: true, LastStatus: 429, Remaining: 4878, Limit: 5000},
		},
	}
	return snapshot
}

func demoGitHubAPIPrimaryExhaustedSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.RateLimits.GitHubREST = &telemetry.RateLimitBucket{Remaining: 0, Used: 5000, Limit: 5000, ResetAt: demoTimePtr(now.Add(30 * time.Minute)), ResetInSeconds: 1800}
	snapshot.RateLimits.GitHubGraphQL = &telemetry.RateLimitBucket{Remaining: 4880, Used: 120, Limit: 5000, ResetAt: demoTimePtr(now.Add(42 * time.Minute)), ResetInSeconds: 2520}
	snapshot.RateLimits.RESTUsage = &telemetry.RESTUsage{TotalRequests: 5000, Contributors: []telemetry.RESTUsageContributor{{EndpointFamily: "issues", Count: 4400, Remaining: 0, Limit: 5000}, {EndpointFamily: "pull requests", Count: 600, Remaining: 0, Limit: 5000}}}
	return snapshot
}

func demoDrainingSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	snapshot.Shutdown = telemetry.Shutdown{Status: "draining", Draining: true, SessionsRemaining: 3, RequestedAt: demoTimePtr(now.Add(-7 * time.Minute))}
	return snapshot
}

func demoDegradedSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.LifetimeTotals = telemetry.LifetimeTotals{Available: false, DegradedReason: "usage ledger unavailable in this scenario"}
	snapshot.CycleTime = telemetry.CycleTimeReport{Available: false, DegradedReason: "cycle-time query timed out"}
	snapshot.Budget.DegradedReason = "budget history is partially unavailable"
	snapshot.WorkflowMetrics.DegradedReason = "workflow metrics query failed"
	snapshot.WorkflowMetrics.RuntimeStore.Status = "degraded"
	snapshot.WorkflowMetrics.RuntimeStore.Healthy = false
	return snapshot
}

func demoBudgetRefusalsSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	now := snapshot.GeneratedAt
	capValue := 42.0
	snapshot.Budget.CurrentSpendUSD = 41.15
	snapshot.Budget.ProjectedCostUSD = 6.2
	snapshot.Budget.ProjectedSpendUSD = 47.35
	snapshot.Budget.PerDayMaxUSD = &capValue
	snapshot.Budget.Refusals = []telemetry.BudgetRefusal{
		{IssueID: "demo-refusal-1", Identifier: "digitaldrywood/billing-api#5310", Code: "daily_cap_exceeded", Message: "Projected spend would exceed the daily cap.", CurrentSpendUSD: 41.15, ProjectedCostUSD: 6.2, MaxUSD: &capValue, RefusedAt: now.Add(-18 * time.Minute), ResetAt: demoTimePtr(now.Truncate(24 * time.Hour).Add(24 * time.Hour))},
	}
	return snapshot
}

func demoBlockedHeavySnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.Blocked = append(snapshot.Blocked,
		demoBlocked("billing-api", "demo-blocked-human", "digitaldrywood/billing-api#5282", "Human approval required for billing migration", "waiting for operator approval", "human-review", 18),
		demoBlocked("infra-platform", "demo-blocked-stale", "digitaldrywood/infra-platform#5283", "Stale lease after workspace hook timeout", "lease expired before worker heartbeat", "reclaim", 20),
	)
	snapshot.Counts.Blocked = len(snapshot.Blocked)
	return snapshot
}

func demoLongContentSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	if len(snapshot.Running) > 0 {
		identifier := "digitaldrywood/creswoodcorners-phone#66"
		snapshot.Running[0].Identifier = identifier
		snapshot.Running[0].URL = demoIssueURL(identifier)
		snapshot.Running[0].Title = "Implement page-addressable screenshot scenarios with very long deterministic fixture names, wide workspace paths, detailed token accounting, and browser-friendly waiting selectors"
		snapshot.Running[0].SessionID = "thread-demo-core-5260-very-long-session-id-for-wide-table-verification-0000000001"
		snapshot.Running[0].WorkspacePath = "/tmp/detent-screenshots/workspaces/detent-core/5260/very/deep/generated/worktree/path/that/exercises/wrapping"
		snapshot.Running[0].LastMessage = "Long message: scenario route loaded, manifest matched, Chart.js endpoint agreed with seeded ledger, and visual baseline is ready for capture."
		snapshot.Running[0].PullRequest = &telemetry.PullRequest{
			Number:           75,
			URL:              demoPRURL(identifier, 75),
			BranchName:       "detent/demo-long-content",
			State:            "OPEN",
			MergeableState:   "clean",
			CIStatus:         "pending",
			CodexReviewState: "CLEAN",
		}
		snapshot.Running[0].Tokens = telemetry.Tokens{Input: 980000, Output: 214000, Total: 1194000, RuntimeSeconds: 9180}
	}
	return snapshot
}

func demoDenseSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	for i := 0; i < 12; i++ {
		state := []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework"}[i%6]
		snapshot.BoardIssues = append(snapshot.BoardIssues, demoIssue(demoPrimaryProjectID, fmt.Sprintf("demo-dense-%02d", i), fmt.Sprintf("digitaldrywood/detent-core#54%02d", i), fmt.Sprintf("Dense lane card %02d exercises compact chips and overflow", i+1), state, i+1))
	}
	return snapshot
}

func demoHotPathSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.Projects = demoProjectSnapshots(demoProjectsForVariant("hot-path"))
	snapshot.Tokens = telemetry.Tokens{Input: 180000, Output: 52000, Total: 232000, RuntimeSeconds: 6400}
	snapshot.Budget.CurrentSpendUSD = 36.8
	return snapshot
}

func demoTrackerRefreshGapSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	completed := demoCompleted(demoPrimaryProjectID, "demo-tracker-refresh-gap", "digitaldrywood/detent-core#5294", "Keep completed implementation visible during tracker refresh", "completed", 3, "gpt-5-codex", 41000)
	completed.PullRequest = &telemetry.PullRequest{
		Number:             5294,
		URL:                demoPRURL(completed.Identifier, 5294),
		BranchName:         "detent/demo-tracker-refresh-gap",
		State:              "OPEN",
		MergeableState:     "clean",
		CIStatus:           "success",
		CheckRunCount:      5,
		StatusContextCount: 1,
		CIDurationSeconds:  260,
		CodexReviewState:   "CLEAN",
	}
	snapshot.Completed = append([]telemetry.Completed{completed}, snapshot.Completed...)
	snapshot.Counts.Completed = len(snapshot.Completed)
	for i := range snapshot.Projects {
		if snapshot.Projects[i].Project.ID == demoPrimaryProjectID {
			snapshot.Projects[i].Counts.Completed++
		}
	}
	return snapshot
}

func demoIssue(projectID string, id string, identifier string, title string, state string, hoursAgo int) telemetry.Issue {
	at := demoBaseTime.Add(-time.Duration(hoursAgo) * time.Hour)
	return telemetry.Issue{
		ID:             id,
		Identifier:     identifier,
		ProjectID:      projectID,
		URL:            demoIssueURL(identifier),
		Title:          title,
		Description:    title + " for deterministic Detent screenshot scenarios.",
		State:          state,
		Labels:         []string{"enhancement", "demo"},
		Assignees:      []string{"detent-bot"},
		Owner:          "detent-bot",
		UpdatedAt:      &at,
		StageUpdatedAt: &at,
	}
}

func demoPipelineIssue(projectID string, id string, identifier string, title string, state string, pr int, ci string, mergeable string, hoursAgo int) telemetry.Issue {
	issue := demoIssue(projectID, id, identifier, title, state, hoursAgo)
	issue.PullRequest = &telemetry.PullRequest{
		Number:             pr,
		URL:                demoPRURL(identifier, pr),
		BranchName:         "detent/demo-" + id,
		State:              "OPEN",
		MergeableState:     mergeable,
		CIStatus:           ci,
		CheckRunCount:      5,
		StatusContextCount: 1,
		CIDurationSeconds:  int64(240 + hoursAgo*20),
		QuietWaitSeconds:   int64(hoursAgo * 60),
		RunningChecks:      []string{"make check"},
		CodexReviewState:   "CLEAN",
	}
	if ci == "failure" {
		issue.PullRequest.CodexReviewState = "P1"
		issue.PullRequest.SlowChecks = []telemetry.PullRequestCheck{{Name: "go test -race", Status: "completed", Conclusion: "failure", DurationSeconds: 620}}
	}
	if state == "Done" {
		issue.PullRequest.State = "MERGED"
	}
	return issue
}

func demoQueued(projectID string, id string, identifier string, title string, attempt int, dueAt time.Time, err string) telemetry.Queued {
	dueIn := dueAt.Sub(demoBaseTime).Milliseconds()
	return telemetry.Queued{
		Issue:          demoIssue(projectID, id, identifier, title, "Todo", attempt+2),
		Attempt:        attempt,
		DueAt:          &dueAt,
		DueInMillis:    dueIn,
		Error:          err,
		WorkerHost:     "demo-worker-queue",
		WorkspacePath:  "/tmp/detent-screenshots/workspaces/" + projectID + "/" + id,
		ProjectedSpend: float64(attempt) * 1.35,
	}
}

func demoBlocked(projectID string, id string, identifier string, title string, err string, target string, hoursAgo int) telemetry.Blocked {
	blockedAt := demoBaseTime.Add(-time.Duration(hoursAgo) * time.Hour)
	lastAt := blockedAt.Add(20 * time.Minute)
	return telemetry.Blocked{
		Issue:          demoIssue(projectID, id, identifier, title, "Blocked", hoursAgo),
		WorkerHost:     "demo-worker-blocked",
		WorkspacePath:  "/tmp/detent-screenshots/workspaces/" + projectID + "/" + id,
		SessionID:      "thread-" + id,
		Error:          err,
		RecoveryReason: err,
		RecoveryTarget: target,
		BlockedAt:      &blockedAt,
		LastEventAt:    &lastAt,
		LastEvent:      "blocked",
		LastMessage:    err,
	}
}

func demoCompleted(projectID string, id string, identifier string, title string, state string, minutesAgo int, model string, totalTokens int64) telemetry.Completed {
	completedAt := demoBaseTime.Add(-time.Duration(minutesAgo) * time.Minute)
	startedAt := completedAt.Add(-42 * time.Minute)
	input := totalTokens * 72 / 100
	output := totalTokens - input
	return telemetry.Completed{
		Issue:          demoIssue(projectID, id, identifier, title, state, minutesAgo/60+1),
		SessionID:      "thread-" + id,
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
		Turns:          6,
		RuntimeSeconds: completedAt.Sub(startedAt).Seconds(),
		FinalState:     state,
		Model:          model,
		Tokens:         telemetry.Tokens{Input: input, Output: output, Total: totalTokens, RuntimeSeconds: completedAt.Sub(startedAt).Seconds()},
	}
}

func demoIssueURL(identifier string) string {
	repo, number, ok := strings.Cut(identifier, "#")
	if !ok {
		return "https://github.test/digitaldrywood/detent/issues/0"
	}
	return "https://github.test/" + repo + "/issues/" + number
}

func demoPRURL(identifier string, number int) string {
	repo, _, ok := strings.Cut(identifier, "#")
	if !ok {
		repo = "digitaldrywood/detent"
	}
	return fmt.Sprintf("https://github.test/%s/pull/%d", repo, number)
}

func demoTimePtr(value time.Time) *time.Time {
	return &value
}

func demoProjectsForVariant(variant string) []templates.ProjectSmallMultiple {
	now := demoBaseTime
	if variant == "empty" || variant == "settings-empty" {
		return nil
	}
	projects := []templates.ProjectSmallMultiple{
		demoProject(demoPrimaryProjectID, demoPrimaryProjectName, "https://github.test/digitaldrywood/detent-core", false, 1, 1, 1, 2, 101730, 18.72, now),
		demoProject("docs-site", "docs-site", "https://github.test/digitaldrywood/docs-site", false, 1, 1, 0, 1, 59450, 6.1, now),
		demoProject("billing-api", "billing-api", "https://github.test/digitaldrywood/billing-api", false, 0, 1, 1, 0, 48600, 9.4, now),
		demoProject("mobile-client", "mobile-client", "https://github.test/digitaldrywood/mobile-client", true, 0, 1, 0, 1, 22000, 2.2, now),
		demoProject("infra-platform", "infra-platform", "https://github.test/digitaldrywood/infra-platform", false, 1, 0, 0, 0, 29200, 4.7, now),
		demoProject("release-train", "release-train", "https://github.test/digitaldrywood/release-train", false, 0, 0, 0, 1, 52000, 5.8, now),
		demoProject("observability-console", "observability-console-with-long-name", "https://github.test/digitaldrywood/observability-console", false, 0, 0, 0, 0, 17000, 1.8, now),
		demoProject("agent-lab", "agent-lab", "https://github.test/digitaldrywood/agent-lab", false, 0, 0, 0, 0, 0, 0, now),
	}
	switch variant {
	case "project-empty", "reports-empty", "settings-empty", "no-history":
		for i := range projects {
			projects[i].Running = 0
			projects[i].QueueCount = 0
			projects[i].Blocked = 0
			projects[i].Completed = 0
			projects[i].TotalTokens = 0
			projects[i].ThroughputTokensPerSecond = 0
			projects[i].CurrentSpendUSD = 0
			projects[i].Samples = nil
		}
	case "hot-path", "model-heavy", "filtered-project":
		for i := range projects {
			if projects[i].ID == "billing-api" {
				projects[i].Running = 2
				projects[i].QueueCount = 4
				projects[i].Blocked = 2
				projects[i].Completed = 8
				projects[i].TotalTokens = 310000
				projects[i].CurrentSpendUSD = 31.4
				projects[i].Samples = demoSamples(now, 2, 4, 2, 8, 310000, 31.4)
			}
		}
	}
	return projects
}

func demoProject(id string, name string, url string, paused bool, running int, queue int, blocked int, completed int, tokens int64, spend float64, now time.Time) templates.ProjectSmallMultiple {
	return templates.ProjectSmallMultiple{
		ID:                        id,
		Name:                      name,
		URL:                       url,
		Color:                     projectcolor.ColorForID(id),
		Paused:                    paused,
		Running:                   running,
		QueueCount:                queue,
		Blocked:                   blocked,
		Completed:                 completed,
		TotalTokens:               tokens,
		ThroughputTokensPerSecond: float64(tokens%50000) / 600,
		CurrentSpendUSD:           spend,
		Samples:                   demoSamples(now, running, queue, blocked, completed, tokens, spend),
	}
}

func demoSamples(now time.Time, running int, queue int, blocked int, completed int, tokens int64, spend float64) []templates.ProjectSmallMultipleSample {
	samples := make([]templates.ProjectSmallMultipleSample, 0, 12)
	for i := 11; i >= 0; i-- {
		scale := int64(12 - i)
		samples = append(samples, templates.ProjectSmallMultipleSample{
			At:                        now.Add(-time.Duration(i) * time.Minute),
			Running:                   max(0, running-(i%2)),
			TotalTokens:               tokens - int64(i)*max(100, tokens/30),
			ThroughputTokensPerSecond: float64(scale) * 1.8,
			SpendUSD:                  spend * float64(scale) / 12,
			QueueDepth:                max(0, queue-(i%3)),
			Blocked:                   blocked,
			Completed:                 max(0, completed-(i/4)),
		})
	}
	return samples
}

func demoProjectSnapshots(projects []templates.ProjectSmallMultiple) []telemetry.ProjectSnapshot {
	out := make([]telemetry.ProjectSnapshot, 0, len(projects))
	for _, project := range projects {
		out = append(out, telemetry.ProjectSnapshot{
			Project: telemetry.Project{ID: project.ID, DisplayName: project.Name, URL: project.URL, Color: project.Color},
			Counts:  telemetry.Counts{Running: project.Running, Queue: project.QueueCount, Blocked: project.Blocked, Completed: project.Completed},
			Tokens:  telemetry.Tokens{Total: project.TotalTokens},
			Throughput: telemetry.TokenThroughput{
				TokensPerSecond: project.ThroughputTokensPerSecond,
				WindowSeconds:   600,
				Tokens:          int64(project.ThroughputTokensPerSecond * 600),
			},
		})
	}
	return out
}

func demoBudgetSpendPoints(now time.Time, total float64) []telemetry.BudgetSpendPoint {
	points := make([]telemetry.BudgetSpendPoint, 0, 8)
	for i := 7; i >= 0; i-- {
		scale := float64(8 - i)
		points = append(points, telemetry.BudgetSpendPoint{At: now.Add(-time.Duration(i) * time.Hour), SpendUSD: total * scale / 8})
	}
	return points
}

func demoBudgetDays() []telemetry.BudgetDay {
	return []telemetry.BudgetDay{
		{Date: "2026-06-09", SpendUSD: 8.4},
		{Date: "2026-06-10", SpendUSD: 12.8},
		{Date: "2026-06-11", SpendUSD: 15.2},
		{Date: "2026-06-12", SpendUSD: 10.9},
		{Date: "2026-06-13", SpendUSD: 22.4},
		{Date: "2026-06-14", SpendUSD: 18.1},
		{Date: "2026-06-15", SpendUSD: 18.72},
	}
}

func demoCycleTime(now time.Time) telemetry.CycleTimeReport {
	return telemetry.CycleTimeReport{
		Available:      true,
		AverageSeconds: int64(7 * time.Hour / time.Second),
		Buckets: []telemetry.CycleTimeBucket{
			{Label: "< 2h", MinSeconds: 0, MaxSeconds: int64(2 * time.Hour / time.Second), Count: 3},
			{Label: "2h-8h", MinSeconds: int64(2 * time.Hour / time.Second), MaxSeconds: int64(8 * time.Hour / time.Second), Count: 7},
			{Label: "8h-1d", MinSeconds: int64(8 * time.Hour / time.Second), MaxSeconds: int64(24 * time.Hour / time.Second), Count: 4},
		},
		Issues: []telemetry.CycleTimeIssue{
			{Key: "digitaldrywood/detent-core#5240", StartedAt: now.Add(-9 * time.Hour), CompletedAt: now.Add(-2 * time.Hour), DurationSeconds: int64(7 * time.Hour / time.Second), Sessions: 1},
			{Key: "digitaldrywood/docs-site#5241", StartedAt: now.Add(-13 * time.Hour), CompletedAt: now.Add(-5 * time.Hour), DurationSeconds: int64(8 * time.Hour / time.Second), Sessions: 2},
		},
	}
}

func demoWorkflowMetrics(now time.Time) telemetry.WorkflowMetrics {
	return telemetry.WorkflowMetrics{
		Available:    true,
		RuntimeStore: demoRuntimeStoreEvidence(now, 36),
		Windows: []telemetry.WorkflowMetricsWindow{
			demoWorkflowMetricsWindow("24h", now.Add(-24*time.Hour), now, 6*time.Minute, 18*time.Minute, 11*time.Minute),
			demoWorkflowMetricsWindow("7d", now.Add(-7*24*time.Hour), now, 8*time.Minute, 14*time.Minute, 9*time.Minute),
			demoWorkflowMetricsWindow("30d", now.Add(-30*24*time.Hour), now, 10*time.Minute, 12*time.Minute, 8*time.Minute),
		},
		ActiveBottleneck: telemetry.WorkflowBottleneck{
			Kind:       "lane_age",
			Label:      "Human Review is slowest",
			Detail:     "digitaldrywood/detent-core#5281 has waited longest in Human Review.",
			ProjectID:  demoPrimaryProjectID,
			IssueID:    "demo-blocked-hook",
			Identifier: "digitaldrywood/detent-core#5281",
			Seconds:    int64(5 * time.Hour / time.Second),
			Count:      1,
		},
	}
}

func demoEmptyWorkflowMetrics(now time.Time) telemetry.WorkflowMetrics {
	return telemetry.WorkflowMetrics{
		Available:    true,
		RuntimeStore: demoRuntimeStoreEvidence(now, 0),
		Windows: []telemetry.WorkflowMetricsWindow{
			{Label: "24h", From: now.Add(-24 * time.Hour), To: now},
			{Label: "7d", From: now.Add(-7 * 24 * time.Hour), To: now},
			{Label: "30d", From: now.Add(-30 * 24 * time.Hour), To: now},
		},
	}
}

func demoWorkflowMetricsWindow(label string, from time.Time, to time.Time, inProgress time.Duration, review time.Duration, merging time.Duration) telemetry.WorkflowMetricsWindow {
	rework := merging + 3*time.Minute
	return telemetry.WorkflowMetricsWindow{
		Label: label,
		From:  from,
		To:    to,
		Lanes: []telemetry.WorkflowPhaseMetric{
			demoWorkflowLaneMetric("In Progress", 9, inProgress, false, 46),
			demoWorkflowLaneMetric("Human Review", 5, review, true, 8),
			demoWorkflowLaneMetric("Merging", 4, merging, false, 22),
			demoWorkflowLaneMetric("Rework", 3, rework, false, 58),
		},
		SubPhases: []telemetry.WorkflowPhaseMetric{
			{ProjectID: demoPrimaryProjectID, PhaseType: "agent_session", PhaseName: "agent_active", Count: 9, TotalSeconds: int64((42 * time.Minute) / time.Second), AverageSeconds: int64((5 * time.Minute) / time.Second), Turns: 38, TotalTokens: 284000, EndpointFamily: "codex"},
			{ProjectID: demoPrimaryProjectID, PhaseType: "ci", PhaseName: "ci_wait", Count: 4, TotalSeconds: int64((19 * time.Minute) / time.Second), AverageSeconds: int64((5 * time.Minute) / time.Second), EndpointFamily: "checks"},
		},
		LaneTrends: []telemetry.WorkflowLaneTrend{
			demoWorkflowLaneTrend("In Progress", inProgress),
			demoWorkflowLaneTrend("Human Review", review),
			demoWorkflowLaneTrend("Merging", merging),
			demoWorkflowLaneTrend("Rework", rework),
		},
	}
}

func demoWorkflowLaneMetric(name string, count int64, average time.Duration, bottleneck bool, activePercent int64) telemetry.WorkflowPhaseMetric {
	seconds := int64(average / time.Second)
	totalSeconds := seconds * count
	activeSeconds := totalSeconds * activePercent / 100
	return telemetry.WorkflowPhaseMetric{
		ProjectID:      demoPrimaryProjectID,
		PhaseType:      "lane",
		PhaseName:      name,
		Count:          count,
		TotalSeconds:   totalSeconds,
		AverageSeconds: seconds,
		P50Seconds:     seconds,
		P90Seconds:     int64((average + average/3) / time.Second),
		P95Seconds:     int64((average + average/2) / time.Second),
		ActiveSeconds:  activeSeconds,
		WaitSeconds:    totalSeconds - activeSeconds,
		ActivePercent:  float64(activePercent),
		Bottleneck:     bottleneck,
		Comparison:     &telemetry.WorkflowMetricComparison{Label: "demo comparison", Direction: "unchanged"},
	}
}

func demoWorkflowLaneTrend(name string, average time.Duration) telemetry.WorkflowLaneTrend {
	points := make([]telemetry.WorkflowLaneTrendPoint, 0, 8)
	baseSeconds := int64(average / time.Second)
	for i := range 8 {
		offset := int64(i - 4)
		value := baseSeconds + offset*20
		if value < 0 {
			value = 0
		}
		points = append(points, telemetry.WorkflowLaneTrendPoint{
			Label:          strconv.Itoa(i + 1),
			Count:          1,
			AverageSeconds: value,
		})
	}
	return telemetry.WorkflowLaneTrend{
		ProjectID:  demoPrimaryProjectID,
		PhaseName:  name,
		Points:     points,
		TotalCount: int64(len(points)),
	}
}

func demoRuntimeStoreEvidence(now time.Time, workflowRows int64) telemetry.RuntimeStoreEvidence {
	var oldest *time.Time
	var newest *time.Time
	if workflowRows > 0 {
		oldestValue := now.Add(-27 * 24 * time.Hour)
		newestValue := now.Add(-11 * time.Minute)
		oldest = &oldestValue
		newest = &newestValue
	}
	return telemetry.RuntimeStoreEvidence{
		Backend:          "sqlite",
		Status:           "healthy",
		Healthy:          true,
		Path:             "/tmp/detent-screenshots/detent.db",
		MigrationStatus:  "applied through 6",
		MigrationVersion: 6,
		Tables: []telemetry.RuntimeStoreTableEvidence{
			{Name: "detent_runs", Scope: "fleet", RowCount: 7},
			{Name: "codex_sessions", Scope: "fleet", RowCount: 22},
			{Name: "fair_share_usage", Scope: "project", RowCount: 1},
			{Name: "usage_events", Scope: "project", RowCount: 18},
			{Name: "workflow_phase_events", Scope: "project", RowCount: workflowRows},
			{Name: "work_attempts", Scope: "project", RowCount: 3},
			{Name: "scheduler_decisions", Scope: "project", RowCount: 12},
		},
		WorkflowPhaseEvents: telemetry.RuntimeStoreWorkflowPhaseEvents{
			RowCount:         workflowRows,
			OldestFinishedAt: oldest,
			NewestFinishedAt: newest,
		},
	}
}

func demoTokenTrend(now time.Time) []telemetry.TokenTrendPoint {
	points := make([]telemetry.TokenTrendPoint, 0, 10)
	for i := 9; i >= 0; i-- {
		input := int64(8000 + (9-i)*1400)
		output := int64(2500 + (9-i)*550)
		points = append(points, telemetry.TokenTrendPoint{At: now.Add(-time.Duration(i) * time.Minute), Input: input, Output: output, Total: input + output})
	}
	return points
}

func SeedDemoUsageEvents(ctx context.Context, backend store.Store) error {
	if backend == nil {
		return nil
	}
	for _, event := range DemoUsageEvents() {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			return fmt.Errorf("seed demo usage event: %w", err)
		}
	}
	return nil
}

func DemoUsageEvents() []store.UsageEvent {
	events := make([]store.UsageEvent, 0, 24)
	projects := []string{demoPrimaryProjectID, "docs-site", "billing-api", "mobile-client", "infra-platform", "release-train"}
	models := []string{"gpt-5-codex", "gpt-5", "gpt-5-mini"}
	for day := 13; day >= 0; day-- {
		for i, projectID := range projects {
			finished := demoBaseTime.AddDate(0, 0, -day).Add(time.Duration(i-6) * time.Hour)
			tokens := int64(12000 + (14-day)*900 + i*1400)
			if projectID == "billing-api" && day == 2 {
				tokens *= 5
			}
			pr := int64(5200 + day*10 + i)
			events = append(events, store.UsageEvent{
				ProjectID:      projectID,
				IssueID:        fmt.Sprintf("usage-%s-%02d", projectID, day),
				Identifier:     fmt.Sprintf("digitaldrywood/%s#%d", demoUsageRepo(projectID), 5200+day*10+i),
				PRNumber:       &pr,
				Model:          models[(day+i)%len(models)],
				InputTokens:    tokens * 7 / 10,
				OutputTokens:   tokens * 3 / 10,
				TotalTokens:    tokens,
				CostUSD:        float64(tokens) / 100000,
				RuntimeSeconds: int64(600 + i*90),
				StartedAt:      finished.Add(-40 * time.Minute),
				FinishedAt:     finished,
				Outcome:        "completed",
			})
		}
	}
	return events
}

func demoUsageRepo(projectID string) string {
	if projectID == demoPrimaryProjectID {
		return demoPrimaryProjectName
	}
	return projectID
}

func (s *Server) demoRefresh(c echo.Context, scenario demoScenario) error {
	if scenario.Variant == "refresh-unavailable" {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}
	return c.JSON(http.StatusAccepted, orchestrator.RefreshResponse{
		Queued:      true,
		RequestedAt: demoBaseTime,
		Operations:  []string{"poll", "reconcile", "snapshot"},
	})
}

func (s *Server) demoEvents(c echo.Context, scenario demoScenario) error {
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "streaming unsupported")
	}
	ctx := c.Request().Context()
	selectedProjectID := strings.TrimSpace(c.QueryParam("project"))
	selectedNav := staticSidebarNav(c.QueryParam("nav"))
	selectedView := strings.ToLower(strings.TrimSpace(c.QueryParam("view")))
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream; charset=utf-8")
	res.Header().Set(echo.HeaderCacheControl, "no-cache")
	res.Header().Set("Connection", "keep-alive")
	res.Header().Set("X-Accel-Buffering", "no")
	res.WriteHeader(http.StatusOK)
	if err := s.writeDemoSSE(ctx, res, scenario, demoBaseTime, selectedProjectID, selectedNav, selectedView); err != nil {
		return err
	}
	flusher.Flush()
	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()
	step := 1
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			now := demoBaseTime
			if s.demo.clock == DemoClockPlay || scenario.Variant == "play" {
				now = demoBaseTime.Add(time.Duration(step) * time.Minute)
				step++
			}
			if err := writeSSEComponent(ctx, res.Writer, sseEventTick, templates.LiveTick(now)); err != nil {
				return err
			}
			if s.demo.clock == DemoClockPlay || scenario.Variant == "play" {
				if err := s.writeDemoSSE(ctx, res, scenario, now, selectedProjectID, selectedNav, selectedView); err != nil {
					return err
				}
			}
			flusher.Flush()
		}
	}
}

func (s *Server) writeDemoSSE(ctx context.Context, res *echo.Response, scenario demoScenario, now time.Time, selectedProjectID string, selectedNav string, selectedView string) error {
	data := s.demoDashboardData(ctx, scenario)
	if selectedProjectID == "" {
		selectedProjectID = strings.TrimSpace(scenario.ProjectID)
	}
	if selectedProjectID != "" {
		projectScenario := scenario
		projectScenario.ProjectID = selectedProjectID
		if projectData, ok := s.demoProjectDashboardData(ctx, projectScenario); ok {
			data = projectData
		}
	}
	if selectedNav != "" {
		data.ActiveNav = selectedNav
	}
	if selectedView == "" {
		selectedView = demoSSEViewForScenario(scenario)
	}
	data.Snapshot.GeneratedAt = now
	if len(data.Snapshot.Running) > 0 && now.After(demoBaseTime) {
		delta := int64(now.Sub(demoBaseTime).Minutes())
		data.Snapshot.Running[0].TurnCount += int(delta)
		data.Snapshot.Running[0].Tokens.Total += delta * 1200
		data.Snapshot.Running[0].Tokens.Input += delta * 850
		data.Snapshot.Running[0].Tokens.Output += delta * 350
		data.Snapshot.Running[0].DiffAdded += int(delta * 8)
		data.Snapshot.Running[0].DiffRemoved += int(delta)
		data.Snapshot.Tokens.Total += delta * 1200
	}
	snapshotComponent := templates.SnapshotView(data)
	switch selectedView {
	case sseViewKanban:
		data.ActiveNav = "kanban"
		snapshotComponent = templates.ProjectKanbanSnapshot(data)
	case sseViewRuns:
		data.ActiveNav = "runs"
		snapshotComponent = templates.ProjectRunsSnapshot(data)
	case sseViewDiagnostics:
		data.ActiveNav = "diagnostics"
		snapshotComponent = templates.ProjectDiagnosticsSnapshot(data)
	case sseViewConfiguration:
		data.ActiveNav = "configuration"
	}
	if err := writeSSEComponent(ctx, res.Writer, sseEventSnapshot, snapshotComponent); err != nil {
		return err
	}
	if err := writeSSEComponent(ctx, res.Writer, sseEventSidebar, templates.DashboardSidebarContent(templates.DashboardShellDataFromDashboard(data))); err != nil {
		return err
	}
	return writeSSEComponent(ctx, res.Writer, sseEventGitHubAPI, templates.GitHubAPIHealthChrome(data.Snapshot))
}

func demoSSEViewForScenario(scenario demoScenario) string {
	switch scenario.Page {
	case "fleet-kanban":
		return sseViewKanban
	case "kanban":
		return sseViewKanban
	case "runs":
		return sseViewRuns
	case "diagnostics":
		return sseViewDiagnostics
	case "settings":
		if scenario.ProjectID != "" {
			return sseViewConfiguration
		}
	}
	return ""
}
