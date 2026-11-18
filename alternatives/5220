package web

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type Dependencies struct{}

type Mode string

const ModeOnboarding Mode = "onboarding"

type StartupLifecycleState string

const (
	StartupLifecycleStarting StartupLifecycleState = "starting"
	StartupLifecycleReady    StartupLifecycleState = "ready"
	StartupLifecycleFailed   StartupLifecycleState = "failed"
)

type StartupLifecycle struct {
	state atomic.Uint32
}

func NewStartupLifecycle() *StartupLifecycle {
	return &StartupLifecycle{}
}

func (l *StartupLifecycle) State() StartupLifecycleState {
	if l == nil {
		return StartupLifecycleReady
	}
	switch l.state.Load() {
	case 0:
		return StartupLifecycleStarting
	case 1:
		return StartupLifecycleReady
	default:
		return StartupLifecycleFailed
	}
}

func (l *StartupLifecycle) MarkReady() {
	l.state.Store(1)
}

func (l *StartupLifecycle) MarkFailed() {
	l.state.Store(2)
}

const (
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPIdleTimeout       = 2 * time.Minute
	stateEndpointTargetLatencyMS = 500
)

type Config struct {
	Logger                *slog.Logger
	Mode                  Mode
	StaticDir             string
	SSEFragmentInterval   time.Duration
	HTTPReadHeaderTimeout time.Duration
	HTTPIdleTimeout       time.Duration
	WorkflowPath          string
	Version               string
	Build                 buildinfo.Info
	DashboardURL          string
	GlobalConfig          globalconfig.Config
	GlobalConfigSource    func() globalconfig.Config
	LookupEnv             func(string) string
	ServerAddress         string
}

type Server struct {
	echo               *echo.Echo
	logger             *slog.Logger
	build              buildinfo.Info
	globalConfig       globalconfig.Config
	globalConfigSource func() globalconfig.Config
	lookupEnv          func(string) string
	serverAddr         string
	assets             staticAssets
}

func NewServer(cfg Config, _ Dependencies) (*Server, error) {
	if err := rejectSessionAuth(cfg.GlobalConfig.Auth); err != nil {
		return nil, err
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Server.ReadHeaderTimeout = cfg.httpReadHeaderTimeout()
	e.Server.IdleTimeout = cfg.httpIdleTimeout()

	server := &Server{
		echo:               e,
		logger:             cfg.logger(),
		build:              cfg.Build,
		globalConfig:       cfg.GlobalConfig,
		globalConfigSource: cfg.GlobalConfigSource,
		lookupEnv:          cfg.lookupEnv(),
		serverAddr:         strings.TrimSpace(cfg.ServerAddress),
		assets:             newStaticAssets(cfg.StaticDir),
	}
	e.HTTPErrorHandler = server.handleHTTPError
	e.Use(server.privateDashboardAccess, server.uiAPICookie)
	e.GET("/static/*", server.assets.serve)
	e.GET("/health", server.health)
	e.GET(openAPIPath, server.openAPI)
	server.warnIfAPITokenMissingOnNonLoopback()

	return server, nil
}

func rejectSessionAuth(authConfig globalconfig.Auth) error {
	if authConfig.MagicLinkEnabled() {
		if _, err := authConfig.LinkTTLDuration(); err != nil {
			return err
		}
		if _, err := authConfig.SessionTTLDuration(); err != nil {
			return err
		}
		if _, err := auth.NewSMTPSender(auth.SMTPConfig{
			Host:     authConfig.SMTP.Host,
			Port:     authConfig.SMTP.NormalizedPort(),
			Username: authConfig.SMTP.Username,
			Password: authConfig.SMTP.Password,
			From:     authConfig.SMTP.From,
		}); err != nil {
			return fmt.Errorf("configure magic link sender: %w", err)
		}
		return fmt.Errorf("configure magic link auth: %w", auth.ErrMissingStore)
	}
	if authConfig.OIDCEnabled() {
		if _, err := authConfig.SessionTTLDuration(); err != nil {
			return err
		}
		return fmt.Errorf("configure oidc sessions: %w", auth.ErrMissingStore)
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	return s.echo
}

func (s *Server) StartListener(listener net.Listener) error {
	s.logger.Info("starting web server", "addr", listener.Addr().String())
	return s.echo.Server.Serve(listener)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("stopping web server")
	return s.echo.Shutdown(ctx)
}

func (s *Server) health(c echo.Context) error {
	idle := healthStateEndpoint{Status: "idle", TargetLatencyMS: stateEndpointTargetLatencyMS}
	return c.JSON(http.StatusOK, healthResponse{
		CandidatesMissingVsTracker: map[string]*int{},
		Status:                     "ok",
		Ready:                      true,
		Lifecycle:                  StartupLifecycleReady,
		Version:                    s.build.Version,
		Commit:                     s.build.Commit,
		ProjectStatus:              "unknown",
		Mode:                       string(ModeOnboarding),
		Checks: map[string]string{
			"hub":              "configured",
			"store":            "missing",
			"registry":         "configured",
			"connector":        "missing",
			"worker_processes": "missing",
		},
		Environment:    healthEnvironment{Path: os.Getenv("PATH")},
		AgentMemory:    []struct{}{},
		StateEndpoints: healthStateEndpoints{Fleet: idle, Project: idle},
		TickLiveness:   []telemetry.TickLiveness{},
	})
}

func (cfg Config) logger() *slog.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return slog.Default()
}

func (cfg Config) lookupEnv() func(string) string {
	if cfg.LookupEnv != nil {
		return cfg.LookupEnv
	}
	return defaultLookupEnv
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

type healthResponse struct {
	CandidatesMissingVsTracker map[string]*int `json:"candidates_missing_vs_tracker"`

	Status                 string                           `json:"status"`
	Ready                  bool                             `json:"ready"`
	Lifecycle              StartupLifecycleState            `json:"lifecycle"`
	Version                string                           `json:"version"`
	Commit                 string                           `json:"commit"`
	ProjectStatus          string                           `json:"project_status"`
	Mode                   string                           `json:"mode"`
	Connector              string                           `json:"connector"`
	Checks                 map[string]string                `json:"checks"`
	Environment            healthEnvironment                `json:"environment"`
	Dispatch               telemetry.DispatchStatus         `json:"dispatch"`
	Refresh                telemetry.Refresh                `json:"refresh"`
	MemoryPressure         telemetry.MemoryPressure         `json:"memory_pressure"`
	IOPressure             telemetry.IOPressure             `json:"io_pressure"`
	CPUPressure            telemetry.CPUPressure            `json:"cpu_pressure"`
	AgentMemory            []struct{}                       `json:"agent_memory"`
	SnapshotAgeSeconds     int64                            `json:"snapshot_age_seconds"`
	StateEndpoints         healthStateEndpoints             `json:"state_endpoints"`
	TickLiveness           []telemetry.TickLiveness         `json:"tick_liveness"`
	OrphanedAgentProcesses telemetry.OrphanedAgentProcesses `json:"orphaned_agent_processes"`
}

type healthEnvironment struct {
	Path string `json:"path"`
}
