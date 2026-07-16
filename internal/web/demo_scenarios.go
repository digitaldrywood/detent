package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/efficiency"
	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/demofixtures"
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
	ID                string
	Route             string
	Method            string
	WaitSelector      string
	Page              string
	Variant           string
	ProjectID         string
	KanbanMode        string
	ShowBlockedAlerts bool
	HideFromManifest  bool
	Status            int
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
	return scenario.Route != "/events" && !scenario.HideFromManifest
}

func demoScenarioDefinitions() []demoScenario {
	return []demoScenario{
		{ID: "fleet-empty-first-snapshot", Route: "/fleet", WaitSelector: "#snapshot", Page: "fleet", Variant: "empty"},
		{ID: "fleet-healthy-parallel-work", Route: "/fleet", WaitSelector: "#snapshot", Page: "fleet", Variant: "healthy"},
		{ID: "fleet-overloaded-rate-limited", Route: "/fleet", WaitSelector: "#snapshot", Page: "fleet", Variant: "overloaded"},
		{ID: "fleet-draining-shutdown", Route: "/fleet", WaitSelector: "#snapshot", Page: "fleet", Variant: "draining"},
		{ID: "fleet-dense-multiproject", Route: "/fleet", WaitSelector: "#snapshot", Page: "fleet", Variant: "dense"},
		{ID: "fleet-degraded-telemetry", Route: "/fleet", WaitSelector: "#snapshot", Page: "fleet", Variant: "degraded"},
		{ID: "github-api-healthy", Route: "/health/ui", WaitSelector: "#health-verdict", Page: "health", Variant: "github-api-healthy"},
		{ID: "github-api-warning", Route: "/health/ui", WaitSelector: "#health-verdict", Page: "health", Variant: "github-api-warning"},
		{ID: "github-api-secondary-backoff", Route: "/health/ui", WaitSelector: "#health-verdict", Page: "health", Variant: "github-api-secondary-backoff"},
		{ID: "github-api-primary-exhausted", Route: "/health/ui", WaitSelector: "#health-verdict", Page: "health", Variant: "github-api-primary-exhausted"},
		{ID: "backend-capacity-outage", Route: "/health/ui", WaitSelector: "#backend-capacity-outage", Page: "health", Variant: "backend-capacity-outage", HideFromManifest: true},
		{ID: "fleet-kanban-multiproject", Route: "/", WaitSelector: "#board-lanes", Page: "fleet-kanban", Variant: "dense-kanban", KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "fleet-kanban-blocked-alerts", Route: "/", WaitSelector: "#board-lanes", Page: "fleet-kanban", Variant: "healthy", KanbanMode: workflowconfig.KanbanModeReadOnly, ShowBlockedAlerts: true},
		{ID: "board-ramp-active-recoveries", Route: "/", WaitSelector: "#board-lanes", Page: "fleet-kanban", Variant: "board-ramp-active-recoveries", KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "board-scheduled-pacing", Route: "/", WaitSelector: "#board-lanes", Page: "fleet-kanban", Variant: "board-scheduled-pacing", KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "health-scheduled-pacing", Route: "/health/ui", WaitSelector: "#dispatch-recovery-status", Page: "health", Variant: "board-scheduled-pacing"},
		{ID: "board-degraded-health-banners", Route: "/", WaitSelector: "#board-lanes", Page: "fleet-kanban", Variant: "board-degraded-health-banners", KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "health-dispatch-recoveries", Route: "/health/ui", WaitSelector: "#dispatch-recovery-status", Page: "health", Variant: "board-degraded-health-banners"},
		{ID: "fleet-kanban-external-lane-timer", Route: "/", WaitSelector: "#board-lanes", Page: "fleet-kanban", Variant: "external-lane-timer", KanbanMode: workflowconfig.KanbanModeReadOnly, HideFromManifest: true},
		{ID: "fleet-kanban-unblocker-boost", Route: "/", WaitSelector: "#board-lanes", Page: "fleet-kanban", Variant: "unblocker-boost", KanbanMode: workflowconfig.KanbanModeReadOnly, HideFromManifest: true},
		{ID: "stop-run-picker", Route: "/fleet", WaitSelector: "#snapshot", Page: "fleet", Variant: "stop-run-picker", HideFromManifest: true},
		{ID: "project-active-overview", Route: "/projects/dogfood", WaitSelector: "#snapshot", Page: "project", Variant: "healthy", ProjectID: demoPrimaryProjectID},
		{ID: "project-paused-overview", Route: "/projects/mobile-client", WaitSelector: "#snapshot", Page: "project", Variant: "paused", ProjectID: "mobile-client"},
		{ID: "project-empty-overview", Route: "/projects/agent-lab", WaitSelector: "#snapshot", Page: "project", Variant: "project-empty", ProjectID: "agent-lab"},
		{ID: "project-hot-path", Route: "/projects/billing-api", WaitSelector: "#snapshot", Page: "project", Variant: "hot-path", ProjectID: "billing-api"},
		{ID: "project-not-found", Route: "/projects/missing-project", WaitSelector: "body", Page: "project", Variant: "not-found", ProjectID: "missing-project", Status: http.StatusNotFound},
		{ID: "kanban-full-integration", Route: "/projects/dogfood/kanban", WaitSelector: "#board-lanes", Page: "kanban", Variant: "healthy", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-startup-loading", Route: "/projects/dogfood/kanban", WaitSelector: "#snapshot", Page: "kanban", Variant: "startup-loading", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-read-only", Route: "/projects/dogfood/kanban", WaitSelector: "#board-lanes", Page: "kanban", Variant: "healthy", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeReadOnly},
		{ID: "kanban-empty-lanes", Route: "/projects/agent-lab/kanban", WaitSelector: "#board-lanes", Page: "kanban", Variant: "project-empty", ProjectID: "agent-lab", KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-dense-overflow", Route: "/projects/dogfood/kanban", WaitSelector: "#board-lanes", Page: "kanban", Variant: "dense-kanban", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-transition-blocked", Route: "/projects/dogfood/kanban", WaitSelector: "#board-lanes", Page: "kanban", Variant: "transition-blocked", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
		{ID: "kanban-terminal-states", Route: "/projects/dogfood/kanban", WaitSelector: "#board-lanes", Page: "kanban", Variant: "terminal", ProjectID: demoPrimaryProjectID, KanbanMode: workflowconfig.KanbanModeIntegration},
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
		// The fleet demo scenarios route to the board home ("/").
		return []string{"#snapshot"}
	case "fleet-kanban":
		return []string{"#snapshot", "#board-lanes"}
	case "kanban":
		// The startup-loading scenario renders a skeleton, so only
		// #snapshot is reliably present across every kanban scenario.
		return []string{"#snapshot"}
	case "reports":
		return []string{"main", "#reports-kpis"}
	case "settings":
		return []string{"main", "#settings-global"}
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
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.FleetPage(data))
}

func (s *Server) demoBoard(c echo.Context, scenario demoScenario) error {
	data := s.demoDashboardData(c.Request().Context(), scenario)
	data.ActiveNav = "board"
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.BoardPage(data))
}

func (s *Server) demoHealthDashboard(c echo.Context, scenario demoScenario) error {
	data := s.demoDashboardData(c.Request().Context(), scenario)
	data.ActiveNav = "health"
	data.Title = instancePageTitle(s.instanceName(), "Health - Detent")
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.HealthPageV2(data))
}

