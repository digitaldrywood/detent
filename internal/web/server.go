package web

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hub"
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
}

type Mode string

const (
	ModeRunning    Mode = "running"
	ModeOnboarding Mode = "onboarding"
)

const (
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPIdleTimeout       = 2 * time.Minute
	sidebarStateCookieName       = "sidebar_state"
)

type Config struct {
	Logger                *slog.Logger
	Mode                  Mode
	StaticDir             string
	SSETickInterval       time.Duration
	HTTPReadHeaderTimeout time.Duration
	HTTPIdleTimeout       time.Duration
	WorkflowPath          string
	Version               string
	Build                 buildinfo.Info
	DashboardURL          string
	Pricing               budget.PricingTable
	GlobalConfig          globalconfig.Config
	GlobalConfigSource    func() globalconfig.Config
	Hostname              func() (string, error)
	ConfigPathRule        globalconfig.PathRule
	RuntimeDBPath         string
	RuntimeLogPath        string
	ServerAddress         string
}

type Server struct {
	echo               *echo.Echo
	hub                *hub.Hub[telemetry.Snapshot]
	store              store.Store
	registry           *project.Registry
	connector          connector.Connector
	refresher          Refresher
	logger             *slog.Logger
	mode               Mode
	tickEvery          time.Duration
	workflow           string
	version            string
	build              buildinfo.Info
	dashboardURL       string
	pricing            budget.PricingTable
	globalConfig       globalconfig.Config
	globalConfigSource func() globalconfig.Config
	hostname           func() (string, error)
	configRule         globalconfig.PathRule
	dbPath             string
	logPath            string
	serverAddr         string
	assets             staticAssets
	projects           *projectSmallMultipleRecorder
	snapshots          *snapshotEnrichmentCache
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

	server := &Server{
		echo:               e,
		hub:                deps.Hub,
		store:              deps.Store,
		registry:           deps.Registry,
		connector:          deps.Connector,
		refresher:          deps.Refresher,
		logger:             cfg.logger(),
		mode:               mode,
		tickEvery:          cfg.sseTickInterval(),
		workflow:           cfg.workflowPath(),
		version:            strings.TrimSpace(cfg.Version),
		build:              cfg.Build,
		dashboardURL:       cfg.dashboardURL(),
		pricing:            cfg.pricing(),
		globalConfig:       cfg.GlobalConfig,
		globalConfigSource: cfg.globalConfigSource(),
		hostname:           cfg.hostname(),
		configRule:         cfg.ConfigPathRule,
		dbPath:             strings.TrimSpace(cfg.RuntimeDBPath),
		logPath:            strings.TrimSpace(cfg.RuntimeLogPath),
		serverAddr:         strings.TrimSpace(cfg.ServerAddress),
		assets:             newStaticAssets(cfg.staticDir()),
		projects:           newProjectSmallMultipleRecorder(),
		snapshots:          newSnapshotEnrichmentCache(),
	}
	e.HTTPErrorHandler = server.handleHTTPError
	server.registerRoutes()

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
	return s.echo.Shutdown(ctx)
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

	s.echo.GET("/", s.dashboard)
	s.echo.GET("/projects/*", s.projectDashboard)
	s.echo.GET("/settings", s.settings)
	s.echo.GET("/reports", s.reports)
	s.echo.GET("/events", s.events)
	s.echo.GET("/onboarding", s.redirectToDashboard)
	s.echo.POST("/onboarding/tracker", s.onboardingTracker)
	s.echo.POST("/onboarding/credentials", s.onboardingCredentials)
	s.echo.POST("/onboarding/project", s.onboardingProject)
	s.echo.POST("/onboarding/agent", s.onboardingAgent)
	s.echo.POST("/onboarding/write", s.onboardingWrite)
	s.echo.GET("/api/v1/state", s.apiState)
	s.echo.GET("/api/v1/timeseries", s.apiTimeSeries)
	s.echo.GET("/api/v1/projects/*", s.apiProject)
	s.echo.POST("/api/v1/refresh", s.apiRefresh)
	s.echo.GET("/api/v1/refresh", s.methodNotAllowed)
	s.echo.GET("/api/v1/usage", s.apiUsage)
	s.echo.GET("/api/v1/*", s.apiIssue)
}

