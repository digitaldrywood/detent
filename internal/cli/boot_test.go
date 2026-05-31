package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
	projectpkg "github.com/digitaldrywood/symphony/internal/project"
	"github.com/digitaldrywood/symphony/internal/web"
)

func TestShouldLaunchTerminalDashboard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  BootConfig
		want bool
	}{
		{
			name: "running terminal",
			cfg:  BootConfig{Mode: BootModeRunning, StdoutTTY: true},
			want: true,
		},
		{
			name: "headless terminal",
			cfg:  BootConfig{Mode: BootModeRunning, Headless: true, StdoutTTY: true},
			want: false,
		},
		{
			name: "non terminal",
			cfg:  BootConfig{Mode: BootModeRunning},
			want: false,
		},
		{
			name: "onboarding terminal",
			cfg:  BootConfig{Mode: BootModeOnboarding, StdoutTTY: true},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := shouldLaunchTerminalDashboard(tt.cfg); got != tt.want {
				t.Fatalf("shouldLaunchTerminalDashboard(%#v) = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestRedirectDefaultLoggerWritesToFile(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	path := filepath.Join(t.TempDir(), "runtime", "symphony.log")
	restore, err := redirectDefaultLogger(path)
	if err != nil {
		t.Fatalf("redirectDefaultLogger() error = %v", err)
	}

	slog.Info("dashboard log message", "mode", "tui")
	restore()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	logs := string(raw)
	for _, want := range []string{`"msg":"dashboard log message"`, `"mode":"tui"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log file missing %q:\n%s", want, logs)
		}
	}
	if slog.Default() != previous {
		t.Fatal("default logger was not restored")
	}
}

func TestTerminalDashboardError(t *testing.T) {
	t.Parallel()

	serverErr := errors.New("server failed")
	tests := []struct {
		name   string
		first  error
		second error
		want   error
	}{
		{
			name:   "dashboard quit stops server cleanly",
			second: context.Canceled,
		},
		{
			name:  "server failure wins",
			first: serverErr,
			want:  serverErr,
		},
		{
			name:   "external cancel is preserved",
			first:  context.Canceled,
			second: context.Canceled,
			want:   context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := terminalDashboardError(tt.first, tt.second); !errors.Is(got, tt.want) {
				t.Fatalf("terminalDashboardError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegistryRefresherRequestsProjectOrchestrators(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, startRefreshProject(t, "alpha"))
	mustSetProject(t, registry, startRefreshProject(t, "beta"))

	refresher := refresherForRegistry(registry)
	if refresher == nil {
		t.Fatal("refresherForRegistry() = nil, want refresher")
	}

	response, err := refresher.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("RequestRefresh() error = %v", err)
	}
	assertRefresh(t, response)
}

func TestRegistryRefresherSkipsStoppedProjectOrchestrators(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, newRefreshProject(t, "stopped"))

	refresher := refresherForRegistry(registry)
	_, err := refresher.RequestRefresh(context.Background())
	if !errors.Is(err, projectpkg.ErrProjectNotFound) {
		t.Fatalf("RequestRefresh() error = %v, want %v", err, projectpkg.ErrProjectNotFound)
	}
}

func TestRegistryRefresherReturnsProjectNotFoundWithoutOrchestrators(t *testing.T) {
	t.Parallel()

	refresher := refresherForRegistry(projectpkg.NewRegistry())
	_, err := refresher.RequestRefresh(context.Background())
	if !errors.Is(err, projectpkg.ErrProjectNotFound) {
		t.Fatalf("RequestRefresh() error = %v, want %v", err, projectpkg.ErrProjectNotFound)
	}
}

func newRefreshProject(t *testing.T, id string) *projectpkg.Project {
	t.Helper()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	project, err := projectpkg.New(projectpkg.Config{
		Project: globalconfig.Project{
			ID:      id,
			Workdir: t.TempDir(),
			Weight:  1,
		},
		Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."},
	}, projectpkg.Dependencies{})
	if err != nil {
		t.Fatalf("project.New() error = %v", err)
	}
	return project
}

func startRefreshProject(t *testing.T, id string) *projectpkg.Project {
	t.Helper()

	project := newRefreshProject(t, id)
	if err := project.Start(context.Background()); err != nil {
		t.Fatalf("Project.Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := project.Stop(ctx); err != nil && !errors.Is(err, projectpkg.ErrNotRunning) {
			t.Fatalf("Project.Stop() error = %v", err)
		}
	})
	return project
}

func mustSetProject(t *testing.T, registry *projectpkg.Registry, project *projectpkg.Project) {
	t.Helper()

	if err := registry.Set(project); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
}

func assertRefresh(t *testing.T, response web.RefreshResponse) {
	t.Helper()

	if !response.Queued {
		t.Fatalf("Queued = false, want true; response = %#v", response)
	}
	if response.RequestedAt.IsZero() {
		t.Fatalf("RequestedAt is zero; response = %#v", response)
	}
	if len(response.Operations) != 2 || response.Operations[0] != "poll" || response.Operations[1] != "reconcile" {
		t.Fatalf("Operations = %#v, want poll/reconcile", response.Operations)
	}
}
