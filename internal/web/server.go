package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/hub"
	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

var (
	ErrMissingHub       = errors.New("web server requires hub")
	ErrMissingStore     = errors.New("web server requires store")
	ErrMissingRegistry  = errors.New("web server requires registry")
	ErrMissingConnector = errors.New("web server requires connector")
)

type Dependencies struct {
	Hub       *hub.Hub[telemetry.Snapshot]
	Store     store.Store
	Registry  *project.Registry
	Connector connector.Connector
	Refresher Refresher
	Recovery  WorkAttemptRecovery
	Activity  *activity.Broker
	History   activity.HistoryReader
}

type Mode string

const (
	ModeRunning    Mode = "running"
	ModeOnboarding Mode = "onboarding"
)

const (
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPIdleTimeout       = 2 * time.Minute
	defaultSSEFragmentInterval   = 5 * time.Second
	defaultSSEHealthInterval     = 10 * time.Second
	defaultSSEMetricsInterval    = time.Minute
	sidebarStateCookieName       = "sidebar_state"
	themeCookieName              = "theme"
	densityCookieName            = "density"
)

type Config struct {
	Logger                *slog.Logger
	Mode                  Mode
	StaticDir             string
	SSETickInterval       time.Duration
	SSEFragmentInterval   time.Duration
	SSEHealthInterval     time.Duration
	SSEMetricsInterval    time.Duration
	HTTPReadHeaderTimeout time.Duration
	HTTPIdleTimeout       time.Duration
	WorkflowPath          string
	Version               string
	Build                 buildinfo.Info
	DashboardURL          string
	Pricing               budget.PricingTable
	GlobalConfig          globalconfig.Config
	GlobalConfigSource    func() globalconfig.Config
	LookupEnv             func(string) string
	Hostname              func() (string, error)
	ConfigPathRule        globalconfig.PathRule
	Kanban                workflowconfig.Kanban
	KanbanWorkflow        workflowconfig.Config
	GitHubWebhookSecret   string
	RuntimeDBPath         string
	RuntimeLogPath        string
	ServerAddress         string
	Demo                  DemoConfig
}

type Server struct {
	echo                *echo.Echo
	hub                 *hub.Hub[telemetry.Snapshot]
	store               store.Store
	registry            *project.Registry
	connector           connector.Connector
	refresher           Refresher
	recovery            WorkAttemptRecovery
	activity            *activity.Broker
	history             activity.HistoryReader
	logger              *slog.Logger
	mode                Mode
	tickEvery           time.Duration
	sseFragmentInterval time.Duration
	sseHealthInterval   time.Duration
	sseMetricsInterval  time.Duration
	workflow            string
	version             string
	build               buildinfo.Info
	dashboardURL        string
	pricing             budget.PricingTable
	globalConfig        globalconfig.Config
	globalConfigSource  func() globalconfig.Config
	lookupEnv           func(string) string
	hostname            func() (string, error)
	configRule          globalconfig.PathRule
	kanban              workflowconfig.Kanban
	kanbanWorkflow      workflowconfig.Config
	githubWebhookSecret string
	dbPath              string
	logPath             string
	serverAddr          string
	assets              staticAssets
	projects            *projectSmallMultipleRecorder
	snapshots           *snapshotEnrichmentCache
	kanbanMutations     *kanbanstate.MutationTracker
	kanbanRefreshes     *kanbanRefreshFeedbackTracker
	kanbanRetryInFlight atomic.Bool
	refreshes           *manualRefreshTracker
	demo                *demoScenarioSet
	apiKeys             *apikey.Service
	ipLimiter           *apiRateLimiter
	keyLimiter          *apiRateLimiter
	asyncWrites         *asyncStoreWriter
	dashboardAuthSecret [32]byte
	afterFunc           func(time.Duration, func()) *time.Timer
}

