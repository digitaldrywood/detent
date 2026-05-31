package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/hub"
	"github.com/digitaldrywood/symphony-go/internal/store"
	"github.com/digitaldrywood/symphony-go/internal/telemetry"
	"github.com/digitaldrywood/symphony-go/internal/web/templates"
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
}

type Config struct {
	Logger          *slog.Logger
	StaticDir       string
	SSETickInterval time.Duration
}

type Server struct {
	echo      *echo.Echo
	hub       *hub.Hub[telemetry.Snapshot]
	store     store.Store
	registry  any
	connector connector.Connector
	logger    *slog.Logger
	tickEvery time.Duration
}

func NewServer(cfg Config, deps Dependencies) (*Server, error) {
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

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	server := &Server{
		echo:      e,
		hub:       deps.Hub,
		store:     deps.Store,
		registry:  deps.Registry,
		connector: deps.Connector,
		logger:    cfg.logger(),
		tickEvery: cfg.sseTickInterval(),
	}
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
	s.echo.GET("/", s.dashboard)
	s.echo.GET("/events", s.events)
	s.echo.GET("/health", s.health)
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
		Connector: s.connector.Name(),
		Checks: map[string]string{
			"hub":       configuredStatus(s.hub),
			"store":     configuredStatus(s.store),
			"registry":  configuredStatus(s.registry),
			"connector": configuredStatus(s.connector),
		},
	})
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

func configuredStatus(value any) string {
	if value == nil {
		return "missing"
	}
	return "configured"
}

type healthResponse struct {
	Status    string            `json:"status"`
	Connector string            `json:"connector"`
	Checks    map[string]string `json:"checks"`
}