func (s *Server) dashboard(c echo.Context) error {
	ctx := c.Request().Context()
	data := s.dashboardData(ctx, s.latestSnapshot(ctx))
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	return render(c, templates.Dashboard(data))
}

func (s *Server) projectDashboard(c echo.Context) error {
	ctx := c.Request().Context()
	data, ok := s.projectDashboardData(ctx, projectRouteParam(c), s.latestSnapshot(ctx))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())
	return render(c, templates.Dashboard(data))
}

func dashboardSidebarCollapsed(r *http.Request) bool {
	cookie, err := r.Cookie(sidebarStateCookieName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(cookie.Value) == "false"
}

func projectRouteParam(c echo.Context) string {
	projectID := strings.Trim(strings.TrimSpace(c.Param("*")), "/")
	if unescaped, err := url.PathUnescape(projectID); err == nil {
		return strings.Trim(strings.TrimSpace(unescaped), "/")
	}
	return projectID
}

func (s *Server) dashboardData(ctx context.Context, snapshot telemetry.Snapshot) templates.DashboardData {
	instanceName := s.instanceName()
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
		Assets:          s.assets.templatePaths(),
		ActiveNav:       "fleet",
	}
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
	})
	name := strings.TrimSpace(project.Name)
	if name == "" {
		name = strings.TrimSpace(project.ID)
	}
	instanceName := s.instanceName()
	return templates.DashboardData{
		Title:           instancePageTitle(instanceName, name+" - Detent"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		Version:         s.version,
		Build:           s.build,
		ConnectorName:   s.connector.Name(),
		DashboardURL:    s.dashboardURL,
		Snapshot:        scopedSnapshot,
		Projects:        projects,
		WorkflowStates:  s.projectWorkflowStates(project.ID),
		Assets:          s.assets.templatePaths(),
		ActiveNav:       "project",
		ProjectID:       strings.TrimSpace(project.ID),
		ProjectName:     name,
		ProjectPaused:   project.Paused,
	}, true
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
			ID:   projectID(projectSnapshot.Project),
			Name: strings.TrimSpace(projectSnapshot.Project.DisplayName),
			URL:  strings.TrimSpace(projectSnapshot.Project.URL),
		}, true
	}
	return templates.ProjectSmallMultiple{}, false
}

func (s *Server) latestSnapshot(ctx context.Context) telemetry.Snapshot {
	snapshot, ok := s.hub.Latest()
	if !ok {
		return s.enrichSnapshot(ctx, telemetry.Snapshot{})
	}
	return s.cachedEnrichedSnapshot(ctx, snapshot)
}

func (s *Server) health(c echo.Context) error {
	status := "ok"
	sessionsRemaining := 0
	if s.hub != nil {
		if snapshot, ok := s.hub.Latest(); ok && snapshot.Shutdown.Draining {
			status = "draining"
			sessionsRemaining = snapshot.Shutdown.SessionsRemaining
		}
	}
	return c.JSON(http.StatusOK, healthResponse{
		Status:            status,
		Mode:              string(s.mode),
		Connector:         s.connectorName(),
		SessionsRemaining: sessionsRemaining,
		Checks: map[string]string{
			"hub":       configuredStatus(s.hub),
			"store":     configuredStatus(s.store),
			"registry":  configuredStatus(s.registry),
			"connector": configuredStatus(s.connector),
		},
	})
}

func (s *Server) redirectToOnboarding(c echo.Context) error {
	return c.Redirect(http.StatusFound, "/onboarding")
}

func (s *Server) redirectToDashboard(c echo.Context) error {
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
	Checks            map[string]string `json:"checks"`
}