func (s *Server) demoAnalyticsDashboard(c echo.Context, scenario demoScenario) error {
	data := s.demoDashboardData(c.Request().Context(), scenario)
	data.ActiveNav = "analytics"
	data.Title = instancePageTitle(s.instanceName(), "Analytics - Detent")
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.AnalyticsPageV2(data))
}

func (s *Server) demoProjectDashboard(c echo.Context, scenario demoScenario, view string) error {
	if scenario.Status == http.StatusNotFound || scenario.Variant == "not-found" {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	data, ok := s.demoProjectDashboardData(c.Request().Context(), scenario)
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	applyDashboardPreferences(c.Request(), &data)
	switch view {
	case "kanban":
		data.ActiveNav = "kanban"
		data.Title = s.projectPageTitle(data, "Kanban")
		return render(c, templates.ProjectBoardPage(data))
	case "runs":
		data.ActiveNav = "runs"
		data.Title = s.projectPageTitle(data, "Runs")
		return render(c, templates.ProjectRunsPageV2(data))
	case "diagnostics":
		data.ActiveNav = "diagnostics"
		data.Title = s.projectPageTitle(data, "Diagnostics")
		return render(c, templates.ProjectDiagnosticsPageV2(data))
	case "configuration":
		settingsData := s.demoSettingsData(c.Request().Context(), scenario, scenario.ProjectID)
		settingsData.ActiveNav = "configuration"
		applySettingsPreferences(c.Request(), &settingsData)
		return render(c, templates.Settings(settingsData))
	default:
		data.ActiveNav = "overview"
		return render(c, templates.ProjectOverviewPage(data))
	}
}

func (s *Server) demoReports(c echo.Context, scenario demoScenario) error {
	if scenario.Variant == "invalid-date-range" {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_date_range", "from must be on or before to"))
	}
	if scenario.Variant == "reports-empty" {
		data := s.demoEmptyReportsData(c.Request().Context(), scenario)
		applyReportsPreferences(c.Request(), &data)
		return render(c, templates.ReportsPageV2(data))
	}
	projectID := ""
	if scenario.Variant == "filtered-project" {
		projectID = scenario.ProjectID
	}
	timezone, err := reportsTimezone(c.QueryParam("tz"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_timezone", "tz must be an IANA timezone"))
	}
	data, err := s.reportsData(c.Request().Context(), time.Time{}, time.Time{}, projectID, timezone, demoBaseTime)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse("usage_reports_failed", "Usage reports failed"))
	}
	data.GeneratedAt = demoBaseTime
	data.Projects = demofixtures.ProjectsForVariant(scenario.Variant)
	data.Snapshot = demofixtures.SnapshotForScenario(scenario.ProjectID, scenario.Variant)
	data.Efficiency = demoEfficiencyRollup()
	applyReportsPreferences(c.Request(), &data)
	return render(c, templates.ReportsPageV2(data))
}

