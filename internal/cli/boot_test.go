package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/web"
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

func TestBuildGlobalSchedulerFromSettings(t *testing.T) {
	t.Parallel()

	global, err := buildGlobalScheduler(globalconfig.Settings{
		MaxConcurrentAgents: 2,
		Scheduling:          globalconfig.SchedulingRoundRobin,
		FairShare:           map[string]any{"half_life": "30m"},
	}, nil)
	if err != nil {
		t.Fatalf("buildGlobalScheduler() error = %v", err)
	}
	if global.Mode() != scheduler.ModeRoundRobin {
		t.Fatalf("Mode() = %q, want %q", global.Mode(), scheduler.ModeRoundRobin)
	}

	first, err := global.RequestSlot(context.Background(), scheduler.SlotRequest{State: "Todo"})
	if err != nil {
		t.Fatalf("RequestSlot() first error = %v", err)
	}
	second, err := global.RequestSlot(context.Background(), scheduler.SlotRequest{State: "Todo"})
	if err != nil {
		t.Fatalf("RequestSlot() second error = %v", err)
	}
	if _, err := global.RequestSlot(context.Background(), scheduler.SlotRequest{State: "Todo"}); !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("RequestSlot() third error = %v, want ErrNoSlots", err)
	}
	if err := global.ReleaseSlot(first); err != nil {
		t.Fatalf("ReleaseSlot() first error = %v", err)
	}
	if err := global.ReleaseSlot(second); err != nil {
		t.Fatalf("ReleaseSlot() second error = %v", err)
	}
}

func TestRedirectDefaultLoggerWritesToFile(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	path := filepath.Join(t.TempDir(), "runtime", "detent.log")
	restore, err := redirectDefaultLogger(path, "info")
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

func TestResolveBootConfigUsesWorkflowRefForServerHost(t *testing.T) {
	t.Parallel()

	repo := initRuntimeWorkflowRepo(t)
	writeBootHostWorkflow(t, filepath.Join(repo, "WORKFLOW.md"), "127.0.0.8")
	commitRuntimeWorkflowRepo(t, repo, "initial workflow")
	updateRuntimeWorkflowRef(t, repo, "origin/main", "HEAD")
	writeBootHostWorkflow(t, filepath.Join(repo, "WORKFLOW.md"), "127.0.0.9")

	configPath := filepath.Join(t.TempDir(), "global.yaml")
	cfg, err := globalconfig.DefaultAt(configPath)
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}
	cfg.Projects = []globalconfig.Project{
		{
			ID:          "detent",
			Workflow:    "WORKFLOW.md",
			WorkflowRef: "origin/main",
			Workdir:     repo,
			Weight:      1,
			Priority:    0,
		},
	}
	if err := globalconfig.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := resolveBootConfig(context.Background(), configPath, "", runtimeFlags{}, defaultOptions())
	if err != nil {
		t.Fatalf("resolveBootConfig() error = %v", err)
	}

	if got.Host != "127.0.0.8" {
		t.Fatalf("Host = %q, want ref-backed host", got.Host)
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
		return
	}

	response, err := refresher.RequestRefresh(context.Background())
	if err != nil {
		t.Fatalf("RequestRefresh() error = %v", err)
	}
	assertRefresh(t, response)
}

func TestStartRunningBootsDashboardAndStopsOnContextCancel(t *testing.T) {
	host, port := freeLoopbackPort(t)
	globalPath := filepath.Join(t.TempDir(), "global.yaml")
	global, err := globalconfig.DefaultAt(globalPath)
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, BootConfig{
			Mode:   BootModeRunning,
			Global: global,
			Host:   host,
			Port:   &port,
		})
	}()

	body := waitForDashboard(t, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/", done)
	if !strings.Contains(body, "Detent") {
		t.Fatalf("dashboard body missing Detent:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for startRunning to stop")
	}
}