func NewServer(cfg Config, deps Dependencies) (*Server, error) {
	mode := cfg.mode()
	if mode == ModeRunning {
		if deps.Hub == nil {
			return nil, ErrMissingHub
		}
		if deps.Store == nil {
			return nil, ErrMissingStore
		}
		if deps.Registry == nil {
			return nil, ErrMissingRegistry
		}
		if deps.Connector == nil {
			return nil, ErrMissingConnector
		}
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Server.ReadHeaderTimeout = cfg.httpReadHeaderTimeout()
	e.Server.IdleTimeout = cfg.httpIdleTimeout()
	kanban := cfg.kanban()
	kanbanWorkflow := cfg.kanbanWorkflow(kanban)
	logger := cfg.logger()
	activityBroker := deps.Activity
	if activityBroker == nil {
		activityBroker = activity.NewBroker()
	}
	historyReader := deps.History
	if historyReader == nil {
		historyReader = activity.NewRolloutHistoryReader("", "")
	}
	dashboardAuthSecret, err := newDashboardAuthSecret()
	if err != nil {
		return nil, fmt.Errorf("dashboard auth secret: %w", err)
	}

	server := &Server{
		echo:                e,
		hub:                 deps.Hub,
		store:               deps.Store,
		registry:            deps.Registry,
		connector:           deps.Connector,
		refresher:           deps.Refresher,
		recovery:            deps.Recovery,
		activity:            activityBroker,
		history:             historyReader,
		logger:              logger,
		mode:                mode,
		tickEvery:           cfg.sseTickInterval(),
		sseFragmentInterval: cfg.sseFragmentInterval(),
		sseHealthInterval:   cfg.sseHealthInterval(),
		sseMetricsInterval:  cfg.sseMetricsInterval(),
		workflow:            cfg.workflowPath(),
		version:             strings.TrimSpace(cfg.Version),
		build:               cfg.Build,
		dashboardURL:        cfg.dashboardURL(),
		pricing:             cfg.pricing(),
		globalConfig:        cfg.GlobalConfig,
		globalConfigSource:  cfg.globalConfigSource(),
		lookupEnv:           cfg.lookupEnv(),
		hostname:            cfg.hostname(),
		configRule:          cfg.ConfigPathRule,
		kanban:              kanban,
		kanbanWorkflow:      kanbanWorkflow,
		githubWebhookSecret: cfg.githubWebhookSecret(kanbanWorkflow),
		dbPath:              strings.TrimSpace(cfg.RuntimeDBPath),
		logPath:             strings.TrimSpace(cfg.RuntimeLogPath),
		serverAddr:          strings.TrimSpace(cfg.ServerAddress),
		assets:              newStaticAssets(cfg.staticDir()),
		projects:            newProjectSmallMultipleRecorder(),
		snapshots:           newSnapshotEnrichmentCache(),
		kanbanMutations:     kanbanstate.NewMutationTracker(),
		kanbanRefreshes:     newKanbanRefreshFeedbackTracker(),
		refreshes:           newManualRefreshTracker(),
		demo:                newDemoScenarioSet(cfg.Demo),
		apiKeys:             apikey.NewService(deps.Store),
		ipLimiter:           newAPIRateLimiter(300, 60),
		keyLimiter:          newAPIRateLimiter(120, 30),
		asyncWrites:         newAsyncStoreWriter(256, logger),
		dashboardAuthSecret: dashboardAuthSecret,
		afterFunc:           time.AfterFunc,
	}
	e.HTTPErrorHandler = server.handleHTTPError
	e.Use(server.uiAPICookie)
	server.registerRoutes()
	server.warnIfAPITokenMissingOnNonLoopback()

	return server, nil
}

func (s *Server) Handler() http.Handler {
	return s.echo
}

func (s *Server) Echo() *echo.Echo {
	return s.echo
}

func (s *Server) Start(addr string) error {
	s.logger.Info("starting web server", "addr", addr)
	return s.echo.Start(addr)
}

func (s *Server) StartListener(listener net.Listener) error {
	s.logger.Info("starting web server", "addr", listener.Addr().String())
	return s.echo.Server.Serve(listener)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("stopping web server")
	if s.ipLimiter != nil {
		s.ipLimiter.Stop()
	}
	if s.keyLimiter != nil {
		s.keyLimiter.Stop()
	}
	err := s.echo.Shutdown(ctx)
	if s.asyncWrites != nil {
		if closeErr := s.asyncWrites.Close(ctx); closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return err
}

func (s *Server) registerRoutes() {
	s.echo.GET("/static/*", s.assets.serve)
	s.echo.GET("/health", s.health)
	if s.mode == ModeOnboarding {
		s.echo.GET("/", s.redirectToOnboarding)
		s.echo.GET("/onboarding", s.onboarding)
		s.echo.POST("/onboarding/tracker", s.onboardingTracker)
		s.echo.POST("/onboarding/credentials", s.onboardingCredentials)
		s.echo.POST("/onboarding/project", s.onboardingProject)
		s.echo.POST("/onboarding/agent", s.onboardingAgent)
		s.echo.POST("/onboarding/write", s.onboardingWrite)
		return
	}

	s.echo.GET("/", s.board)
	s.echo.GET("/live-session", s.boardLiveSessionPage)
	s.echo.GET("/fleet", s.dashboard)
	s.echo.GET("/kanban", s.redirectToBoard)
	s.echo.GET("/health/ui", s.healthDashboard)
	s.echo.GET("/analytics", s.analyticsDashboard)
	s.echo.GET("/library", s.library)
	s.echo.GET("/projects/*", s.projectDashboard)
	s.echo.GET("/settings", s.settings)
	s.echo.GET("/api-keys", s.apiKeysPage)
	s.echo.GET("/reports", s.reports)
	s.echo.GET("/events", s.events)
	s.echo.GET("/onboarding", s.redirectToDashboard)
	s.echo.POST("/onboarding/tracker", s.onboardingTracker)
	s.echo.POST("/onboarding/credentials", s.onboardingCredentials)
	s.echo.POST("/onboarding/project", s.onboardingProject)
	s.echo.POST("/onboarding/agent", s.onboardingAgent)
	s.echo.POST("/onboarding/write", s.onboardingWrite)
	apiReadAuth := s.apiAuth(false)
	apiMutateAuth := s.apiAuth(true)
	apiDashboardReadAuth := s.apiAuthWithOptions(apiAuthOptions{allowUICookie: true, allowDashboardHTMX: true})
	apiDashboardSSEReadAuth := s.apiAuthWithOptions(apiAuthOptions{allowUICookie: true, allowDashboardSSE: true})
	apiDashboardMutateAuth := s.apiAuthWithOptions(apiAuthOptions{mutating: true, allowUICookie: true, allowDashboardHTMX: true})
	apiKeyDashboardReadAuth := s.apiAuthWithOptions(apiAuthOptions{allowUICookie: true, allowDashboardHTMX: true, requireDashboardManagementToken: true})
	apiKeyDashboardMutateAuth := s.apiAuthWithOptions(apiAuthOptions{mutating: true, allowUICookie: true, allowDashboardHTMX: true, requireDashboardManagementToken: true})
	apiReadScope := s.requireScope(apikey.ScopeRead)
	apiWriteScope := s.requireScope(apikey.ScopeWrite)
	apiAdminScope := s.requireScope(apikey.ScopeAdmin)
	apiProjectWriteScope := s.requireProjectScope(apikey.ScopeWrite, "project_id")
	s.echo.GET("/api/v1/state", s.apiState, apiReadAuth, apiReadScope)
	s.echo.GET("/api/v1/demo/scenarios", s.apiDemoScenarios, apiReadAuth, apiReadScope)
	s.echo.GET("/api/v1/timeseries", s.apiTimeSeries, apiReadAuth, apiReadScope)
	s.echo.POST("/api/v1/projects/:project_id/work-items", s.apiCreateWorkItem, apiMutateAuth, apiProjectWriteScope)
	s.echo.GET("/api/v1/projects/:project_id/work-attempts/:attempt_id", s.apiWorkAttemptReceipt, apiDashboardReadAuth, apiReadScope)
	s.echo.POST("/api/v1/projects/:project_id/work-attempts/:attempt_id/recovery", s.apiWorkAttemptRecovery, apiDashboardMutateAuth, apiProjectWriteScope)
	s.echo.GET("/api/v1/projects/*", s.apiProject, apiReadAuth, apiReadScope)
	s.echo.GET("/api/v1/keys", s.apiKeysList, apiKeyDashboardReadAuth, apiAdminScope)
	s.echo.POST("/api/v1/keys", s.apiKeysCreate, apiKeyDashboardMutateAuth, apiAdminScope)
	s.echo.GET("/api/v1/keys/:id/rotate", s.apiKeysRotateDialog, apiKeyDashboardReadAuth, apiAdminScope)
	s.echo.POST("/api/v1/keys/:id/rotate", s.apiKeysRotate, apiKeyDashboardMutateAuth, apiAdminScope)
	s.echo.DELETE("/api/v1/keys/:id", s.apiKeysRevoke, apiKeyDashboardMutateAuth, apiAdminScope)
	s.echo.POST("/api/v1/refresh", s.apiRefresh, apiDashboardMutateAuth, apiWriteScope)
	s.echo.POST("/api/v1/webhooks/github", s.githubWebhook)
	s.echo.POST("/api/v1/intake/:project_id/:source", s.intakeWebhook)
	s.echo.GET("/api/v1/refresh", s.methodNotAllowed, apiDashboardReadAuth, apiReadScope)
	s.echo.GET("/api/v1/usage", s.apiUsage, apiReadAuth, apiReadScope)
	s.echo.GET("/api/v1/workflow/timeline", s.apiWorkflowTimeline, apiReadAuth, apiReadScope)
	s.echo.GET("/api/v1/board/card", s.apiBoardCard, apiDashboardReadAuth, apiReadScope)
	s.echo.GET("/api/v1/board/activity", s.apiBoardActivity, apiDashboardReadAuth, apiReadScope)
	s.echo.GET("/api/v1/board/activity/events", s.apiBoardActivityEvents, apiDashboardSSEReadAuth, apiReadScope)
	s.echo.GET("/api/v1/board/session", s.apiBoardSession, apiDashboardReadAuth, apiReadScope)
	s.echo.GET("/api/v1/board/session/events", s.apiBoardSessionEvents, apiDashboardSSEReadAuth, apiReadScope)
	s.echo.GET("/api/v1/board/session/history", s.apiBoardSessionHistory, apiDashboardReadAuth, apiReadScope)
	s.echo.GET("/api/v1/kanban/move", s.apiKanbanMoveDialog, apiDashboardReadAuth, apiReadScope)
	s.echo.POST("/api/v1/kanban/move", s.apiKanbanMove, apiDashboardMutateAuth, apiProjectWriteScope)
	s.echo.POST("/api/v1/kanban/remove", s.apiKanbanRemove, apiDashboardMutateAuth, apiProjectWriteScope)
	s.echo.GET("/api/v1/kanban/comment", s.apiKanbanCommentDialog, apiDashboardReadAuth, apiReadScope)
	s.echo.POST("/api/v1/kanban/comment", s.apiKanbanComment, apiDashboardMutateAuth, apiProjectWriteScope)
	s.echo.POST("/api/v1/kanban/comment/edit", s.apiKanbanCommentEdit, apiDashboardMutateAuth, apiProjectWriteScope)
	s.echo.DELETE("/api/v1/kanban/comment", s.apiKanbanCommentDelete, apiDashboardMutateAuth, apiProjectWriteScope)
	s.echo.GET("/api/v1/*", s.apiIssue, apiReadAuth, apiReadScope)
}

func (s *Server) dashboard(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		return s.demoDashboard(c, scenario)
	}
	ctx := c.Request().Context()
	data := s.dashboardData(ctx, s.latestSnapshot(ctx))
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.FleetPage(data))
}

