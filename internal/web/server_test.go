package web_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/store/sqlc"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
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
			wantContent: "Detent",
		},
		{
			name:        "settings",
			path:        "/settings",
			wantStatus:  http.StatusOK,
			wantContent: "Settings",
		},
		{
			name:        "reports",
			path:        "/reports",
			wantStatus:  http.StatusOK,
			wantContent: "Usage reports",
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

func TestServerStaticAssetsUseFingerprintsAndCacheHeaders(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	css := "body{color:purple}"
	writeTestCSS(t, staticDir, css)

	server, err := web.NewServer(web.Config{StaticDir: staticDir}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	onboardingServer, err := web.NewServer(web.Config{
		Mode:      web.ModeOnboarding,
		StaticDir: staticDir,
	}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() onboarding error = %v", err)
	}

	fingerprintedPath := "/static/css/output." + shortTestHash(css) + ".css"

	t.Run("html links fingerprinted stylesheet and revalidates", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			handler http.Handler
			path    string
		}{
			{name: "dashboard", handler: server.Handler(), path: "/"},
			{name: "settings", handler: server.Handler(), path: "/settings"},
			{name: "reports", handler: server.Handler(), path: "/reports"},
			{name: "onboarding", handler: onboardingServer.Handler(), path: "/onboarding"},
		}

		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, tt.path, nil)

				tt.handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
				}
				if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
					t.Fatalf("Cache-Control = %q, want no-cache", got)
				}
				if !strings.Contains(rec.Body.String(), `href="`+fingerprintedPath+`"`) {
					t.Fatalf("body missing fingerprinted stylesheet %q:\n%s", fingerprintedPath, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), `href="/static/css/output.css"`) {
					t.Fatalf("body still links non-fingerprinted stylesheet:\n%s", rec.Body.String())
				}
			})
		}
	})

	t.Run("fingerprinted asset is immutable", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, fingerprintedPath, nil)

		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		if rec.Body.String() != css {
			t.Fatalf("body = %q, want %q", rec.Body.String(), css)
		}
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("Cache-Control = %q, want immutable static caching", got)
		}
		if got := rec.Header().Get("ETag"); got == "" {
			t.Fatal("ETag is empty")
		}
	})
}

func TestServerLegacyStaticAssetsRequireRevalidation(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	css := "body{color:green}"
	writeTestCSS(t, staticDir, css)

	server, err := web.NewServer(web.Config{StaticDir: staticDir}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/output.css", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != css {
		t.Fatalf("body = %q, want %q", rec.Body.String(), css)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("ETag"); got == "" {
		t.Fatal("ETag is empty")
	}
}

func TestServerServesDefaultStaticAssetsFromArbitraryWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore Chdir() error = %v", err)
		}
	}()

	server, err := web.NewServer(web.Config{Mode: web.ModeOnboarding}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/output.css", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css", got)
	}
	if !strings.Contains(rec.Body.String(), "tailwindcss") {
		t.Fatalf("body missing embedded CSS marker:\n%s", rec.Body.String())
	}
}

func writeTestCSS(t *testing.T, staticDir string, css string) {
	t.Helper()

	cssDir := filepath.Join(staticDir, "css")
	if err := os.MkdirAll(cssDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(cssDir, "output.css"), []byte(css), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func shortTestHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:12]
}