func TestStartRunningPublishesStartupSnapshotBeforeProjectStartCompletes(t *testing.T) {
	host, port := freeLoopbackPort(t)
	configPath := filepath.Join(t.TempDir(), "global.yaml")
	alpha := createBootProjectFiles(t)
	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "alpha", Workflow: alpha.workflowPath, Workdir: alpha.workdirPath, Weight: 1},
	})
	global, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	provisionStarted := make(chan struct{})
	provisionRelease := make(chan struct{})
	var provisionStartedOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, BootConfig{
			Mode:   BootModeRunning,
			Global: global,
			Host:   host,
			Port:   &port,
			ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
				return bootProvisioningConnector{
					provision: func(ctx context.Context) error {
						provisionStartedOnce.Do(func() {
							close(provisionStarted)
						})
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-provisionRelease:
							return nil
						}
					},
				}, nil
			},
		})
	}()

	select {
	case <-provisionStarted:
	case err := <-done:
		t.Fatalf("startRunning returned before provisioning blocked: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for project provisioning to start")
	}

	stateURL := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/api/v1/state"
	body := waitForDashboardCondition(t, stateURL, done, "startup snapshot", func(body string) bool {
		return strings.Contains(body, `"generated_at"`) &&
			strings.Contains(body, `"alpha"`) &&
			!strings.Contains(body, "snapshot_unavailable")
	})
	if !strings.Contains(body, `"running":0`) {
		t.Fatalf("startup snapshot body missing zero running count:\n%s", body)
	}

	close(provisionRelease)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for startRunning to stop")
	}
}

func TestStartRunningHotReloadsGlobalConfigProjects(t *testing.T) {
	host, port := freeLoopbackPort(t)
	configPath := filepath.Join(t.TempDir(), "global.yaml")
	alpha := createBootProjectFiles(t)
	bravo := createBootProjectFiles(t)
	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "alpha", Workflow: alpha.workflowPath, Workdir: alpha.workdirPath, Weight: 1},
	})
	global, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, BootConfig{
			Mode:   BootModeRunning,
			Global: global,
			Host:   host,
			Port:   &port,
		})
	}()

	settingsURL := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/settings"
	body := waitForDashboard(t, settingsURL, done)
	if !strings.Contains(body, "alpha") {
		t.Fatalf("settings body missing alpha:\n%s", body)
	}

	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "alpha", Workflow: alpha.workflowPath, Workdir: alpha.workdirPath, Weight: 1},
		{ID: "bravo", Workflow: bravo.workflowPath, Workdir: bravo.workdirPath, Weight: 1},
	})
	body = waitForDashboardCondition(t, settingsURL, done, "bravo added", func(body string) bool {
		return strings.Contains(body, "bravo")
	})
	if !strings.Contains(body, "alpha") {
		t.Fatalf("settings body missing alpha after reload:\n%s", body)
	}

	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "bravo", Workflow: bravo.workflowPath, Workdir: bravo.workdirPath, Weight: 1},
	})
	body = waitForDashboardCondition(t, settingsURL, done, "alpha removed", func(body string) bool {
		return strings.Contains(body, "bravo") && !strings.Contains(body, "alpha")
	})
	if strings.Contains(body, "alpha") {
		t.Fatalf("settings body still contains alpha after removal:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for startRunning to stop")
	}
}

func TestStartRunningReconcilesGlobalConfigChangedBeforeWatcherStarts(t *testing.T) {
	host, port := freeLoopbackPort(t)
	configPath := filepath.Join(t.TempDir(), "global.yaml")
	alpha := createBootProjectFiles(t)
	bravo := createBootProjectFiles(t)
	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "alpha", Workflow: alpha.workflowPath, Workdir: alpha.workdirPath, Weight: 1},
	})
	global, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	provisionStarted := make(chan struct{})
	provisionRelease := make(chan struct{})
	var provisionStartedOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, BootConfig{
			Mode:   BootModeRunning,
			Global: global,
			Host:   host,
			Port:   &port,
			ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
				return bootProvisioningConnector{
					provision: func(ctx context.Context) error {
						provisionStartedOnce.Do(func() {
							close(provisionStarted)
						})
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-provisionRelease:
							return nil
						}
					},
				}, nil
			},
		})
	}()

	select {
	case <-provisionStarted:
	case err := <-done:
		t.Fatalf("startRunning returned before provisioning blocked: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for project provisioning to start")
	}

	settingsURL := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/settings"
	body := waitForDashboard(t, settingsURL, done)
	if !strings.Contains(body, "alpha") {
		t.Fatalf("settings body missing alpha:\n%s", body)
	}

	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "alpha", Workflow: alpha.workflowPath, Workdir: alpha.workdirPath, Weight: 1},
		{ID: "bravo", Workflow: bravo.workflowPath, Workdir: bravo.workdirPath, Weight: 1},
	})
	close(provisionRelease)

	body = waitForDashboardCondition(t, settingsURL, done, "bravo added after startup write", func(body string) bool {
		return strings.Contains(body, "bravo")
	})
	if !strings.Contains(body, "alpha") {
		t.Fatalf("settings body missing alpha after startup write:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for startRunning to stop")
	}
}