func (s *Server) board(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		return s.demoBoard(c, scenario)
	}
	ctx := c.Request().Context()
	data := s.boardData(ctx, s.latestSnapshot(ctx))
	data = s.withKanbanRefreshFeedback(data)
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.BoardPage(data))
}

func (s *Server) redirectToBoard(c echo.Context) error {
	return c.Redirect(http.StatusFound, "/")
}

// apiBoardCard renders the session detail sheet for one board card into
// the body-level sheet host. The scope param mirrors the board that
// opened the sheet so its kanban actions post against the same scope
// and success responses return the matching board fragment.
func (s *Server) apiBoardCard(c echo.Context) error {
	ctx := c.Request().Context()
	projectID := strings.TrimSpace(c.QueryParam("project"))
	projectScope := c.QueryParam("scope") == "project" && projectID != ""
	snapshot := s.latestSnapshot(ctx)
	data := s.boardData(ctx, snapshot)
	demo := false
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		demo = true
		data = s.demoDashboardData(ctx, scenario)
		if projectScope {
			projectScenario := scenario
			projectScenario.ProjectID = projectID
			if scoped, ok := s.demoProjectDashboardData(ctx, projectScenario); ok {
				data = scoped
			}
		}
	} else if projectScope {
		if scoped, ok := s.projectDashboardData(ctx, projectID, snapshot); ok {
			data = scoped
		}
	}
	card, ok := templates.FindBoardCard(data, projectID, c.QueryParam("issue"))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Card not found")
	}
	if !demo {
		receipt, err := s.store.EfficiencyReceipt(ctx, projectID, card.IssueID, card.Identifier)
		if err == nil {
			data.EfficiencyReceipts = []efficiency.Receipt{receipt}
		} else if !errors.Is(err, sql.ErrNoRows) {
			s.logger.Warn("efficiency receipt query failed", slog.Any("error", err))
		}
	}
	boardActions := c.QueryParam("actions") == "board"
	expanded := c.QueryParam("expanded") == "1"
	conversation := templates.BoardCardConversationData(data, card, boardActions, expanded)
	if !demo {
		conversation = s.hydrateKanbanConversation(ctx, conversation)
	}
	activityRequest := boardActivityRequest{
		ProjectID: projectID,
		Issue:     c.QueryParam("issue"),
		Limit:     defaultBoardActivityLimit,
	}
	issue := boardActivityIssue(data.Snapshot, activityRequest)
	activityData := s.boardActivityData(ctx, data.Snapshot, issue, activityRequest)
	sessionData := s.boardSessionData(ctx, data.Snapshot, issue, projectID)
	return render(c, templates.BoardCardSheet(data, card, boardActions, expanded, conversation, activityData, sessionData))
}

