package gotestreceipt

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	requestTimeout  = 15 * time.Second
	maxRecordBytes  = 2 << 20
	socketName      = "receipts.sock"
	maxSocketPath   = 100
	clientBaseURL   = "http://receipts"
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
	execution Execution
	dir       string
	socket    string
	server    *http.Server
	listener  net.Listener
	store     Store
	logger    *slog.Logger
	served    chan error
	handlers  sync.WaitGroup
	once      sync.Once
	lookups   atomic.Int64
	hits      atomic.Int64
	records   atomic.Int64
	failures  atomic.Int64
	refused   atomic.Int64
}

func Start(ctx context.Context, store Store, logger *slog.Logger) (*Session, error) {
	if store == nil {
		return nil, errors.New("go test receipt store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	execution := Execution{DigestKey: make([]byte, 32)}
	if source, ok := store.(ExecutionStore); ok {
		var err error
		execution, err = source.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin receipt execution: %w", err)
		}
	} else {
		if _, err := rand.Read(execution.DigestKey); err != nil {
			return nil, fmt.Errorf("create digest key: %w", err)
		}
	}
	if len(execution.DigestKey) < 32 {
		return nil, errors.New("receipt execution digest key is invalid")
	}
	dir, err := os.MkdirTemp(socketBase(), "dgtr")
	if err != nil {
		return nil, fmt.Errorf("create receipt socket directory: %w", err)
	}
	socket := filepath.Join(dir, socketName)
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", socket)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listen for go test receipts: %w", err), os.RemoveAll(dir))
	}
	session := &Session{execution: execution, dir: dir, socket: socket, listener: listener, store: store, logger: logger, served: make(chan error, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /execute", session.execute)
	session.server = &http.Server{Handler: mux, ReadHeaderTimeout: requestTimeout, ReadTimeout: requestTimeout}
	go func() {
		err := session.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		session.served <- err
	}()
	return session, nil
}

func socketBase() string {
	base := os.TempDir()
	if runtime.GOOS != "windows" && len(filepath.Join(base, "dgtr0123456789", socketName)) > maxSocketPath {
		return "/tmp"
	}
	return base
}

func (s *Session) Environment() map[string]string {
	return map[string]string{SocketEnvironment: s.socket}
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

func truncate(value string) string {
	if len(value) > maxReasonBytes/4 {
		return strings.ToValidUTF8(value[:maxReasonBytes/4], "")
	}
	return value
}

type executionStore struct{ session *Session }

func (c executionStore) Lookup(ctx context.Context, key string) ([]Candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	c.session.lookups.Add(1)
	candidates, err := c.session.store.Lookup(ctx, key)
	if err != nil {
		c.session.failures.Add(1)
	}
	if len(candidates) > MaxCandidates {
		candidates = candidates[:MaxCandidates]
	}
	return candidates, err
}
func (c executionStore) Hit(context.Context, string) error { c.session.hits.Add(1); return nil }

type Refusal struct {
	Package string
	Reason  string
}

func (c executionStore) Refuse(_ context.Context, refusal Refusal) error {
	c.session.refused.Add(1)
	c.session.logger.Info("go test receipt not recorded", "package", truncate(refusal.Package), "reason", truncate(refusal.Reason))
	return nil
}
func (c executionStore) Record(ctx context.Context, receipt Receipt) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	receipt.ExecutionID = c.session.execution.ID
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := c.session.store.Record(ctx, receipt); err != nil {
		c.session.failures.Add(1)
		return err
	}
	c.session.records.Add(1)
	return nil
}

func ValidationEnvironment(environment []string) []string {
	return slices.DeleteFunc(slices.Clone(environment), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return name == TokenEnvironment
	})
}
