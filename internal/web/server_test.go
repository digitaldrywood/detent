package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/hub"
	"github.com/digitaldrywood/symphony-go/internal/store"
	"github.com/digitaldrywood/symphony-go/internal/store/sqlc"
	"github.com/digitaldrywood/symphony-go/internal/telemetry"
	"github.com/digitaldrywood/symphony-go/internal/web"
)

func TestNewServerValidatesDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		deps web.Dependencies
		want error
	}{
		{
			name: "missing hub",
			deps: testDeps(t),
			want: web.ErrMissingHub,
		},
		{
			name: "missing store",
			deps: func() web.Dependencies {
				deps := testDeps(t)
				deps.Store = nil
				return deps
			}(),
			want: web.ErrMissingStore,
		},
		{
			name: "missing registry",
			deps: func() web.Dependencies {
				deps := testDeps(t)
				deps.Registry = nil
				return deps
			}(),
			want: web.ErrMissingRegistry,
		},
		{
			name: "missing connector",
			deps: func() web.Dependencies {
				deps := testDeps(t)
				deps.Connector = nil
				return deps
			}(),
			want: web.ErrMissingConnector,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.want == web.ErrMissingHub {
				tt.deps.Hub = nil
			}

			_, err := web.NewServer(web.Config{}, tt.deps)
			if !errors.Is(err, tt.want) {
				t.Fatalf("NewServer() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestServerRoutes(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	cssDir := filepath.Join(staticDir, "css")
	if err := os.MkdirAll(cssDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(cssDir, "output.css"), []byte("body{color:black}"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: staticDir}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantContent string
	}{
		{
			name:        "dashboard",
			path:        "/",
			wantStatus:  http.StatusOK,
			wantContent: "Symphony",
		},
		{
			name:        "health",
			path:        "/health",
			wantStatus:  http.StatusOK,
			wantContent: `"status":"ok"`,
		},
		{
			name:        "static css",
			path:        "/static/css/output.css",
			wantStatus:  http.StatusOK,
			wantContent: "body{color:black}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantContent) {
				t.Fatalf("body missing %q:\n%s", tt.wantContent, rec.Body.String())
			}
		})
	}
}

func TestDashboardRendersLatestSnapshot(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC),
		Counts: telemetry.Counts{
			Running:   1,
			Queue:     2,
			Blocked:   3,
			Completed: 4,
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-35",
					Identifier: "digitaldrywood/symphony-go#35",
					Title:      "Dashboard templates",
					State:      "In Progress",
				},
				TurnCount:      2,
				RuntimeSeconds: 120,
				Tokens: telemetry.Tokens{
					Total: 42_000,
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{StaticDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"digitaldrywood/symphony-go#35",
		"Dashboard templates",
		"42,000",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func testDeps(t *testing.T) web.Dependencies {
	t.Helper()

	return web.Dependencies{
		Hub:       hub.New[telemetry.Snapshot](),
		Store:     storeProbe{},
		Registry:  struct{}{},
		Connector: connectorProbe{name: "memory"},
	}
}

type storeProbe struct {
	store.Store
}

func (storeProbe) Queries() *sqlc.Queries {
	return nil
}

func (storeProbe) Close() error {
	return nil
}

type connectorProbe struct {
	name string
}

func (p connectorProbe) Name() string {
	return p.name
}

func (p connectorProbe) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p connectorProbe) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p connectorProbe) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (p connectorProbe) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (p connectorProbe) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}