func (s *Server) healthDashboard(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		return s.demoHealthDashboard(c, scenario)
	}
	ctx := c.Request().Context()
	data := s.healthDashboardData(ctx, s.latestSnapshot(ctx))
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.HealthPageV2(data))
}

func (s *Server) analyticsDashboard(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		return s.demoAnalyticsDashboard(c, scenario)
	}
	ctx := c.Request().Context()
	data := s.analyticsDashboardData(ctx, s.latestSnapshot(ctx))
	data.AnalyticsKind = c.QueryParam("kind")
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.AnalyticsPageV2(data))
}

func (s *Server) projectDashboard(c echo.Context) error {
	ctx := c.Request().Context()
	projectID, view := projectRouteViewParam(c)
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		if strings.TrimSpace(scenario.ProjectID) == "" {
			scenario.ProjectID = projectID
		}
		return s.demoProjectDashboard(c, scenario, view)
	}
	data, ok := s.projectDashboardData(ctx, projectID, s.latestSnapshot(ctx))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	switch view {
	case "kanban":
		data.ActiveNav = "kanban"
		data.Title = s.projectPageTitle(data, "Kanban")
		data = s.withKanbanRefreshFeedback(data)
		applyDashboardPreferences(c.Request(), &data)
		return render(c, templates.ProjectBoardPage(data))
	case "runs":
		data.ActiveNav = "runs"
		data.Title = s.projectPageTitle(data, "Runs")
		applyDashboardPreferences(c.Request(), &data)
		return render(c, templates.ProjectRunsPageV2(data))
	case "diagnostics":
		data.ActiveNav = "diagnostics"
		data.Title = s.projectPageTitle(data, "Diagnostics")
		applyDashboardPreferences(c.Request(), &data)
		return render(c, templates.ProjectDiagnosticsPageV2(data))
	case "configuration":
		settingsData := s.settingsData(ctx, projectID)
		settingsData.ActiveNav = "configuration"
		settingsData.Title = s.projectPageTitle(data, "Configuration")
		applySettingsPreferences(c.Request(), &settingsData)
		return render(c, templates.Settings(settingsData))
	}
	data.ActiveNav = "overview"
	applyDashboardPreferences(c.Request(), &data)
	return render(c, templates.ProjectOverviewPage(data))
}