func TestOnboardingModeDoesNotRequireRuntimeDependencies(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		Mode:      web.ModeOnboarding,
		StaticDir: t.TempDir(),
	}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantContent  string
		wantLocation string
	}{
		{
			name:         "root redirects to onboarding",
			path:         "/",
			wantStatus:   http.StatusFound,
			wantLocation: "/onboarding",
		},
		{
			name:        "onboarding page",
			path:        "/onboarding",
			wantStatus:  http.StatusOK,
			wantContent: "Detent onboarding",
		},
		{
			name:        "health",
			path:        "/health",
			wantStatus:  http.StatusOK,
			wantContent: `"mode":"onboarding"`,
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
			if tt.wantLocation != "" && rec.Header().Get("Location") != tt.wantLocation {
				t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), tt.wantLocation)
			}
			if tt.wantContent != "" && !strings.Contains(rec.Body.String(), tt.wantContent) {
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
					Identifier: "digitaldrywood/detent#35",
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
		"digitaldrywood/detent#35",
		"Dashboard templates",
		"42,000",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestDashboardRendersServerMetadata(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{
		StaticDir: t.TempDir(),
		Version:   "v9.8.7",
	}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "dashboard.example.test:4100"

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"v9.8.7",
		`href="http://localhost:4000"`,
		`href="/reports"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), "http://dashboard.example.test:4100") {
		t.Fatalf("dashboard link used request host:\n%s", rec.Body.String())
	}
}

func TestDashboardWiresHTMXSSE(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{DashboardURL: "http://localhost:4101"}, testDeps(t))
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
		`href="http://localhost:4101"`,
		`src="https://unpkg.com/htmx.org@2.0.4"`,
		`src="https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4"`,
		`src="https://cdn.jsdelivr.net/npm/idiomorph@0.7.3/dist/idiomorph-ext.min.js"`,
		`hx-ext="sse morph"`,
		`sse-connect="/events"`,
		`sse-swap="snapshot"`,
		`sse-swap="tick"`,
		`hx-swap="morph:innerHTML"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestSettingsRendersConfigProjectsAndRuntimePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "global.yaml")
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	workdir := filepath.Join(root, "repo")
	worktreeRoot := filepath.Join(root, "worktrees")
	dbPath := filepath.Join(root, "detent.db")
	logPath := filepath.Join(root, "detent.log")
	projectURL := "https://github.com/orgs/digitaldrywood/projects/4"

	registry := project.NewRegistry()
	trackedProject := newSettingsTestProject(t, globalconfig.Project{
		ID:       "detent",
		Workflow: workflowPath,
		Workdir:  workdir,
		Weight:   3,
		Priority: 2,
		Paused:   true,
	}, worktreeRoot, projectURL)
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	deps := testDeps(t)
	deps.Registry = registry
	server, err := web.NewServer(web.Config{
		StaticDir:      t.TempDir(),
		Version:        "v1.2.3",
		GlobalConfig:   globalconfig.Config{Path: configPath},
		ConfigPathRule: globalconfig.PathRuleFlag,
		RuntimeDBPath:  dbPath,
		RuntimeLogPath: logPath,
		ServerAddress:  "127.0.0.1:4101",
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"Settings",
		`href="/"`,
		`href="/reports"`,
		`href="/settings"`,
		`aria-current="page"`,
		"v1.2.3",
		"Resolved global config path",
		configPath,
		string(globalconfig.PathRuleFlag),
		"detent",
		workflowPath,
		workdir,
		worktreeRoot,
		"weight 3",
		"priority 2",
		"paused true",
		"github",
		projectURL,
		dbPath,
		logPath,
		"127.0.0.1:4101",
		"navigator.clipboard.writeText",
		"Copied!",
		`data-copy="` + configPath + `"`,
		`data-copy="` + workflowPath + `"`,
		`data-copy="` + workdir + `"`,
		`data-copy="` + worktreeRoot + `"`,
		`data-copy="` + projectURL + `"`,
		`data-copy="` + dbPath + `"`,
		`data-copy="` + logPath + `"`,
		`data-copy="127.0.0.1:4101"`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestServerEventsReplaysLatestSnapshot(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{Counts: telemetry.Counts{Running: 2, Queue: 3, Blocked: 1, Completed: 5}}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()

	event := readSSEEvent(t, body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{"Running", "2", "Queue", "3", "Blocked", "1", "Completed", "5"} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("snapshot event missing %q:\n%s", want, event.data)
		}
	}
}

func TestServerEventsStreamsPublishedSnapshots(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()

	if err := deps.Hub.Publish(telemetry.Snapshot{Counts: telemetry.Counts{Running: 4}}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	event := readSSEEvent(t, body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	if !strings.Contains(event.data, "4") {
		t.Fatalf("snapshot event missing running count:\n%s", event.data)
	}
}

func TestServerEventsStreamsLiveDashboardSections(t *testing.T) {
	t.Parallel()

	perDay := 25.0
	perIssue := 5.0
	deps := testDeps(t)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()
	generatedAt := time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC)

	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Counts: telemetry.Counts{
			Running:   5,
			Queue:     4,
			Blocked:   3,
			Completed: 2,
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-live",
					Identifier: "DD-LIVE",
					Title:      "Live dashboard row",
					State:      "In Progress",
				},
				SessionID:   "thread-live",
				TurnCount:   6,
				StartedAt:   generatedAt.Add(-6 * time.Minute),
				DiffAdded:   4,
				DiffRemoved: 2,
				DiffFiles:   3,
				DiffStatus:  "ok",
				Tokens: telemetry.Tokens{
					Input:  100,
					Output: 221,
					Total:  321,
				},
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-done-1",
					Identifier: "DD-DONE-1",
				},
				StartedAt:   generatedAt.Add(-2 * time.Minute),
				CompletedAt: generatedAt.Add(-45 * time.Second),
			},
			{
				Issue: telemetry.Issue{
					ID:         "issue-done-2",
					Identifier: "DD-DONE-2",
				},
				StartedAt:   generatedAt.Add(-5 * time.Minute),
				CompletedAt: generatedAt.Add(-3 * time.Minute),
			},
		},
		Budget: telemetry.Budget{
			Enabled:          true,
			PerDayMaxUSD:     &perDay,
			PerIssueMaxUSD:   &perIssue,
			CurrentSpendUSD:  12.34,
			ProjectedCostUSD: 20,
		},
		RateLimits: &telemetry.RateLimits{
			LimitName: "Codex",
			Primary: &telemetry.RateLimitBucket{
				Remaining:      87,
				Used:           13,
				Limit:          100,
				ResetInSeconds: 3600,
			},
		},
		Tokens: telemetry.Tokens{
			Input:          100,
			Output:         221,
			Total:          321,
			RuntimeSeconds: 60,
		},
		Throughput: telemetry.TokenThroughput{
			TokensPerSecond: 2.85,
			WindowSeconds:   60,
			Tokens:          171,
		},
		TokenTrend: []telemetry.TokenTrendPoint{
			{
				At:     time.Date(2026, 5, 31, 15, 0, 0, 0, time.UTC),
				Input:  50,
				Output: 100,
				Total:  150,
			},
			{
				At:     time.Date(2026, 5, 31, 15, 1, 0, 0, time.UTC),
				Input:  100,
				Output: 221,
				Total:  321,
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	event := readSSEEvent(t, body)
	if event.name != "snapshot" {
		t.Fatalf("event name = %q, want snapshot", event.name)
	}
	for _, want := range []string{
		"Running",
		"5",
		"Queue",
		"4",
		"Blocked",
		"3",
		"Completed",
		"2",
		"$12.34",
		"$19.74",
		"Cost burn-down",
		"DD-LIVE",
		"Live dashboard row",
		"+4 -2 (3 files)",
		"321",
		"Codex rate limits",
		"Primary",
		"87",
		"13",
		"100",
		"Token trend",
		"Input 15:01: 100 tokens",
		"Output 15:01: 221 tokens",
		"Token throughput",
		"2.9 tps",
		"Last 1m token throughput",
		"Runtime",
		"1m 0s",
		"Agent activity",
		"Live now",
		"DD-LIVE",
		"DD-DONE-1",
	} {
		if !strings.Contains(event.data, want) {
			t.Fatalf("snapshot event missing %q:\n%s", want, event.data)
		}
	}
}

func TestServerEventsSendsTickEvents(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{SSETickInterval: time.Millisecond}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := openEventStream(t, server)
	defer body.Close()

	event := readSSEEvent(t, body)
	if event.name != "tick" {
		t.Fatalf("event name = %q, want tick", event.name)
	}
	if strings.TrimSpace(event.data) == "" {
		t.Fatal("tick event data is empty")
	}
}

func TestServerAPIRoutes(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 5, 31, 3, 25, 0, 0, time.UTC)
	startedAt := generatedAt.Add(-5 * time.Minute)
	blockedAt := generatedAt.Add(-2 * time.Minute)
	lastEventAt := generatedAt.Add(-time.Minute)
	dueAt := generatedAt.Add(time.Minute)
	completedAt := generatedAt.Add(-30 * time.Second)
	perDay := 50.0

	events := hub.New[telemetry.Snapshot]()
	if err := events.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:          "issue-running",
					Identifier:  "digitaldrywood/detent#37",
					URL:         "https://github.com/digitaldrywood/detent/issues/37",
					Title:       "REST API",
					Description: strings.Repeat("api ", 90),
					State:       "In Progress",
				},
				WorkerHost:    "host-a",
				WorkspacePath: "/workspaces/DD-37",
				SessionID:     "thread-running",
				TurnCount:     3,
				StartedAt:     startedAt,
				LastEventAt:   &lastEventAt,
				LastEvent:     "notification",
				LastMessage:   "rendered",
				RecentEvents: []telemetry.ActivityEvent{
					{At: lastEventAt.Add(-time.Second), Event: "turn_started", Message: "turn started"},
					{At: lastEventAt, Event: "notification", Message: "rendered"},
				},
				RuntimeSeconds: 300,
				DiffAdded:      4,
				DiffRemoved:    2,
				DiffFiles:      3,
				DiffStatus:     "ok",
				Tokens: telemetry.Tokens{
					Input:  10,
					Output: 20,
					Total:  30,
				},
			},
		},
		Queue: []telemetry.Queued{
			{
				Issue: telemetry.Issue{
					ID:         "issue-retry",
					Identifier: "DD-RETRY",
					URL:        "https://github.com/digitaldrywood/detent/issues/38",
					Title:      "Retry API",
					State:      "Todo",
				},
				Attempt:       2,
				DueAt:         &dueAt,
				Error:         "no available orchestrator slots",
				WorkspacePath: "/workspaces/DD-RETRY",
			},
		},
		Blocked: []telemetry.Blocked{
			{
				Issue: telemetry.Issue{
					ID:         "issue-blocked",
					Identifier: "DD-BLOCKED",
					URL:        "https://github.com/digitaldrywood/detent/issues/39",
					Title:      "Blocked API",
					State:      "Todo",
				},
				WorkerHost:    "host-b",
				WorkspacePath: "/workspaces/DD-BLOCKED",
				SessionID:     "thread-blocked",
				Error:         "dependency is not merged",
				BlockedAt:     &blockedAt,
				LastEventAt:   &lastEventAt,
				LastEvent:     "turn_input_required",
				LastMessage:   "waiting for operator input",
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "issue-completed",
					Identifier: "DD-DONE",
					URL:        "https://github.com/digitaldrywood/detent/issues/40",
				},
				StartedAt:      startedAt,
				CompletedAt:    completedAt,
				Turns:          2,
				RuntimeSeconds: 45,
				FinalState:     "Done",
				Model:          "gpt-5",
				Tokens: telemetry.Tokens{
					Input:  100,
					Output: 200,
					Total:  300,
				},
			},
		},
		RateLimits: &telemetry.RateLimits{
			Primary: &telemetry.RateLimitBucket{Remaining: 11},
		},
		Tokens: telemetry.Tokens{
			Input:          11,
			Output:         22,
			Total:          33,
			RuntimeSeconds: 44.5,
		},
		Throughput: telemetry.TokenThroughput{
			TokensPerSecond: 7.5,
			WindowSeconds:   60,
			Tokens:          450,
		},
		LifetimeTotals: telemetry.LifetimeTotals{
			Available:      true,
			InputTokens:    1000,
			OutputTokens:   250,
			TotalTokens:    1250,
			RuntimeSeconds: 600,
			Sessions:       5,
			Runs:           2,
		},
		Budget: telemetry.Budget{
			Enabled:         true,
			CurrentSpendUSD: 1.25,
			PerDayMaxUSD:    &perDay,
			Days: []telemetry.BudgetDay{
				{Date: "2026-05-31", SpendUSD: 1.25},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	refresher := &refreshProbe{
		response: web.RefreshResponse{
			Queued:      true,
			Coalesced:   false,
			RequestedAt: generatedAt,
			Operations:  []string{"poll", "reconcile"},
		},
	}

	deps := testDeps(t)
	deps.Hub = events
	deps.Refresher = refresher

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	if state["generated_at"] != generatedAt.Format(time.RFC3339) {
		t.Fatalf("generated_at = %q, want %q", state["generated_at"], generatedAt.Format(time.RFC3339))
	}
	if got := nestedString(t, state, "counts", "running"); got != "1" {
		t.Fatalf("counts.running = %s, want 1", got)
	}
	if got := nestedString(t, state, "counts", "retrying"); got != "1" {
		t.Fatalf("counts.retrying = %s, want 1", got)
	}
	if got := nestedString(t, state, "counts", "blocked"); got != "1" {
		t.Fatalf("counts.blocked = %s, want 1", got)
	}
	if got := boardStateCount(t, state, "Todo"); got != "2" {
		t.Fatalf("board Todo count = %s, want 2", got)
	}
	if got := boardStateCount(t, state, "In Progress"); got != "1" {
		t.Fatalf("board In Progress count = %s, want 1", got)
	}
	if got := boardStateCount(t, state, "Done"); got != "1" {
		t.Fatalf("board Done count = %s, want 1", got)
	}
	flow := state["board"].(map[string]any)["flow"].([]any)
	if len(flow) != 1 || flow[0].(map[string]any)["label"] != "03:24" || flow[0].(map[string]any)["count"] != float64(1) {
		t.Fatalf("board flow = %#v", flow)
	}

	running := state["running"].([]any)[0].(map[string]any)
	if running["issue_identifier"] != "digitaldrywood/detent#37" || running["issue_title"] != "REST API" {
		t.Fatalf("running row = %#v", running)
	}
	description := running["issue_description"].(string)
	if len(description) != 250 || !strings.HasSuffix(description, "...") {
		t.Fatalf("issue_description length = %d, suffix ok = %v", len(description), strings.HasSuffix(description, "..."))
	}
	if running["budget_alert?"] != false {
		t.Fatalf("budget_alert? = %#v, want false", running["budget_alert?"])
	}
	for key, want := range map[string]any{
		"diff_added":   float64(4),
		"diff_removed": float64(2),
		"diff_files":   float64(3),
		"diff_status":  "ok",
	} {
		if running[key] != want {
			t.Fatalf("running[%q] = %#v, want %#v; row = %#v", key, running[key], want, running)
		}
	}
	if running["turn_count"] != float64(3) {
		t.Fatalf("running.turn_count = %#v, want 3", running["turn_count"])
	}
	runningTokens := running["tokens"].(map[string]any)
	if runningTokens["input_tokens"] != float64(10) || runningTokens["output_tokens"] != float64(20) || runningTokens["total_tokens"] != float64(30) {
		t.Fatalf("running.tokens = %#v, want live token counts", runningTokens)
	}
	runningEvents := running["recent_events"].([]any)
	if len(runningEvents) != 2 || runningEvents[1].(map[string]any)["message"] != "rendered" {
		t.Fatalf("running.recent_events = %#v", runningEvents)
	}

	retrying := state["retrying"].([]any)[0].(map[string]any)
	if retrying["issue_identifier"] != "DD-RETRY" || retrying["attempt"] != float64(2) {
		t.Fatalf("retrying row = %#v", retrying)
	}

	if got := nestedString(t, state, "codex_totals", "seconds_running"); got != "44.5" {
		t.Fatalf("codex_totals.seconds_running = %s, want 44.5", got)
	}
	if got := nestedString(t, state, "throughput", "tokens_per_second"); got != "7.5" {
		t.Fatalf("throughput.tokens_per_second = %s, want 7.5", got)
	}
	if got := nestedString(t, state, "throughput", "tokens"); got != "450" {
		t.Fatalf("throughput.tokens = %s, want 450", got)
	}
	if got := nestedString(t, state, "lifetime_totals", "total_tokens"); got != "1250" {
		t.Fatalf("lifetime_totals.total_tokens = %s, want 1250", got)
	}
	if got := nestedString(t, state, "lifetime_totals", "sessions"); got != "5" {
		t.Fatalf("lifetime_totals.sessions = %s, want 5", got)
	}
	if len(state["recent_sessions"].([]any)) != 1 {
		t.Fatalf("recent_sessions = %#v, want one entry", state["recent_sessions"])
	}
	if got := nestedString(t, state, "budget", "today_spend_usd"); got != "1.25" {
		t.Fatalf("budget.today_spend_usd = %s, want 1.25", got)
	}
	days := state["budget"].(map[string]any)["days"].([]any)
	if len(days) != 1 || days[0].(map[string]any)["date"] != "2026-05-31" || days[0].(map[string]any)["spend_usd"] != float64(1.25) {
		t.Fatalf("budget.days = %#v", days)
	}

	issue := requestJSON(t, server, http.MethodGet, "/api/v1/digitaldrywood/detent%2337", http.StatusOK)
	if issue["status"] != "running" || issue["issue_id"] != "issue-running" {
		t.Fatalf("issue payload = %#v", issue)
	}
	if issue["retry"] != nil || issue["blocked"] != nil || issue["last_error"] != nil {
		t.Fatalf("running issue nullable fields = %#v", issue)
	}
	runningIssue := issue["running"].(map[string]any)
	for key, want := range map[string]any{
		"diff_added":   float64(4),
		"diff_removed": float64(2),
		"diff_files":   float64(3),
		"diff_status":  "ok",
	} {
		if runningIssue[key] != want {
			t.Fatalf("issue.running[%q] = %#v, want %#v; running = %#v", key, runningIssue[key], want, runningIssue)
		}
	}
	if runningIssue["turn_count"] != float64(3) {
		t.Fatalf("issue.running.turn_count = %#v, want 3", runningIssue["turn_count"])
	}
	issueTokens := runningIssue["tokens"].(map[string]any)
	if issueTokens["input_tokens"] != float64(10) || issueTokens["output_tokens"] != float64(20) || issueTokens["total_tokens"] != float64(30) {
		t.Fatalf("issue.running.tokens = %#v, want live token counts", issueTokens)
	}
	issueEvents := issue["recent_events"].([]any)
	if len(issueEvents) != 2 || issueEvents[1].(map[string]any)["event"] != "notification" {
		t.Fatalf("issue.recent_events = %#v", issueEvents)
	}

	retryIssue := requestJSON(t, server, http.MethodGet, "/api/v1/DD-RETRY", http.StatusOK)
	if retryIssue["status"] != "retrying" || retryIssue["last_error"] != "no available orchestrator slots" {
		t.Fatalf("retry issue payload = %#v", retryIssue)
	}

	blockedIssue := requestJSON(t, server, http.MethodGet, "/api/v1/DD-BLOCKED", http.StatusOK)
	if blockedIssue["status"] != "blocked" || blockedIssue["last_error"] != "dependency is not merged" {
		t.Fatalf("blocked issue payload = %#v", blockedIssue)
	}

	missing := requestJSON(t, server, http.MethodGet, "/api/v1/DD-MISSING", http.StatusNotFound)
	if nestedString(t, missing, "error", "code") != "issue_not_found" {
		t.Fatalf("missing issue response = %#v", missing)
	}

	refresh := requestJSON(t, server, http.MethodPost, "/api/v1/refresh", http.StatusAccepted)
	if refresher.calls != 1 || refresh["queued"] != true || refresh["coalesced"] != false {
		t.Fatalf("refresh calls = %d, payload = %#v", refresher.calls, refresh)
	}
	if operations := refresh["operations"].([]any); len(operations) != 2 || operations[0] != "poll" || operations[1] != "reconcile" {
		t.Fatalf("refresh operations = %#v", refresh["operations"])
	}
}

func TestServerEnrichesBudgetBurnDownFromStoreAndRegistry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	generatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	backend := openWebTestStore(t)
	events := []store.UsageEvent{
		{
			ProjectID:  "detent",
			CostUSD:    1.25,
			StartedAt:  time.Date(2026, 5, 31, 8, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 5, 31, 8, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
		{
			ProjectID:  "detent",
			CostUSD:    1,
			StartedAt:  time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 6, 1, 6, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
		{
			ProjectID:  "pyroapex",
			CostUSD:    9,
			StartedAt:  time.Date(2026, 6, 1, 7, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 6, 1, 7, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
		{
			ProjectID:  "detent",
			CostUSD:    2.5,
			StartedAt:  time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 6, 1, 11, 1, 0, 0, time.UTC),
			Outcome:    "completed",
		},
	}
	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}

	registry := project.NewRegistry()
	if err := registry.Set(newBudgetTestProject(t, "detent", 100, 10)); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{GeneratedAt: generatedAt}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Store = backend
	deps.Registry = registry
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	budget := state["budget"].(map[string]any)
	if budget["enabled"] != true || budget["today_spend_usd"] != float64(3.5) || budget["projected_spend_usd"] != float64(7) {
		t.Fatalf("budget = %#v, want enabled 3.5 today and 7 projected", budget)
	}
	if budget["per_day_max_usd"] != float64(100) || budget["per_issue_max_usd"] != float64(10) {
		t.Fatalf("budget caps = %#v", budget)
	}
	points := budget["spend_points"].([]any)
	if len(points) != 2 || points[1].(map[string]any)["spend_usd"] != float64(3.5) {
		t.Fatalf("budget spend_points = %#v", points)
	}
	days := budget["days"].([]any)
	if len(days) != 2 || days[0].(map[string]any)["date"] != "2026-05-31" || days[1].(map[string]any)["spend_usd"] != float64(3.5) {
		t.Fatalf("budget days = %#v", days)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"Cost burn-down",
		"$3.50",
		"$100.00",
		"$7.00",
		"Projected period end: $7.00",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestServerPreservesSnapshotBudgetWhenSpendQueryFails(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	capUSD := 44.0
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Budget: telemetry.Budget{
			Enabled:           true,
			CurrentSpendUSD:   12.34,
			ProjectedSpendUSD: 56.78,
			PerDayMaxUSD:      &capUSD,
			Days: []telemetry.BudgetDay{
				{Date: "2026-06-01", SpendUSD: 12.34},
			},
			SpendPoints: []telemetry.BudgetSpendPoint{
				{At: generatedAt, SpendUSD: 12.34},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	registry := project.NewRegistry()
	if err := registry.Set(newBudgetTestProject(t, "detent", 100, 10)); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Registry = registry
	deps.Store = storeProbe{
		budgetCostEvents: func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
			return nil, errors.New("store is busy")
		},
	}
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	budget := state["budget"].(map[string]any)
	if budget["today_spend_usd"] != float64(12.34) || budget["projected_spend_usd"] != float64(56.78) {
		t.Fatalf("budget = %#v, want preserved snapshot spend", budget)
	}
	if budget["per_day_max_usd"] != float64(44) {
		t.Fatalf("budget per_day_max_usd = %#v, want preserved snapshot cap", budget["per_day_max_usd"])
	}
	days := budget["days"].([]any)
	if len(days) != 1 || days[0].(map[string]any)["spend_usd"] != float64(12.34) {
		t.Fatalf("budget days = %#v, want preserved snapshot days", days)
	}
	points := budget["spend_points"].([]any)
	if len(points) != 1 || points[0].(map[string]any)["spend_usd"] != float64(12.34) {
		t.Fatalf("budget spend_points = %#v, want preserved snapshot points", points)
	}
}

func TestServerAPIPreservesUnknownDiffStatus(t *testing.T) {
	t.Parallel()

	events := hub.New[telemetry.Snapshot]()
	if err := events.Publish(telemetry.Snapshot{
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "issue-running",
					Identifier: "DD-RUNNING",
					State:      "In Progress",
				},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = events

	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	state := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	running := state["running"].([]any)[0].(map[string]any)
	if got, ok := running["diff_status"].(string); !ok || got != "" {
		t.Fatalf("state running diff_status = %#v, want empty string", running["diff_status"])
	}

	issue := requestJSON(t, server, http.MethodGet, "/api/v1/DD-RUNNING", http.StatusOK)
	runningIssue := issue["running"].(map[string]any)
	if got, ok := runningIssue["diff_status"].(string); !ok || got != "" {
		t.Fatalf("issue running diff_status = %#v, want empty string", runningIssue["diff_status"])
	}
}

func TestServerAPIErrorRoutes(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "state method not allowed",
			method:     http.MethodPost,
			path:       "/api/v1/state",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "refresh method not allowed",
			method:     http.MethodGet,
			path:       "/api/v1/refresh",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "unknown route",
			method:     http.MethodGet,
			path:       "/unknown",
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "state unavailable",
			method:     http.MethodGet,
			path:       "/api/v1/state",
			wantStatus: http.StatusOK,
			wantCode:   "snapshot_unavailable",
		},
		{
			name:       "refresh unavailable",
			method:     http.MethodPost,
			path:       "/api/v1/refresh",
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "orchestrator_unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := requestJSON(t, server, tt.method, tt.path, tt.wantStatus)
			if got := nestedString(t, payload, "error", "code"); got != tt.wantCode {
				t.Fatalf("error.code = %q, want %q; payload = %#v", got, tt.wantCode, payload)
			}
		})
	}
}

func TestServerUsageAPIReportsAggregates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	usageStore, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := usageStore.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	seedUsageAPIEvents(t, ctx, usageStore)

	deps := testDeps(t)
	deps.Store = usageStore
	server, err := web.NewServer(web.Config{
		Pricing: budget.PricingTable{
			"gpt-report": {
				USDPerInputToken:  0.01,
				USDPerOutputToken: 0.02,
			},
			"gpt-report-mini": {
				USDPerInputToken:  0.001,
				USDPerOutputToken: 0.002,
			},
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	day := requestJSON(t, server, http.MethodGet, "/api/v1/usage?by=day&from=2026-05-31&to=2026-05-31", http.StatusOK)
	if day["by"] != "day" {
		t.Fatalf("by = %#v, want day", day["by"])
	}
	if got := nestedString(t, day, "totals", "total_tokens"); got != "225" {
		t.Fatalf("totals.total_tokens = %s, want 225", got)
	}
	if got := nestedString(t, day, "totals", "spend_usd"); got != "2.1" {
		t.Fatalf("totals.spend_usd = %s, want 2.1", got)
	}
	series := day["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1: %#v", len(series), series)
	}
	point := series[0].(map[string]any)
	if point["bucket"] != "2026-05-31" || point["date"] != "2026-05-31" || point["events"] != float64(2) {
		t.Fatalf("day point = %#v", point)
	}

	project := requestJSON(t, server, http.MethodGet, "/api/v1/usage?by=project&from=2026-05-31&to=2026-06-01", http.StatusOK)
	breakdowns := project["breakdowns"].([]any)
	if len(breakdowns) != 2 {
		t.Fatalf("breakdowns len = %d, want 2: %#v", len(breakdowns), breakdowns)
	}
	detent := usageBucket(t, breakdowns, "detent")
	if detent["total_tokens"] != float64(225) || detent["spend_usd"] != 2.1 {
		t.Fatalf("detent breakdown = %#v", detent)
	}

	tests := []struct {
		name       string
		path       string
		wantBucket string
	}{
		{name: "issue", path: "/api/v1/usage?by=issue", wantBucket: "digitaldrywood/detent#119"},
		{name: "pr", path: "/api/v1/usage?by=pr", wantBucket: "detent#141"},
		{name: "model", path: "/api/v1/usage?by=model", wantBucket: "gpt-report"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := requestJSON(t, server, http.MethodGet, tt.path, http.StatusOK)
			rows := payload["breakdowns"].([]any)
			if usageBucket(t, rows, tt.wantBucket) == nil {
				t.Fatalf("missing bucket %q in %#v", tt.wantBucket, rows)
			}
		})
	}
}

func TestServerUsageAPIRejectsInvalidParameters(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name     string
		path     string
		wantCode string
	}{
		{name: "invalid group", path: "/api/v1/usage?by=week", wantCode: "invalid_usage_group"},
		{name: "invalid from", path: "/api/v1/usage?by=day&from=2026-31-05", wantCode: "invalid_date"},
		{name: "invalid range", path: "/api/v1/usage?by=day&from=2026-06-02&to=2026-06-01", wantCode: "invalid_date_range"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := requestJSON(t, server, http.MethodGet, tt.path, http.StatusBadRequest)
			if got := nestedString(t, payload, "error", "code"); got != tt.wantCode {
				t.Fatalf("error.code = %q, want %q; payload = %#v", got, tt.wantCode, payload)
			}
		})
	}
}

func TestReportsPageRendersUsageCharts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	usageStore := openWebTestStore(t)
	seedUsageAPIEvents(t, ctx, usageStore)

	deps := testDeps(t)
	deps.Store = usageStore
	server, err := web.NewServer(web.Config{
		Pricing: budget.PricingTable{
			"gpt-report": {
				USDPerInputToken:  0.01,
				USDPerOutputToken: 0.02,
			},
			"gpt-report-mini": {
				USDPerInputToken:  0.001,
				USDPerOutputToken: 0.002,
			},
		},
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/reports", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"Usage reports",
		`href="/"`,
		`href="/reports"`,
		`href="/settings"`,
		"Spend trend",
		"Token trend",
		"Top issues by tokens",
		"Top PRs by tokens",
		"Per-project breakdown",
		"Model split",
		"$3.40",
		"325",
		"digitaldrywood/detent#119",
		"detent#141",
		"pyroapex",
		"gpt-report",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("reports page missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func testDeps(t *testing.T) web.Dependencies {
	t.Helper()

	return web.Dependencies{
		Hub:       hub.New[telemetry.Snapshot](),
		Store:     storeProbe{},
		Registry:  project.NewRegistry(),
		Connector: connectorProbe{name: "memory"},
	}
}

func newSettingsTestProject(t *testing.T, cfg globalconfig.Project, worktreeRoot string, projectURL string) *project.Project {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerGitHub
	workflowCfg.Tracker.Endpoint = "https://api.github.com/graphql"
	workflowCfg.Tracker.APIKey = "$GITHUB_TOKEN"
	workflowCfg.Tracker.ProjectSlug = projectURL
	workflowCfg.Workspace.Root = worktreeRoot
	workflowCfg.Workspace.SourceRoot = cfg.Workdir

	trackedProject, err := project.New(project.Config{
		Project: cfg,
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: connectorProbe{name: "github"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return trackedProject
}

func newBudgetTestProject(t *testing.T, id string, perDayMaxUSD float64, perIssueMaxUSD float64) *project.Project {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerGitHub
	workflowCfg.Tracker.Endpoint = "https://api.github.com/graphql"
	workflowCfg.Tracker.APIKey = "$GITHUB_TOKEN"
	workflowCfg.Tracker.ProjectSlug = "https://github.com/orgs/digitaldrywood/projects/4"
	workflowCfg.Budget.Enabled = true
	workflowCfg.Budget.PerDayMaxUSD = perDayMaxUSD
	workflowCfg.Budget.PerIssueMaxUSD = perIssueMaxUSD

	trackedProject, err := project.New(project.Config{
		Project: globalconfig.Project{ID: id},
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: connectorProbe{name: "github"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return trackedProject
}

func openWebTestStore(t *testing.T) store.Store {
	t.Helper()

	backend, err := store.Open(context.Background(), store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return backend
}

func seedUsageAPIEvents(t *testing.T, ctx context.Context, backend store.Store) {
	t.Helper()

	events := []store.UsageEvent{
		{
			ProjectID:      "detent",
			IssueID:        "issue-119",
			Identifier:     "digitaldrywood/detent#119",
			PRNumber:       int64Ptr(141),
			Model:          "gpt-report",
			InputTokens:    100,
			OutputTokens:   50,
			TotalTokens:    150,
			RuntimeSeconds: 30,
			StartedAt:      time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 5, 31, 9, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
		{
			ProjectID:      "detent",
			IssueID:        "issue-120",
			Identifier:     "digitaldrywood/detent#120",
			PRNumber:       int64Ptr(142),
			Model:          "gpt-report-mini",
			InputTokens:    50,
			OutputTokens:   25,
			TotalTokens:    75,
			RuntimeSeconds: 15,
			StartedAt:      time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 5, 31, 10, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
		{
			ProjectID:      "pyroapex",
			IssueID:        "issue-119",
			Identifier:     "digitaldrywood/detent#119",
			PRNumber:       int64Ptr(141),
			Model:          "gpt-report",
			InputTokens:    70,
			OutputTokens:   30,
			TotalTokens:    100,
			RuntimeSeconds: 25,
			StartedAt:      time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 11, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
	}

	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}
}

func usageBucket(t *testing.T, rows []any, bucket string) map[string]any {
	t.Helper()

	for _, row := range rows {
		object := row.(map[string]any)
		if object["bucket"] == bucket {
			return object
		}
	}
	t.Fatalf("missing bucket %q in %#v", bucket, rows)
	return nil
}

func int64Ptr(value int64) *int64 {
	return &value
}

func requestJSON(t *testing.T, server *web.Server, method string, path string, wantStatus int) map[string]any {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, rec.Code, wantStatus, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal(%s %s) error = %v; body = %s", method, path, err, rec.Body.String())
	}
	return payload
}

func nestedString(t *testing.T, payload map[string]any, keys ...string) string {
	t.Helper()

	var current any = payload
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("value for %v is %T, want object", keys, current)
		}
		current = object[key]
	}
	switch value := current.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		t.Fatalf("value for %v is %T, want string or number", keys, current)
		return ""
	}
}

func boardStateCount(t *testing.T, payload map[string]any, stateName string) string {
	t.Helper()

	board := payload["board"].(map[string]any)
	distribution := board["state_distribution"].([]any)
	for _, entry := range distribution {
		row := entry.(map[string]any)
		if row["state"] == stateName {
			return strconv.FormatFloat(row["count"].(float64), 'f', -1, 64)
		}
	}
	t.Fatalf("board state %q missing from %#v", stateName, distribution)
	return ""
}

type storeProbe struct {
	store.Store

	budgetCostEvents func(context.Context, store.BudgetCostQuery) ([]store.BudgetCostEvent, error)
}

func (storeProbe) LifetimeTotals(context.Context) (store.LifetimeTotals, error) {
	return store.LifetimeTotals{}, nil
}

func (storeProbe) UsageReport(_ context.Context, query store.UsageReportQuery) (store.UsageReport, error) {
	return store.UsageReport{By: query.By}, nil
}

func (p storeProbe) BudgetCostEvents(ctx context.Context, query store.BudgetCostQuery) ([]store.BudgetCostEvent, error) {
	if p.budgetCostEvents != nil {
		return p.budgetCostEvents(ctx, query)
	}
	return nil, nil
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

type sseEvent struct {
	name string
	data string
}

func openEventStream(t *testing.T, server *web.Server) io.ReadCloser {
	t.Helper()

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("ReadAll() error = %v", readErr)
		}
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, string(body))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	return resp.Body
}

func readSSEEvent(t *testing.T, r io.Reader) sseEvent {
	t.Helper()

	lines := make(chan string)
	errs := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		errs <- scanner.Err()
		close(lines)
	}()

	var event sseEvent
	deadline := time.After(time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				if err := <-errs; err != nil {
					t.Fatalf("reading SSE stream: %v", err)
				}
				t.Fatal("SSE stream closed before event")
			}
			if line == "" {
				if event.name == "" {
					t.Fatal("SSE event missing name")
				}
				return event
			}
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				event.name = name
				continue
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				if event.data != "" {
					event.data += "\n"
				}
				event.data += data
				continue
			}
			t.Fatalf("unexpected SSE line %q", line)
		case <-deadline:
			t.Fatal("timed out waiting for SSE event")
		}
	}
}

type refreshProbe struct {
	response web.RefreshResponse
	err      error
	calls    int
}

func (p *refreshProbe) RequestRefresh(context.Context) (web.RefreshResponse, error) {
	p.calls++
	if p.err != nil {
		return web.RefreshResponse{}, p.err
	}
	return p.response, nil
}
