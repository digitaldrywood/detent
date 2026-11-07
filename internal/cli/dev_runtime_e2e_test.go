package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type bannerObservingBoardSnapshotStore struct {
	output      *lockedBuffer
	observed    chan string
	releaseLoad chan struct{}
	releaseOnce sync.Once
}

func (s *bannerObservingBoardSnapshotStore) Load(context.Context) (telemetry.Snapshot, bool, error) {
	s.observed <- s.output.String()
	<-s.releaseLoad
	return telemetry.Snapshot{}, false, nil
}

func (*bannerObservingBoardSnapshotStore) Save(context.Context, telemetry.Snapshot) error {
	return nil
}

func (s *bannerObservingBoardSnapshotStore) release() {
	s.releaseOnce.Do(func() { close(s.releaseLoad) })
}

func TestDevRuntimeAuthConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		email        string
		smtp         string
		from         string
		issuer       string
		clientID     string
		clientSecret string
		wantMode     string
		wantErr      bool
	}{
		{name: "disabled"},
		{name: "magic link", email: "operator@example.com", smtp: "127.0.0.1:2525", from: "detent@example.com", wantMode: "magic_link"},
		{name: "oidc", email: "operator@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", clientSecret: "secret", wantMode: "oidc"},
		{name: "mixed providers", email: "operator@example.com", smtp: "127.0.0.1:2525", from: "detent@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", clientSecret: "secret", wantErr: true},
		{name: "incomplete oidc", email: "operator@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := devRuntimeAuthConfig(tt.email, tt.smtp, tt.from, tt.issuer, tt.clientID, tt.clientSecret)
			if (err != nil) != tt.wantErr {
				t.Fatalf("devRuntimeAuthConfig() error = %v, want error %t", err, tt.wantErr)
			}
			if cfg.Mode != tt.wantMode {
				t.Fatalf("devRuntimeAuthConfig().Mode = %q, want %q", cfg.Mode, tt.wantMode)
			}
		})
	}
}

type startupTranscriptSnapshot struct {
	store.Store
	snapshot func(context.Context)
}

func (s startupTranscriptSnapshot) ProtectedCodexThreadIDs(ctx context.Context) ([]string, error) {
	ids, err := s.Store.ProtectedCodexThreadIDs(ctx)
	if err == nil {
		s.snapshot(ctx)
	}
	return ids, err
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type blockingBoardSnapshotStore struct {
	started     chan struct{}
	releaseSave chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingBoardSnapshotStore() *blockingBoardSnapshotStore {
	return &blockingBoardSnapshotStore{
		started:     make(chan struct{}),
		releaseSave: make(chan struct{}),
	}
}

func (s *blockingBoardSnapshotStore) Load(context.Context) (telemetry.Snapshot, bool, error) {
	return telemetry.Snapshot{}, false, nil
}

func (s *blockingBoardSnapshotStore) Save(context.Context, telemetry.Snapshot) error {
	s.startOnce.Do(func() {
		close(s.started)
	})
	<-s.releaseSave
	return nil
}

func (s *blockingBoardSnapshotStore) release() {
	s.releaseOnce.Do(func() {
		close(s.releaseSave)
	})
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForIsolatedRuntimeURL(t *testing.T, output *lockedBuffer, done <-chan error) string {
	t.Helper()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before banner: %v", err)
		case <-deadline:
			t.Fatalf("timed out waiting for isolated runtime banner:\n%s", output.String())
		default:
		}

		for line := range strings.SplitSeq(output.String(), "\n") {
			url, ok := strings.CutPrefix(line, "Dashboard: ")
			if ok && strings.TrimSpace(url) != "" {
				return strings.TrimSpace(url)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForDashboardHeader(t *testing.T, rawURL string, done <-chan error, scenario string) string {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lastErr error
	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before dashboard response: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("X-Detent-Demo-Scenario", scenario)
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			lastErr = errors.New(resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out fetching %s with scenario %s: %v", rawURL, scenario, lastErr)
	return ""
}

func postRuntimeRefresh(t *testing.T, url string, done <-chan error) {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before refresh: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/v1/refresh", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusAccepted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting runtime refresh to %s", url)
}

func waitForDashboardConditionWithRefresh(
	t *testing.T,
	refreshURL string,
	conditionURL string,
	done <-chan error,
	name string,
	ok func(string) bool,
) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	lastBody := ""
	nextRefresh := time.Time{}
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(nextRefresh) {
			postRuntimeRefresh(t, refreshURL, done)
			nextRefresh = now.Add(time.Second)
		}
		body := waitForDashboardContext(t, ctx, conditionURL, done)
		lastBody = body
		if ok(body) {
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for dashboard condition %q at %s; last body:\n%s", name, conditionURL, lastBody)
	return ""
}

func postRuntimeKanbanForm(t *testing.T, rawURL string, done <-chan error, form url.Values) string {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before Kanban action: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			t.Fatalf("Kanban action status = %d, want 200; body = %s", resp.StatusCode, string(body))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting Kanban action to %s", rawURL)
	return ""
}

func boardStateCountFromBody(t *testing.T, body string, state string) int {
	t.Helper()

	var payload struct {
		Board struct {
			StateDistribution []struct {
				State string `json:"state"`
				Count int    `json:"count"`
			} `json:"state_distribution"`
		} `json:"board"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return 0
	}
	for _, entry := range payload.Board.StateDistribution {
		if entry.State == state {
			return entry.Count
		}
	}
	return 0
}

func newRuntimeHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{}, Timeout: time.Second}
}

type isolatedAuditObservation struct {
	request   orchestrator.SecurityAuditRequest
	execution orchestrator.SecurityAuditExecution
}

type observedDevRuntimeRunner struct {
	devRuntimeRunner
	audits chan isolatedAuditObservation
}

func (r observedDevRuntimeRunner) Audit(ctx context.Context, request orchestrator.SecurityAuditRequest) (orchestrator.SecurityAuditExecution, error) {
	execution, err := r.devRuntimeRunner.Audit(ctx, request)
	if err == nil {
		select {
		case r.audits <- isolatedAuditObservation{request: request, execution: execution}:
		default:
		}
	}
	return execution, err
}
