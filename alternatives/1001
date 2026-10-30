package gotestreceipt

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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
	lookupPath      = "/lookup"
	hitPath         = "/hit"
	recordPath      = "/record"
	refusedPath     = "/refused"
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
	dir      string
	socket   string
	server   *http.Server
	listener net.Listener
	store    Store
	logger   *slog.Logger
	token    string
	served   chan error
	handlers sync.WaitGroup
	once     sync.Once
	lookups  atomic.Int64
	hits     atomic.Int64
	records  atomic.Int64
	failures atomic.Int64
	refused  atomic.Int64
}

func Start(ctx context.Context, store Store, logger *slog.Logger) (*Session, error) {
	if store == nil {
		return nil, errors.New("go test receipt store is required")
	}
	if logger == nil {
		logger = slog.Default()
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
	session := &Session{dir: dir, socket: socket, listener: listener, store: store, logger: logger, token: rand.Text(), served: make(chan error, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+lookupPath, session.lookup)
	mux.HandleFunc("POST "+hitPath, session.hit)
	mux.HandleFunc("POST "+recordPath, session.record)
	mux.HandleFunc("POST "+refusedPath, session.refuse)
	session.server = &http.Server{Handler: session.authorize(mux), ReadHeaderTimeout: requestTimeout, ReadTimeout: requestTimeout}
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

func (s *Session) authorize(next http.Handler) http.Handler {
	expected := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handlers.Add(1)
		defer s.handlers.Done()
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Session) Environment() map[string]string {
	return map[string]string{SocketEnvironment: s.socket, TokenEnvironment: s.token}
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

func (s *Session) lookup(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if !ValidKey(key) {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	s.lookups.Add(1)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	candidates, err := s.store.Lookup(ctx, key)
	if err != nil {
		s.failures.Add(1)
		s.logger.Warn("go test receipt lookup failed", "error", err)
		http.Error(w, "lookup failed", http.StatusBadGateway)
		return
	}
	if len(candidates) > MaxCandidates {
		candidates = candidates[:MaxCandidates]
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(candidates); err != nil {
		s.logger.Warn("go test receipt lookup response failed", "error", err)
	}
}

func (s *Session) hit(w http.ResponseWriter, r *http.Request) {
	if !ValidKey(r.URL.Query().Get("key")) {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}
	s.hits.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Session) refuse(w http.ResponseWriter, r *http.Request) {
	var refusal Refusal
	if err := json.NewDecoder(io.LimitReader(r.Body, maxReasonBytes*2)).Decode(&refusal); err != nil {
		http.Error(w, "invalid refusal", http.StatusBadRequest)
		return
	}
	s.refused.Add(1)
	s.logger.Info("go test receipt not recorded", "package", truncate(refusal.Package), "reason", truncate(refusal.Reason))
	w.WriteHeader(http.StatusNoContent)
}

func truncate(value string) string {
	if len(value) > maxReasonBytes/4 {
		return strings.ToValidUTF8(value[:maxReasonBytes/4], "")
	}
	return value
}

func (s *Session) record(w http.ResponseWriter, r *http.Request) {
	var receipt Receipt
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRecordBytes)).Decode(&receipt); err != nil {
		http.Error(w, "invalid receipt", http.StatusBadRequest)
		return
	}
	if err := receipt.Validate(); err != nil {
		http.Error(w, "invalid receipt", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := s.store.Record(ctx, receipt); err != nil {
		s.failures.Add(1)
		s.logger.Warn("go test receipt record failed", "error", err)
		http.Error(w, "record failed", http.StatusBadGateway)
		return
	}
	s.records.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

type Client struct {
	http  *http.Client
	token string
}

func NewClient(socket, token string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: requestTimeout}, token: token}
}

func (c *Client) Lookup(ctx context.Context, key string) ([]Candidate, error) {
	var candidates []Candidate
	if err := c.do(ctx, http.MethodGet, lookupPath+"?key="+url.QueryEscape(key), nil, &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (c *Client) Hit(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodPost, hitPath+"?key="+url.QueryEscape(key), nil, nil)
}

type Refusal struct {
	Package string `json:"package"`
	Reason  string `json:"reason"`
}

func (c *Client) Refuse(ctx context.Context, refusal Refusal) error {
	body, err := json.Marshal(refusal)
	if err != nil {
		return fmt.Errorf("encode refusal: %w", err)
	}
	return c.do(ctx, http.MethodPost, refusedPath, body, nil)
}

func (c *Client) Record(ctx context.Context, receipt Receipt) error {
	body, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	return c.do(ctx, http.MethodPost, recordPath, body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, clientBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("receipt service returned %s", response.Status)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(output)
}
