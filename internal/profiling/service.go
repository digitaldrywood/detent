package profiling

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type Service struct {
	mu            sync.Mutex
	ctx           context.Context
	logger        *slog.Logger
	defaultDir    string
	listenAddr    string
	listener      net.Listener
	server        *http.Server
	serveDone     chan struct{}
	capture       Capture
	captureCancel context.CancelFunc
	captureDone   chan struct{}
	initialized   bool
	closed        bool
}

func New(ctx context.Context, dataDir, process string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{ctx: ctx, logger: logger.With("process", process), defaultDir: filepath.Join(dataDir, "profiles", process)}
}

func (s *Service) Apply(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg = cfg.Normalized()
	if cfg.Capture.Dir == "" {
		cfg.Capture.Dir = s.defaultDir
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return errors.New("profiling service is stopped")
	}
	listenerErr := s.applyListener(cfg.ListenAddr)
	if cfg.Capture != s.capture {
		s.stopCapture()
		s.capture = cfg.Capture
		if cfg.Capture.Enabled || !s.initialized {
			ctx, cancel := context.WithCancel(s.ctx)
			s.captureCancel = cancel
			s.captureDone = make(chan struct{})
			if cfg.Capture.Enabled {
				runtime.SetMutexProfileFraction(100)
				runtime.SetBlockProfileRate(10_000_000)
			}
			go s.runCapture(ctx, cfg.Capture, s.captureDone)
		}
	}
	s.initialized = true
	return listenerErr
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.stopListener()
	s.stopCapture()
}

func (s *Service) applyListener(address string) error {
	if address == s.listenAddr {
		return nil
	}
	if address == "" {
		s.stopListener()
		return nil
	}
	bind, err := loopbackAddress(address)
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(s.ctx, "tcp", bind)
	if err != nil {
		return err
	}
	s.stopListener()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", httppprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", httppprof.Trace)
	ctx, cancel := context.WithCancel(s.ctx)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	s.listener, s.server, s.listenAddr = listener, server, address
	s.serveDone = make(chan struct{})
	done := s.serveDone
	go func() {
		defer close(done)
		defer cancel()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Warn("profiling listener failed", "error", err)
		}
	}()
	s.logger.Info("profiling listener started", "address", listener.Addr().String())
	return nil
}

func (s *Service) stopListener() {
	if s.server != nil {
		if err := s.server.Close(); err != nil {
			s.logger.Warn("close profiling listener", "error", err)
		}
		<-s.serveDone
		s.server, s.listener, s.serveDone, s.listenAddr = nil, nil, nil, ""
		s.logger.Info("profiling listener stopped")
	}
}

func (s *Service) stopCapture() {
	if s.captureCancel != nil {
		s.captureCancel()
		<-s.captureDone
		s.captureCancel, s.captureDone = nil, nil
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	}
}

func (s *Service) runCapture(ctx context.Context, cfg Capture, done chan struct{}) {
	defer close(done)
	s.prune(ctx, cfg)
	if !cfg.Enabled {
		return
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := captureBundle(ctx, cfg, time.Now()); err != nil && ctx.Err() == nil {
				s.logger.Warn("profiling capture failed", "error", err)
			}
			s.prune(ctx, cfg)
		}
	}
}

func (s *Service) prune(ctx context.Context, cfg Capture) {
	if err := pruneBundles(ctx, cfg, time.Now()); err != nil && ctx.Err() == nil {
		s.logger.Warn("profiling retention failed", "error", err)
	}
}