func (s *Server) demoSettings(c echo.Context, scenario demoScenario, selectedProjectID string) error {
	data := s.demoSettingsData(c.Request().Context(), scenario, selectedProjectID)
	applySettingsPreferences(c.Request(), &data)
	return render(c, templates.Settings(data))
}

func (s *Server) demoOnboarding(c echo.Context, scenario demoScenario) error {
	return render(c, templates.OnboardingPage(s.demoOnboardingData(scenario)))
}

func (s *Server) demoDashboardData(ctx context.Context, scenario demoScenario) templates.DashboardData {
	instanceName := s.instanceName()
	snapshot := demofixtures.SnapshotForScenario(scenario.ProjectID, scenario.Variant)
	data := templates.DashboardData{
		Title:           instancePageTitle(instanceName, "Detent"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Build:           s.build,
		ConnectorName:   s.connector.Name(),
		DashboardURL:    s.dashboardURL,
		Snapshot:        snapshot,
		Projects:        demofixtures.ProjectsForVariant(scenario.Variant),
		Kanban:          demoKanbanData(scenario, ""),
		Assets:          s.assets.templatePaths(),
		ActiveNav:       "fleet",
	}
	data.EfficiencyReceipts = demoEfficiencyReceipts(snapshot)
	return data
}

func (s *Server) demoProjectDashboardData(ctx context.Context, scenario demoScenario) (templates.DashboardData, bool) {
	projectID := strings.TrimSpace(scenario.ProjectID)
	if projectID == "" {
		projectID = demoPrimaryProjectID
	}
	snapshot := demofixtures.SnapshotForScenario(scenario.ProjectID, scenario.Variant)
	projects := demofixtures.ProjectsForVariant(scenario.Variant)
	project, ok := demofixtures.ProjectByID(projects, projectID)
	if !ok {
		return templates.DashboardData{}, false
	}
	scoped := projectScopedSnapshotForProject(snapshot, telemetry.Project{
		ID:          project.ID,
		DisplayName: project.Name,
		URL:         project.URL,
		Color:       project.Color,
	})
	scoped = applyProjectBudgetSnapshot(scoped, project)
	instanceName := s.instanceName()
	data := templates.DashboardData{
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
	}
	data.EfficiencyReceipts = demoEfficiencyReceipts(scoped)
	return data, true
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
		ShowBlockedAlerts:  scenario.ShowBlockedAlerts,
		CanMoveCards:       mode == workflowconfig.KanbanModeIntegration,
		CanRemoveCards:     mode == workflowconfig.KanbanModeIntegration,
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
	c.Response().Header().Set("HX-Reswap", "morph:innerHTML")
	return render(c, templates.BoardSnapshot(data))
}

func applyDemoKanbanMove(snapshot *telemetry.Snapshot, projectID string, issueID string, targetState string) {
	kanbanstate.ApplySnapshotIssues(snapshot, func(issue *telemetry.Issue) {
		if issue == nil || !kanbanstate.SameIssue(*issue, projectID, issueID, snapshot.Project.ID) {
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
	sidebarProjects := demofixtures.ProjectsForVariant(scenario.Variant)
	projectID, projectName := "", ""
	if selectedProjectID != "" {
		if project, ok := demofixtures.ProjectByID(sidebarProjects, selectedProjectID); ok {
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
		Snapshot:        demofixtures.SnapshotForScenario(scenario.ProjectID, scenario.Variant),
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

func demoEfficiencyReceipts(snapshot telemetry.Snapshot) []efficiency.Receipt {
	receipts := make([]efficiency.Receipt, 0, len(snapshot.Completed))
	for index, completed := range snapshot.Completed {
		inputTokens := completed.Tokens.Input
		cachedTokens := completed.Tokens.CachedInput
		if inputTokens == 0 {
			inputTokens = completed.Tokens.Total * 4 / 5
		}
		if cachedTokens == 0 {
			cachedTokens = inputTokens * 97 / 100
		}
		receipts = append(receipts, efficiency.Receipt{
			ProjectID:         completed.ProjectID,
			IssueID:           completed.ID,
			Identifier:        completed.Identifier,
			IssueURL:          completed.URL,
			Sessions:          int64(index + 1),
			Attempts:          int64(index + 1),
			InputTokens:       inputTokens,
			CachedInputTokens: cachedTokens,
			OutputTokens:      completed.Tokens.Output,
			TotalTokens:       completed.Tokens.Total,
			EstimatedCostUSD:  float64(index+1) * 1.75,
			FirstDispatchedAt: completed.StartedAt,
			CompletedAt:       completed.CompletedAt,
			WallSeconds:       int64(completed.CompletedAt.Sub(completed.StartedAt) / time.Second),
			WorkingSeconds:    900,
			GateWaitSeconds:   240,
			MergeTrainSeconds: 120,
			Redispatches:      int64(index),
			TokensAnomaly:     index == 2,
		})
	}
	return receipts
}

func demoEfficiencyRollup() efficiency.Rollup {
	return efficiency.Rollup{
		From: demoBaseTime.Add(-30 * 24 * time.Hour),
		To:   demoBaseTime,
		Current: efficiency.RollupWindow{
			Issues:                12,
			TokensPerIssue:        efficiency.Percentiles{P50: 1_200_000, P90: 3_500_000},
			CostPerIssueUSD:       efficiency.Percentiles{P50: 4.2, P90: 10.8},
			CacheShare:            0.97,
			SessionsPerIssue:      1.4,
			FirstAttemptMergeRate: 0.75,
			Dwell:                 efficiency.Dwell{WorkingSeconds: 54_000, GateWaitSeconds: 12_000, MergeTrainSeconds: 4_800, ParkedSeconds: 1_200},
			Anomalies:             2,
		},
		Baseline: efficiency.RollupWindow{
			Issues:                10,
			TokensPerIssue:        efficiency.Percentiles{P50: 1_350_000, P90: 3_100_000},
			CostPerIssueUSD:       efficiency.Percentiles{P50: 4.6, P90: 9.8},
			CacheShare:            0.96,
			SessionsPerIssue:      1.6,
			FirstAttemptMergeRate: 0.70,
			Dwell:                 efficiency.Dwell{WorkingSeconds: 49_000, GateWaitSeconds: 15_000, MergeTrainSeconds: 5_100, ParkedSeconds: 1_800},
		},
		CacheTrend: []efficiency.TrendPoint{{Day: "2026-06-13", CacheShare: 0.95}, {Day: "2026-06-14", CacheShare: 0.96}, {Day: "2026-06-15", CacheShare: 0.97}},
	}
}

func demoSettingsProjects(variant string) []templates.SettingsProject {
	if variant == "settings-empty" {
		return nil
	}
	projects := []templates.SettingsProject{
		{ID: demoPrimaryProjectID, WorkflowPath: "/tmp/detent-screenshots/WORKFLOW.md", WorkflowDetailsURL: "https://github.com/orgs/digitaldrywood/projects/4/workflows", Workdir: "/tmp/detent-screenshots/source/detent-core", WorktreeRoot: "/tmp/detent-screenshots/workspaces/detent-core", Weight: 120, Priority: 100, TrackerKind: "memory", TrackerProject: "digitaldrywood/detent-core", DependencyAutoUnblock: "enabled: Blocked -> Todo when terminal_or_merged"},
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
			projects[i].WorkflowDetailsURL = ""
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
		Snapshot:        demofixtures.SnapshotForScenario(scenario.ProjectID, scenario.Variant),
		GeneratedAt:     demoBaseTime,
		Day:             empty,
		Project:         templates.UsageReportData{By: "project"},
		Issue:           templates.UsageReportData{By: "issue"},
		PR:              templates.UsageReportData{By: "pr"},
		Model:           templates.UsageReportData{By: "model"},
		Assets:          s.assets.templatePaths(),
		Projects:        demofixtures.ProjectsForVariant(scenario.Variant),
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
		if selectedNav == sseViewHealth || selectedNav == sseViewAnalytics {
			selectedView = selectedNav
		} else {
			selectedView = demoSSEViewForScenario(scenario)
		}
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
	case sseViewHealth:
		data.ActiveNav = "health"
		snapshotComponent = templates.HealthSnapshotV2(data)
	case sseViewAnalytics:
		data.ActiveNav = "analytics"
		snapshotComponent = templates.AnalyticsSnapshotV2(data)
	case sseViewBoard, sseViewKanban:
		data.ActiveNav = selectedView
		snapshotComponent = templates.BoardSnapshot(data)
	case sseViewFleet:
		data.ActiveNav = "fleet"
		snapshotComponent = templates.FleetSnapshotV2(data)
	case sseViewOverview:
		data.ActiveNav = "overview"
		snapshotComponent = templates.ProjectOverviewSnapshotV2(data)
	case sseViewRuns:
		data.ActiveNav = "runs"
		snapshotComponent = templates.ProjectRunsSnapshotV2(data)
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
	if err := writeSSEComponent(ctx, res.Writer, sseEventGitHubAPI, templates.GitHubAPIHealthSidebarItem(templates.DashboardShellDataFromDashboard(data))); err != nil {
		return err
	}
	return writeSSEComponent(ctx, res.Writer, sseEventSidebarV2, templates.AppSidebarContent(templates.DashboardShellDataFromDashboard(data)))
}

func demoSSEViewForScenario(scenario demoScenario) string {
	switch scenario.Page {
	case "health":
		return sseViewHealth
	case "analytics":
		return sseViewAnalytics
	case "fleet":
		return sseViewFleet
	case "fleet-kanban":
		return sseViewBoard
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