func TestRegistryRefresherSkipsStoppedProjectOrchestrators(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, newRefreshProject(t, "stopped"))

	refresher := refresherForRegistry(registry)
	if refresher == nil {
		t.Fatal("refresherForRegistry() = nil, want refresher")
		return
	}
	_, err := refresher.RequestRefresh(context.Background())
	if !errors.Is(err, projectpkg.ErrProjectNotFound) {
		t.Fatalf("RequestRefresh() error = %v, want %v", err, projectpkg.ErrProjectNotFound)
	}
}

func TestRegistryRefresherReturnsProjectNotFoundWithoutOrchestrators(t *testing.T) {
	t.Parallel()

	refresher := refresherForRegistry(projectpkg.NewRegistry())
	if refresher == nil {
		t.Fatal("refresherForRegistry() = nil, want refresher")
		return
	}
	_, err := refresher.RequestRefresh(context.Background())
	if !errors.Is(err, projectpkg.ErrProjectNotFound) {
		t.Fatalf("RequestRefresh() error = %v, want %v", err, projectpkg.ErrProjectNotFound)
	}
}

func freeLoopbackPort(t *testing.T) (string, int) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr = %T, want *net.TCPAddr", listener.Addr())
	}
	return "127.0.0.1", addr.Port
}

func waitForDashboard(t *testing.T, url string, done <-chan error) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := http.Client{Timeout: time.Second}
	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("startRunning returned before dashboard responded: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
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
		}

		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for dashboard at %s", url)
	return ""
}

func waitForDashboardCondition(t *testing.T, url string, done <-chan error, name string, ok func(string) bool) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	lastBody := ""
	for ctx.Err() == nil {
		body := waitForDashboard(t, url, done)
		lastBody = body
		if ok(body) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for dashboard condition %q at %s; last body:\n%s", name, url, lastBody)
	return ""
}

type bootProjectFiles struct {
	workflowPath string
	workdirPath  string
}

func createBootProjectFiles(t *testing.T) bootProjectFiles {
	t.Helper()

	workdir := t.TempDir()
	workflow := filepath.Join(workdir, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte(`---
tracker:
  kind: memory
---
Test workflow prompt.
`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return bootProjectFiles{
		workflowPath: workflow,
		workdirPath:  workdir,
	}
}

func writeBootHostWorkflow(t *testing.T, path string, host string) {
	t.Helper()

	content := `---
tracker:
  kind: memory
server:
  host: ` + host + `
---
Boot workflow.
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func writeBootGlobalConfig(t *testing.T, path string, projects []globalconfig.Project) {
	t.Helper()

	cfg := globalconfig.Config{
		Path:       path,
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 8,
			Scheduling:          globalconfig.SchedulingWeighted,
			FairShare:           map[string]any{"half_life": "1h"},
			Startup:             map[string]any{"jitter_seconds": 0, "max_spawn_per_second": 1},
		},
		Projects: projects,
	}
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
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

type bootProvisioningConnector struct {
	provision func(context.Context) error
}

func (bootProvisioningConnector) Name() string {
	return "boot"
}

func (bootProvisioningConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (bootProvisioningConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (bootProvisioningConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (bootProvisioningConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (bootProvisioningConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}

func (bootProvisioningConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (bootProvisioningConnector) SetField(context.Context, string, string, string) error {
	return nil
}

func (c bootProvisioningConnector) Provision(ctx context.Context) error {
	if c.provision == nil {
		return nil
	}
	return c.provision(ctx)
}