func dashboardSidebarCollapsed(r *http.Request) bool {
	cookie, err := r.Cookie(sidebarStateCookieName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(cookie.Value) == "false"
}

func dashboardTheme(r *http.Request) string {
	cookie, err := r.Cookie(themeCookieName)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(cookie.Value) == "light" {
		return "light"
	}
	return ""
}

func dashboardDensity(r *http.Request) string {
	cookie, err := r.Cookie(densityCookieName)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(cookie.Value) == "cozy" {
		return "cozy"
	}
	return ""
}

func applyDashboardPreferences(r *http.Request, data *templates.DashboardData) {
	data.SidebarCollapsed = dashboardSidebarCollapsed(r)
	data.Theme = dashboardTheme(r)
	data.Density = dashboardDensity(r)
}

func applySettingsPreferences(r *http.Request, data *templates.SettingsData) {
	data.SidebarCollapsed = dashboardSidebarCollapsed(r)
	data.Theme = dashboardTheme(r)
	data.Density = dashboardDensity(r)
}

func applyReportsPreferences(r *http.Request, data *templates.ReportsData) {
	data.SidebarCollapsed = dashboardSidebarCollapsed(r)
	data.Theme = dashboardTheme(r)
	data.Density = dashboardDensity(r)
}

func applyLibraryPreferences(r *http.Request, data *templates.LibraryData) {
	data.SidebarCollapsed = dashboardSidebarCollapsed(r)
	data.Theme = dashboardTheme(r)
	data.Density = dashboardDensity(r)
}

func applyAPIKeysPreferences(r *http.Request, data *templates.APIKeysData) {
	data.SidebarCollapsed = dashboardSidebarCollapsed(r)
	data.Theme = dashboardTheme(r)
	data.Density = dashboardDensity(r)
}

func projectRouteParam(c echo.Context) string {
	return cleanProjectRouteParam(c.Param("*"))
}

func (s *Server) projectPageTitle(data templates.DashboardData, title string) string {
	name := strings.TrimSpace(data.ProjectName)
	if name == "" {
		name = strings.TrimSpace(data.ProjectID)
	}
	if name == "" {
		name = "Project"
	}
	return instancePageTitle(s.instanceName(), name+" "+strings.TrimSpace(title)+" - Detent")
}

func projectRouteViewParam(c echo.Context) (string, string) {
	projectID := strings.Trim(strings.TrimSpace(projectEscapedRouteParam(c)), "/")
	for _, view := range []string{"kanban", "runs", "diagnostics", "configuration"} {
		suffix := "/" + view
		if strings.HasSuffix(projectID, suffix) {
			return cleanProjectRouteParam(strings.Trim(strings.TrimSuffix(projectID, suffix), "/")), view
		}
	}
	return cleanProjectRouteParam(projectID), "overview"
}

func projectEscapedRouteParam(c echo.Context) string {
	const projectsPrefix = "/projects/"
	path := c.Request().URL.EscapedPath()
	if strings.HasPrefix(path, projectsPrefix) {
		return strings.TrimPrefix(path, projectsPrefix)
	}
	return c.Param("*")
}

func cleanProjectRouteParam(projectID string) string {
	projectID = strings.Trim(strings.TrimSpace(projectID), "/")
	if unescaped, err := url.PathUnescape(projectID); err == nil {
		return strings.Trim(strings.TrimSpace(unescaped), "/")
	}
	return projectID
}

func (s *Server) dashboardData(ctx context.Context, snapshot telemetry.Snapshot) templates.DashboardData {
	instanceName := s.instanceName()
	snapshot = s.fleetKanbanSnapshotWithPendingStates(snapshot)
	return templates.DashboardData{
		Title:           instancePageTitle(instanceName, "Detent"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Build:           s.build,
		ConnectorName:   s.connector.Name(),
		DashboardURL:    s.dashboardURL,
		Snapshot:        snapshot,
		Projects:        s.projectSmallMultiples(ctx, snapshot),
		Kanban:          s.dashboardKanbanData(ctx, "", snapshot),
		Assets:          s.assets.templatePaths(),
		ActiveNav:       "fleet",
	}
}

func (s *Server) boardData(ctx context.Context, snapshot telemetry.Snapshot) templates.DashboardData {
	data := s.dashboardData(ctx, snapshot)
	data.ActiveNav = "board"
	data.Title = instancePageTitle(s.instanceName(), "Detent")
	return data
}

func (s *Server) healthDashboardData(ctx context.Context, snapshot telemetry.Snapshot) templates.DashboardData {
	data := s.dashboardData(ctx, snapshot)
	data.ActiveNav = "health"
	data.Title = instancePageTitle(s.instanceName(), "Health - Detent")
	return data
}

func (s *Server) analyticsDashboardData(ctx context.Context, snapshot telemetry.Snapshot) templates.DashboardData {
	data := s.dashboardData(ctx, snapshot)
	data.ActiveNav = "analytics"
	data.Title = instancePageTitle(s.instanceName(), "Analytics - Detent")
	return data
}

func (s *Server) projectDashboardData(ctx context.Context, projectID string, snapshot telemetry.Snapshot) (templates.DashboardData, bool) {
	projects := s.projectSmallMultiples(ctx, snapshot)
	project, ok := s.dashboardProject(projectID, projects, snapshot)
	if !ok {
		return templates.DashboardData{}, false
	}

	scopedSnapshot := projectScopedSnapshotForProject(snapshot, telemetry.Project{
		ID:          project.ID,
		DisplayName: project.Name,
		URL:         project.URL,
		Color:       project.Color,
	})
	if target, _, _ := s.kanbanActionTarget(project.ID); target.key != "" {
		scopedSnapshot = s.kanbanSnapshotWithPendingStates(target.key, project.ID, scopedSnapshot)
	}
	scopedSnapshot.WorkflowMetrics = s.snapshotWorkflowMetrics(ctx, scopedSnapshot)
	name := strings.TrimSpace(project.Name)
	if name == "" {
		name = strings.TrimSpace(project.ID)
	}
	instanceName := s.instanceName()
	data := templates.DashboardData{
		Title:           instancePageTitle(instanceName, name+" - Detent"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Build:           s.build,
		ConnectorName:   s.connector.Name(),
		DashboardURL:    s.dashboardURL,
		Snapshot:        scopedSnapshot,
		Projects:        projects,
		Kanban:          s.dashboardKanbanData(ctx, project.ID, scopedSnapshot),
		Assets:          s.assets.templatePaths(),
		ActiveNav:       "project",
		ProjectID:       strings.TrimSpace(project.ID),
		ProjectName:     name,
		ProjectPaused:   project.Paused,
	}
	receipts, err := s.store.ListEfficiencyReceipts(ctx, efficiency.Query{ProjectID: project.ID, Limit: 100})
	if err != nil {
		s.logger.Warn("efficiency receipts query failed", slog.Any("error", err))
	} else {
		data.EfficiencyReceipts = receipts
	}
	return data, true
}

func (s *Server) withKanbanRefreshFeedback(data templates.DashboardData) templates.DashboardData {
	data = s.withKanbanRevertFeedback(data)
	if s == nil || s.kanbanRefreshes == nil {
		return data
	}
	data.Kanban = s.kanbanRefreshes.apply(kanbanRefreshFeedbackKey(data), data.Kanban, data.Snapshot)
	return data
}

func (s *Server) withKanbanRevertFeedback(data templates.DashboardData) templates.DashboardData {
	if s == nil || s.kanbanMutations == nil || strings.TrimSpace(data.Kanban.Feedback) != "" {
		return data
	}
	notices := s.kanbanRevertNotices(data)
	if len(notices) == 0 {
		return data
	}
	data.Kanban.Feedback = kanbanRevertFeedback(notices)
	data.Kanban.FeedbackKind = "error"
	return data
}

func (s *Server) kanbanRevertNotices(data templates.DashboardData) []kanbanstate.RevertNotice {
	if s == nil || s.kanbanMutations == nil {
		return nil
	}
	projectID := strings.TrimSpace(data.ProjectID)
	if projectID != "" {
		return s.kanbanMutations.ConsumeRevertNotices("project:"+projectID, projectID)
	}
	return s.kanbanMutations.ConsumeRevertNotices("", "")
}

func kanbanRevertFeedback(notices []kanbanstate.RevertNotice) string {
	messages := make([]string, 0, len(notices))
	for _, notice := range notices {
		identifier := strings.TrimSpace(notice.Identifier)
		if identifier == "" {
			identifier = "card"
		}
		from := strings.TrimSpace(notice.From)
		if from == "" {
			from = "the requested state"
		}
		to := strings.TrimSpace(notice.To)
		if to == "" {
			to = "the tracker state"
		}
		messages = append(messages, fmt.Sprintf("Move of %s to %s was not confirmed by the tracker; reverted to %s.", identifier, from, to))
	}
	return strings.Join(messages, " ")
}

func kanbanRefreshFeedbackKey(data templates.DashboardData) string {
	if projectID := strings.TrimSpace(data.ProjectID); projectID != "" {
		return "project:" + projectID
	}
	return "fleet"
}

func (s *Server) dashboardProject(selectedProjectID string, projects []templates.ProjectSmallMultiple, snapshot telemetry.Snapshot) (templates.ProjectSmallMultiple, bool) {
	selectedProjectID = strings.TrimSpace(selectedProjectID)
	if selectedProjectID == "" {
		return templates.ProjectSmallMultiple{}, false
	}
	for _, project := range projects {
		if strings.TrimSpace(project.ID) == selectedProjectID {
			return project, true
		}
	}
	if projectSnapshot, ok := projectSnapshotForID(snapshot, selectedProjectID); ok {
		return templates.ProjectSmallMultiple{
			ID:    projectID(projectSnapshot.Project),
			Name:  strings.TrimSpace(projectSnapshot.Project.DisplayName),
			URL:   strings.TrimSpace(projectSnapshot.Project.URL),
			Color: strings.TrimSpace(projectSnapshot.Project.Color),
		}, true
	}
	return templates.ProjectSmallMultiple{}, false
}

func (s *Server) sidebarProjectContext(selectedProjectID string, projects []templates.ProjectSmallMultiple, snapshot telemetry.Snapshot) (string, string, bool) {
	project, ok := s.dashboardProject(selectedProjectID, projects, snapshot)
	if !ok {
		return "", "", false
	}
	name := strings.TrimSpace(project.Name)
	if name == "" {
		name = strings.TrimSpace(project.ID)
	}
	return strings.TrimSpace(project.ID), name, true
}

func (s *Server) latestSnapshot(ctx context.Context) telemetry.Snapshot {
	snapshot, ok := s.hub.Latest()
	if !ok {
		return s.withManualRefresh(s.enrichSnapshot(ctx, telemetry.Snapshot{}))
	}
	return s.withManualRefresh(s.cachedEnrichedSnapshot(ctx, snapshot))
}

func (s *Server) health(c echo.Context) error {
	if _, _, err := s.demoScenarioOrError(c); err != nil {
		return err
	}
	status := "ok"
	sessionsRemaining := 0
	updateStatus := telemetry.Update{}
	if s.hub != nil {
		if snapshot, ok := s.hub.Latest(); ok {
			updateStatus = snapshot.Update
			if snapshot.Shutdown.Draining {
				status = "draining"
				sessionsRemaining = snapshot.Shutdown.SessionsRemaining
			}
		}
	}
	checks := map[string]string{
		"hub":       configuredStatus(s.hub),
		"store":     configuredStatus(s.store),
		"registry":  configuredStatus(s.registry),
		"connector": configuredStatus(s.connector),
	}
	if s.demo != nil {
		checks["demo"] = DemoModeScreenshots
		checks["demo_clock"] = s.demo.clock
	}
	return c.JSON(http.StatusOK, healthResponse{
		Status:            status,
		Mode:              string(s.mode),
		Connector:         s.connectorName(),
		SessionsRemaining: sessionsRemaining,
		Update:            updateStatus,
		Checks:            checks,
		Budgets:           s.enforcedBudgets(),
	})
}

func (s *Server) enforcedBudgets() []healthBudget {
	if s.registry == nil {
		return nil
	}

	projects := s.registry.List()
	budgets := make([]healthBudget, 0, len(projects))
	for _, project := range projects {
		cfg, ok := project.EnforcedBudget()
		if !ok {
			continue
		}
		budgets = append(budgets, healthBudget{
			ProjectID:      string(project.ID()),
			Enabled:        cfg.Enabled,
			PerDayMaxUSD:   cfg.PerDayMaxUSD,
			PerIssueMaxUSD: cfg.PerIssueMaxUSD,
		})
	}
	return budgets
}

func (s *Server) redirectToOnboarding(c echo.Context) error {
	return c.Redirect(http.StatusFound, "/onboarding")
}

func (s *Server) redirectToDashboard(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok && scenario.Page == "onboarding" {
		return s.demoOnboarding(c, scenario)
	}
	return c.Redirect(http.StatusFound, "/")
}

func render(c echo.Context, component templ.Component) error {
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().Header().Set(echo.HeaderCacheControl, revalidateCacheControl)
	return component.Render(c.Request().Context(), c.Response().Writer)
}

func (cfg Config) logger() *slog.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return slog.Default()
}

func (cfg Config) mode() Mode {
	if cfg.Mode == ModeOnboarding {
		return ModeOnboarding
	}
	return ModeRunning
}

func (cfg Config) staticDir() string {
	return cfg.StaticDir
}

func (cfg Config) sseTickInterval() time.Duration {
	if cfg.SSETickInterval > 0 {
		return cfg.SSETickInterval
	}
	return time.Second
}

func (cfg Config) sseFragmentInterval() time.Duration {
	if cfg.SSEFragmentInterval < 0 {
		return 0
	}
	if cfg.SSEFragmentInterval > 0 {
		return cfg.SSEFragmentInterval
	}
	return defaultSSEFragmentInterval
}

func (cfg Config) sseHealthInterval() time.Duration {
	if cfg.SSEHealthInterval < 0 {
		return 0
	}
	if cfg.SSEHealthInterval > 0 {
		return cfg.SSEHealthInterval
	}
	return defaultSSEHealthInterval
}

func (cfg Config) sseMetricsInterval() time.Duration {
	if cfg.SSEMetricsInterval < 0 {
		return 0
	}
	if cfg.SSEMetricsInterval > 0 {
		return cfg.SSEMetricsInterval
	}
	return defaultSSEMetricsInterval
}

func (cfg Config) httpReadHeaderTimeout() time.Duration {
	if cfg.HTTPReadHeaderTimeout > 0 {
		return cfg.HTTPReadHeaderTimeout
	}
	return defaultHTTPReadHeaderTimeout
}

func (cfg Config) httpIdleTimeout() time.Duration {
	if cfg.HTTPIdleTimeout > 0 {
		return cfg.HTTPIdleTimeout
	}
	return defaultHTTPIdleTimeout
}

func (cfg Config) workflowPath() string {
	if cfg.WorkflowPath != "" {
		return cfg.WorkflowPath
	}
	return "WORKFLOW.md"
}

func (cfg Config) dashboardURL() string {
	if dashboardURL := strings.TrimSpace(cfg.DashboardURL); dashboardURL != "" {
		return dashboardURL
	}
	return "http://localhost:4000"
}

func (cfg Config) lookupEnv() func(string) string {
	if cfg.LookupEnv != nil {
		return cfg.LookupEnv
	}
	return defaultLookupEnv
}

func (cfg Config) kanban() workflowconfig.Kanban {
	kanban := cfg.Kanban
	kanban.Normalize()
	return kanban
}

func (cfg Config) kanbanWorkflow(kanban workflowconfig.Kanban) workflowconfig.Config {
	workflow := cfg.KanbanWorkflow
	if workflow.Tracker.Kind == "" &&
		len(workflow.Tracker.ActiveStates) == 0 &&
		len(workflow.Tracker.ObservedStates) == 0 &&
		len(workflow.Tracker.TerminalStates) == 0 {
		workflow = workflowconfig.Default()
	}
	workflow.Server.Kanban = kanban
	return workflow
}

func (cfg Config) githubWebhookSecret(workflow workflowconfig.Config) string {
	if secret := strings.TrimSpace(cfg.GitHubWebhookSecret); secret != "" {
		return secret
	}
	return strings.TrimSpace(workflow.Tracker.GitHubWebhookSecret)
}

func (cfg Config) pricing() budget.PricingTable {
	if cfg.Pricing != nil {
		return cfg.Pricing
	}
	return budget.DefaultPricingTable()
}

func configuredStatus(value any) string {
	if value == nil {
		return "missing"
	}
	return "configured"
}

func (s *Server) connectorName() string {
	if s.connector == nil {
		return ""
	}
	return s.connector.Name()
}

type healthResponse struct {
	Status            string            `json:"status"`
	Mode              string            `json:"mode"`
	Connector         string            `json:"connector"`
	SessionsRemaining int               `json:"sessions_remaining,omitempty"`
	Update            telemetry.Update  `json:"update,omitzero"`
	Checks            map[string]string `json:"checks"`
	Budgets           []healthBudget    `json:"budgets,omitempty"`
}

type healthBudget struct {
	ProjectID      string  `json:"project_id"`
	Enabled        bool    `json:"enabled"`
	PerDayMaxUSD   float64 `json:"per_day_max_usd"`
	PerIssueMaxUSD float64 `json:"per_issue_max_usd"`
}
