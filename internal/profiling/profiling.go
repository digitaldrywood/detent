package profiling

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"runtime"
	"sync"
	"time"
)

type Service struct {
	mu            sync.Mutex
	ctx           context.Context
	logger        *slog.Logger
	defaultDir    string
	config        Config
	server        *http.Server
	listener      net.Listener
	listenerDone  chan struct{}
	cancelCapture context.CancelFunc
	captureDone   chan struct{}
	startupDone   chan struct{}
	initialized   bool
	closed        bool
}

func New(ctx context.Context, defaultDir string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{ctx: ctx, defaultDir: defaultDir, logger: logger}
}

func (s *Service) Apply(config Config) {
	if config == (Config{}) {
		config = Default()
	}
	if err := config.Validate(); err != nil {
		s.logger.Warn("profiling configuration rejected", "error", err)
		return
	}
	if config.Capture.Dir == "" {
		config.Capture.Dir = s.defaultDir
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return
	}
	if s.startupDone != nil {
		<-s.startupDone
		s.startupDone = nil
	}
	if !s.initialized {
		s.initialized = true
		if !config.Capture.Enabled {
			done := make(chan struct{})
			s.startupDone = done
			go func() {
				defer close(done)
				s.prune(config.Capture)
			}()
		}
	}
	if config.ListenAddr != s.config.ListenAddr || (config.ListenAddr != "" && s.server == nil) {
		s.stopListener()
		if config.ListenAddr != "" {
			s.startListener(config.ListenAddr)
		}
	}
	if config.Capture != s.config.Capture {
		s.stopCapture()
		if config.Capture.Enabled {
			runtime.SetMutexProfileFraction(5)
			runtime.SetBlockProfileRate(1_000_000)
			ctx, cancel := context.WithCancel(s.ctx)
			s.cancelCapture = cancel
			s.captureDone = make(chan struct{})
			go s.captureLoop(ctx, config.Capture, s.captureDone)
		}
	}
	s.config = config
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.stopListener()
	s.stopCapture()
	if s.startupDone != nil {
		<-s.startupDone
		s.startupDone = nil
	}
}

func (s *Service) startListener(address string) {
	address, err := loopbackAddress(address)
	if err != nil {
		s.logger.Warn("profiling listener failed", "error", err)
		return
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		s.logger.Warn("profiling listener failed", "address", address, "error", err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", httppprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", httppprof.Trace)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return s.ctx }}
	s.server, s.listener = server, listener
	done := make(chan struct{})
	s.listenerDone = done
	s.logger.Info("profiling listener started", "address", listener.Addr().String())
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Warn("profiling listener stopped", "error", err)
		}
	}()
}

func (s *Service) stopListener() {
	if s.server == nil {
		return
	}
	if err := s.server.Close(); err != nil {
		s.logger.Warn("profiling listener close failed", "error", err)
	}
	<-s.listenerDone
	s.server, s.listener, s.listenerDone = nil, nil, nil
}

func (s *Service) stopCapture() {
	if s.cancelCapture == nil {
		return
	}
	s.cancelCapture()
	<-s.captureDone
	s.cancelCapture, s.captureDone = nil, nil
	runtime.SetMutexProfileFraction(0)
	runtime.SetBlockProfileRate(0)
}

func (s *Service) captureLoop(ctx context.Context, config CaptureConfig, done chan struct{}) {
	defer close(done)
	s.prune(config)
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeBundle(ctx, config, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("profiling capture failed", "directory", config.Dir, "error", err)
			}
			s.prune(config)
		}
	}
}

func (s *Service) prune(config CaptureConfig) {
	if err := pruneBundles(config.Dir, config.MaxAge, int64(config.MaxBytes), time.Now()); err != nil {
		s.logger.Warn("profiling retention failed", "directory", config.Dir, "error", err)
	}
}
