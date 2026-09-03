package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPIdleTimeout       = 2 * time.Minute
)

type Service struct {
	echo      *echo.Echo
	database  *database
	tracker   tracker.Tracker
	config    Config
	ready     atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

type healthResponse struct {
	Status        string `json:"status"`
	SchemaVersion int64  `json:"schema_version,omitempty"`
	Version       string `json:"version,omitempty"`
}

func Open(ctx context.Context, cfg Config) (*Service, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalized()
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	workTracker, err := tracker.NewStore(database)
	if err != nil {
		return nil, errors.Join(err, database.Close())
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Server.ReadHeaderTimeout = defaultHTTPReadHeaderTimeout
	e.Server.IdleTimeout = defaultHTTPIdleTimeout
	service := &Service{echo: e, database: database, tracker: workTracker, config: cfg}
	e.GET("/health", service.health)
	service.ready.Store(true)
	return service, nil
}

func (s *Service) Tracker() tracker.Tracker {
	return s.tracker
}

func Run(ctx context.Context, cfg Config) (resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalized()
	service, err := Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, service.Close())
	}()

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for hub requests: %w", err)
	}
	cfg.Logger.Info("hub serving", "address", listener.Addr().String())

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- service.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		shutdownErr := service.Shutdown(shutdownContext)
		cancel()
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, service.Close())
		}
		return errors.Join(shutdownErr, <-serveResult)
	}
}

func (s *Service) Handler() http.Handler {
	return s.echo
}

func (s *Service) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("hub listener is required")
	}
	err := s.echo.Server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve hub requests: %w", err)
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.ready.Store(false)
	err := s.echo.Shutdown(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("shut down hub server: %w", err)
	}
	return nil
}

func (s *Service) Backup(ctx context.Context, destination string) error {
	if !s.ready.Load() {
		return ErrNotReady
	}
	return s.database.backup(ctx, destination)
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.ready.Store(false)
		httpErr := s.echo.Close()
		if errors.Is(httpErr, http.ErrServerClosed) {
			httpErr = nil
		}
		s.closeErr = errors.Join(httpErr, s.database.Close())
	})
	return s.closeErr
}

func (s *Service) health(c echo.Context) error {
	if !s.ready.Load() || s.database.health(c.Request().Context()) != nil {
		return c.JSON(http.StatusServiceUnavailable, healthResponse{Status: "unavailable"})
	}
	return c.JSON(http.StatusOK, healthResponse{
		Status:        "ok",
		SchemaVersion: s.database.schemaVersion,
		Version:       s.config.Version,
	})
}
