package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/hub"
	"github.com/digitaldrywood/symphony/internal/store"
	"github.com/digitaldrywood/symphony/internal/telemetry"
	"github.com/digitaldrywood/symphony/internal/web/templates"
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
	Registry  any
	Connector connector.Connector
	Refresher Refresher
}

type Mode string

const (
	ModeRunning    Mode = "running"
	ModeOnboarding Mode = "onboarding"
)

type Config struct {
	Logger          *slog.Logger
	Mode            Mode
	StaticDir       string
	SSETickInterval time.Duration
	WorkflowPath    string
}

type Server struct {
	echo      *echo.Echo
	hub       *hub.Hub[telemetry.Snapshot]
	store     store.Store
	registry  any
	connector connector.Connector
	refresher Refresher
	logger    *slog.Logger
	mode      Mode
	tickEvery time.Duration
	workflow  string
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

	server := &Server{
		echo:      e,
		hub:       deps.Hub,
		store:     deps.Store,
		registry:  deps.Registry,
		connector: deps.Connector,
		refresher: deps.Refresher,
		logger:    cfg.logger(),
		mode:      mode,
		tickEvery: cfg.sseTickInterval(),
		workflow:  cfg.workflowPath(),
	}
	e.HTTPErrorHandler = server.handleHTTPError
	server.registerRoutes(cfg.staticDir())

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

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("stopping web server")
	return s.echo.Shutdown(ctx)
}

func (s *Server) registerRoutes(staticDir string) {
	s.echo.Static("/static", staticDir)
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
	s.echo.GET("/events", s.events)
	s.echo.GET("/onboarding", s.redirectToDashboard)
	s.echo.POST("/onboarding/tracker", s.onboardingTracker)
	s.echo.POST("/onboarding/credentials", s.onboardingCredentials)
	s.echo.POST("/onboarding/project", s.onboardingProject)
	s.echo.POST("/onboarding/agent", s.onboardingAgent)
	s.echo.POST("/onboarding/write", s.onboardingWrite)
	s.echo.GET("/api/v1/state", s.apiState)
	s.echo.POST("/api/v1/refresh", s.apiRefresh)
	s.echo.GET("/api/v1/refresh", s.methodNotAllowed)
	s.echo.GET("/api/v1/*", s.apiIssue)
}

func (s *Server) dashboard(c echo.Context) error {
	return render(c, templates.Dashboard(templates.DashboardData{
		Title:         "Symphony",
		ConnectorName: s.connector.Name(),
		Snapshot:      s.latestSnapshot(c.Request().Context()),
	}))
}

func (s *Server) latestSnapshot(ctx context.Context) telemetry.Snapshot {
	sub, err := s.hub.Subscribe(ctx)
	if err != nil {
		return telemetry.Snapshot{}
	}
	defer sub.Close()

	select {
	case snapshot, ok := <-sub.C():
		if ok {
			return snapshot
		}
	default:
	}
	return telemetry.Snapshot{}
}

func (s *Server) health(c echo.Context) error {
	return c.JSON(http.StatusOK, healthResponse{
		Status:    "ok",
		Mode:      string(s.mode),
		Connector: s.connectorName(),
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
	if cfg.StaticDir != "" {
		return cfg.StaticDir
	}
	return "static"
}

func (cfg Config) sseTickInterval() time.Duration {
	if cfg.SSETickInterval > 0 {
		return cfg.SSETickInterval
	}
	return time.Second
}

func (cfg Config) workflowPath() string {
	if cfg.WorkflowPath != "" {
		return cfg.WorkflowPath
	}
	return "WORKFLOW.md"
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
	Status    string            `json:"status"`
	Mode      string            `json:"mode"`
	Connector string            `json:"connector"`
	Checks    map[string]string `json:"checks"`
}
