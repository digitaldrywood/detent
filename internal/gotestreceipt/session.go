package gotestreceipt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

const (
	requestTimeout  = 15 * time.Second
	maxRequestBytes = 2 << 20
	socketName      = "receipts.sock"
	maxSocketPath   = 100
	clientBaseURL   = "http://receipts"
	executePath     = "/execute"
	maxReasonBytes  = 512
	shutdownTimeout = 5 * time.Second
)

type Stats struct {
	Lookups  int64
	Hits     int64
	Records  int64
	Failures int64
	Refused  int64
}

type Session struct {
	root        string
	workDir     string
	goRootOnce  sync.Once
	goRootDir   string
	attestOnce  sync.Once
	attestation Attestation
	attestErr   error
	dir         string
	socket      string
	server      *http.Server
	listener    net.Listener
	store       Store
	logger      *slog.Logger
	served      chan error
	handlers    sync.WaitGroup
	once        sync.Once
	lookups     atomic.Int64
	hits        atomic.Int64
	records     atomic.Int64
	failures    atomic.Int64
	refused     atomic.Int64
}

func Start(ctx context.Context, store Store, logger *slog.Logger, scratch, root string) (*Session, error) {
	if store == nil {
		return nil, errors.New("go test receipt store is required")
	}
	if !filepath.IsAbs(scratch) {
		return nil, errors.New("go test receipt scratch directory must be absolute")
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return nil, errors.New("go test receipt module root must be an absolute directory below the filesystem root")
	}
	if logger == nil {
		logger = slog.Default()
	}
	dir, err := os.MkdirTemp(scratch, "dgtr")
	if err != nil {
		return nil, fmt.Errorf("create receipt socket directory: %w", err)
	}
	workDir := filepath.Join(dir, "work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("create go test work directory: %w", err), os.RemoveAll(dir))
	}
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	socket := filepath.Join(dir, socketName)
	var config net.ListenConfig
	var listener net.Listener
	err = withSocketPath(socket, func(path string) error {
		var listenErr error
		listener, listenErr = config.Listen(ctx, "unix", path)
		return listenErr
	})
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err != nil {
		if listener != nil {
			err = errors.Join(err, listener.Close())
		}
		return nil, errors.Join(fmt.Errorf("listen for go test receipts: %w", err), os.RemoveAll(dir))
	}
	session := &Session{root: root, workDir: workDir, dir: dir, socket: socket, listener: listener, store: store, logger: logger, served: make(chan error, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+executePath, session.execute)
	session.server = &http.Server{Handler: mux, ReadHeaderTimeout: requestTimeout, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		err := session.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		session.served <- err
	}()
	return session, nil
}

func (s *Session) Environment() map[string]string {
	return map[string]string{SocketEnvironment: s.socket, "GOTMPDIR": s.workDir}
}

func (s *Session) Close(ctx context.Context) (Stats, error) {
	var err error
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if shutdownErr := s.server.Shutdown(ctx); shutdownErr != nil {
			err = errors.Join(shutdownErr, s.server.Close())
		}
		joined := make(chan struct{})
		go func() {
			s.handlers.Wait()
			close(joined)
		}()
		select {
		case <-joined:
		case <-ctx.Done():
			err = errors.Join(err, errors.New("go test receipt handlers did not finish"))
		}
		err = errors.Join(err, <-s.served, os.RemoveAll(s.dir))
	})
	return s.Stats(), err
}

func (s *Session) Stats() Stats {
	return Stats{Lookups: s.lookups.Load(), Hits: s.hits.Load(), Records: s.records.Load(), Failures: s.failures.Load(), Refused: s.refused.Load()}
}

func (s *Session) goRoot(ctx context.Context) string {
	s.goRootOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "env", "GOROOT")
		procgroup.Configure(ctx, cmd)
		cmd.Dir = s.root
		cmd.WaitDelay = testWaitDelay
		output, err := cmd.Output()
		if err != nil {
			s.logger.Warn("resolve GOROOT for go test receipts; GOROOT reads are not reusable", "error", err)
			return
		}
		if dir := strings.TrimSpace(string(output)); filepath.IsAbs(dir) {
			s.goRootDir = filepath.Clean(dir)
		}
	})
	return s.goRootDir
}

func (s *Session) attest(ctx context.Context) (Attestation, error) {
	s.attestOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		attestation, err := s.store.Attest(ctx)
		if err == nil {
			err = attestation.Validate()
		}
		if err != nil {
			s.failures.Add(1)
			s.logger.Warn("go test receipt attestation failed; test binaries execute", "error", err)
			err = fmt.Errorf("attest go test receipt session: %w", err)
		}
		s.attestation, s.attestErr = attestation, err
	})
	return s.attestation, s.attestErr
}

func (s *Session) lookup(ctx context.Context, key string) ([]Candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	s.lookups.Add(1)
	candidates, err := s.store.Lookup(ctx, key)
	if err != nil {
		s.failures.Add(1)
		return nil, err
	}
	if len(candidates) > MaxCandidates {
		candidates = candidates[:MaxCandidates]
	}
	return candidates, nil
}

func (s *Session) refuse(pkg string, reason error) {
	s.refused.Add(1)
	s.logger.Info("go test receipt not recorded", "package", truncate(pkg), "reason", truncate(reason.Error()))
}

func (s *Session) record(ctx context.Context, receipt Receipt) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	receipt.Attestation = s.attestation.ID
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := s.store.Record(ctx, receipt); err != nil {
		s.failures.Add(1)
		return err
	}
	s.records.Add(1)
	return nil
}

func truncate(value string) string {
	if len(value) > maxReasonBytes/4 {
		return strings.ToValidUTF8(value[:maxReasonBytes/4], "")
	}
	return value
}
