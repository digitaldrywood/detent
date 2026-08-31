package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/digitaldrywood/detent/internal/activehours"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/instancelock"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tui"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestMain(m *testing.M) {
	if err := os.Unsetenv("DETENT_API_TOKEN"); err != nil {
		panic("clear DETENT_API_TOKEN: " + err.Error())
	}
	os.Exit(m.Run())
}

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

func TestBackfillRuntimeSessionProjects(t *testing.T) {
	t.Parallel()

	projects := []globalconfig.Project{{ID: "detent"}, {ID: "gopher-ai"}, {ID: "duplicate"}}
	backfiller := &fakeSessionProjectBackfiller{}
	err := backfillRuntimeSessionProjects(context.Background(), projects, backfiller, func(project globalconfig.Project) (workflowconfig.Workflow, error) {
		cfg := workflowconfig.Default()
		switch project.ID {
		case "detent":
			cfg.Tracker.Repository = "digitaldrywood/detent"
		case "gopher-ai", "duplicate":
			cfg.Tracker.Repository = "gopherguides/gopher-ai"
		}
		return workflowconfig.Workflow{Config: cfg}, nil
	})
	if err != nil {
		t.Fatalf("backfillRuntimeSessionProjects() error = %v", err)
	}
	want := []store.SessionProjectAttribution{{ProjectID: "detent", Repository: "digitaldrywood/detent"}}
	if len(backfiller.attributions) != len(want) || backfiller.attributions[0] != want[0] {
		t.Fatalf("attributions = %#v, want %#v", backfiller.attributions, want)
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

func TestGlobalPoolConfigsApplySchedulingInheritanceAndOverrides(t *testing.T) {
	t.Parallel()

	pools, err := globalPoolConfigs(globalconfig.Settings{
		MaxConcurrentAgents: 1,
		Scheduling:          globalconfig.SchedulingStrict,
		AgentPools: []globalconfig.AgentPool{
			{Name: "code", MaxConcurrentAgents: 5},
			{Name: "video", MaxConcurrentAgents: 10, BurstTo: 15, Scheduling: globalconfig.SchedulingRoundRobin},
		},
		FairShare: map[string]any{"half_life": "30m"},
	}, nil)
	if err != nil {
		t.Fatalf("globalPoolConfigs() error = %v", err)
	}
	if len(pools) != 3 {
		t.Fatalf("globalPoolConfigs() = %#v, want three pools", pools)
	}
	tests := []struct {
		index    int
		name     string
		capacity int
		kind     string
	}{
		{index: 0, name: scheduler.DefaultPoolName, capacity: 1, kind: globalconfig.SchedulingStrict},
		{index: 1, name: "code", capacity: 5, kind: globalconfig.SchedulingStrict},
		{index: 2, name: "video", capacity: 10, kind: globalconfig.SchedulingRoundRobin},
	}
	for _, tt := range tests {
		pool := pools[tt.index]
		if pool.Name != tt.name || pool.Scheduler.Capacity != tt.capacity || pool.Scheduler.Kind != tt.kind {
			t.Fatalf("pools[%d] = %#v, want name/capacity/kind %s/%d/%s", tt.index, pool, tt.name, tt.capacity, tt.kind)
		}
	}
	if pools[2].BurstTo != 15 {
		t.Fatalf("pools[2].BurstTo = %d, want 15", pools[2].BurstTo)
	}
}

func TestRuntimeLogLevelForReloadPreservesOverrides(t *testing.T) {
	t.Parallel()

	level := &slog.LevelVar{}
	tests := []struct {
		name   string
		source string
		want   *slog.LevelVar
	}{
		{name: "config", source: runtimeSourceConfig, want: level},
		{name: "default", source: runtimeSourceDefault, want: level},
		{name: "flag", source: runtimeSourceFlag},
		{name: "primary environment", source: "LOG_LEVEL"},
		{name: "deprecated environment", source: "DETENT_LOG_LEVEL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := BootConfig{
				Runtime:  RuntimeSettings{LogLevel: RuntimeValue{Source: tt.source}},
				LogLevel: level,
			}
			if got := runtimeLogLevelForReload(cfg); got != tt.want {
				t.Fatalf("runtimeLogLevelForReload() = %p, want %p", got, tt.want)
			}
		})
	}
}

func TestSyncGlobalDispatchProjectsMarksUnstartedProjectsIdle(t *testing.T) {
	t.Parallel()

	projects := []globalconfig.Project{
		{ID: "higher", Weight: 1, Priority: 0},
		{ID: "lower", Weight: 1, Priority: 3},
	}
	gate, err := scheduler.NewPoolRegistry(
		[]scheduler.PoolConfig{{
			Name: scheduler.DefaultPoolName,
			Scheduler: scheduler.Config{
				Kind:     globalconfig.SchedulingStrict,
				Capacity: 1,
			},
		}},
		globalProjectCandidates(projects),
	)
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	syncGlobalDispatchProjects(gate, projects, projectpkg.NewRegistry())

	lower := globalProjectCandidates(projects)[1]
	gate.BeginProjectCycle(lower)
	slot, ok, decision, err := gate.TryAcquireWithDecision(
		t.Context(),
		lower,
		scheduler.SlotRequest{State: "Todo"},
		time.Date(2026, 7, 10, 15, 0, 0, 0, time.Local),
	)
	gate.EndProjectCycle(lower.ID)
	if err != nil {
		t.Fatalf("TryAcquireWithDecision() error = %v", err)
	}
	if !ok || decision.Reason != scheduler.DispatchGateReasonGranted {
		t.Fatalf("TryAcquireWithDecision() ok = %t decision = %#v, want granted", ok, decision)
	}
	if err := gate.Release(slot); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestGlobalProjectCandidatesApplyHostActiveHours(t *testing.T) {
	t.Parallel()
	globalDefault := activehours.Config{Timezone: "America/Chicago", Windows: []string{"Mon-Sun 22:00-06:00"}}
	projectOverride := activehours.Config{Timezone: "America/New_York", Windows: []string{"Mon-Fri 21:00-05:00"}}
	projects := []globalconfig.Project{
		{ID: "default", ActiveHoursOverrideUntil: "2026-08-08T02:00:00Z"},
		{ID: "override", ActiveHours: &projectOverride},
	}

	candidates := globalProjectCandidatesWithDefault(projects, &globalDefault)
	if got := candidates[0].ActiveHours.Timezone; got != globalDefault.Timezone {
		t.Fatalf("default Timezone = %q, want %q", got, globalDefault.Timezone)
	}
	if got := candidates[1].ActiveHours.Timezone; got != projectOverride.Timezone {
		t.Fatalf("override Timezone = %q, want %q", got, projectOverride.Timezone)
	}
	if got := candidates[0].ActiveHoursOverrideUntil.Format(time.RFC3339); got != projects[0].ActiveHoursOverrideUntil {
		t.Fatalf("ActiveHoursOverrideUntil = %q, want %q", got, projects[0].ActiveHoursOverrideUntil)
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

func TestRedirectDefaultLoggerRotatesAndRetainsConfiguredBackups(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	path := filepath.Join(t.TempDir(), "runtime", "detent.log")
	restore, err := redirectDefaultLoggerWithRotation(path, "info", logRotation{
		MaxSizeBytes: 1,
		MaxBackups:   2,
	})
	if err != nil {
		t.Fatalf("redirectDefaultLoggerWithRotation() error = %v", err)
	}

	for index := 1; index <= 4; index++ {
		slog.Info("rotate message", "index", index)
	}
	restore()

	assertLogFileContains(t, path, `"index":4`)
	assertLogFileContains(t, rotatedLogPath(path, 1), `"index":3`)
	assertLogFileContains(t, rotatedLogPath(path, 2), `"index":2`)
	if _, err := os.Stat(rotatedLogPath(path, 3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotated log %s exists or stat failed: %v", rotatedLogPath(path, 3), err)
	}
}

func assertLogFileContains(t *testing.T, path string, want string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if !strings.Contains(string(raw), want) {
		t.Fatalf("log file %s missing %q:\n%s", path, want, string(raw))
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
		{
			name:  "terminal program killed during shutdown is clean",
			first: tea.ErrProgramKilled,
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

func TestRunTerminalDashboardProgramKillsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	program := newTerminalDashboardProgramProbe()
	errs := make(chan error, 1)
	go func() {
		_, err := runTerminalDashboardProgram(ctx, program)
		errs <- err
	}()

	select {
	case <-program.started:
	case <-time.After(time.Second):
		t.Fatal("program did not start")
	}
	cancel()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runTerminalDashboardProgram() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal dashboard program to exit")
	}
	if !program.killCalled() {
		t.Fatal("terminal dashboard program was not killed")
	}
}

func TestRunTerminalDashboardProgramRejectsNilProgram(t *testing.T) {
	t.Parallel()

	if _, err := runTerminalDashboardProgram(context.Background(), nil); !errors.Is(err, errNilTerminalDashboardProgram) {
		t.Fatalf("runTerminalDashboardProgram(nil) error = %v, want %v", err, errNilTerminalDashboardProgram)
	}
}

func TestRunTerminalDashboardProgramSurfacesFinalModelAndPrintsSummary(t *testing.T) {
	t.Parallel()

	model, err := tui.NewModel(
		context.Background(),
		hub.New[telemetry.Snapshot](),
		tui.WithNow(func() time.Time { return time.Date(2026, 7, 12, 20, 30, 45, 0, time.UTC) }),
		tui.WithLogPath("/var/log/detent.log"),
	)
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}
	t.Cleanup(model.Close)

	finalModel, err := runTerminalDashboardProgram(context.Background(), terminalDashboardCompletedProgram{model: model})
	if err != nil {
		t.Fatalf("runTerminalDashboardProgram() error = %v", err)
	}
	if _, ok := finalModel.(tui.Model); !ok {
		t.Fatalf("final model type = %T, want tui.Model", finalModel)
	}
	var output bytes.Buffer
	if err := writeTerminalDashboardSummary(&output, finalModel, tui.Model{}); err != nil {
		t.Fatalf("writeTerminalDashboardSummary() error = %v", err)
	}
	for _, want := range []string{"Detent exit summary", "Timestamp: 2026-07-12T20:30:45Z", "Logs: /var/log/detent.log"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("summary output missing %q: %s", want, output.String())
		}
	}
}

func TestTerminalDashboardSummaryFollowsTerminalRestore(t *testing.T) {
	t.Parallel()

	model, err := tui.NewModel(context.Background(), hub.New[telemetry.Snapshot](), tui.WithLogPath("/var/log/detent.log"))
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}
	t.Cleanup(model.Close)

	var output bytes.Buffer
	program := tea.NewProgram(
		terminalDashboardQuitModel{Model: model},
		append(terminalDashboardProgramOptions(), tea.WithInput(nil), tea.WithOutput(&output))...,
	)
	finalModel, err := runTerminalDashboardProgram(context.Background(), program)
	if err != nil {
		t.Fatalf("runTerminalDashboardProgram() error = %v", err)
	}
	if err := writeTerminalDashboardSummary(&output, finalModel, model); err != nil {
		t.Fatalf("writeTerminalDashboardSummary() error = %v", err)
	}
	restoreAt := strings.LastIndex(output.String(), "\x1b[?1049l")
	summaryAt := strings.Index(output.String(), "Detent exit summary")
	if restoreAt < 0 || summaryAt < 0 || restoreAt > summaryAt {
		t.Fatalf("terminal restore index = %d, summary index = %d: %q", restoreAt, summaryAt, output.String())
	}
}

func TestTerminalDashboardProgramOptionsUseAltScreen(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	opts := append(terminalDashboardProgramOptions(),
		tea.WithInput(nil),
		tea.WithOutput(&output),
	)
	if _, err := tea.NewProgram(terminalDashboardOptionModel{}, opts...).Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got := output.String()
	for _, sequence := range []string{"\x1b[?1049h", "\x1b[?1049l"} {
		if !strings.Contains(got, sequence) {
			t.Fatalf("terminal dashboard output missing %q:\n%q", sequence, got)
		}
	}
}

func TestTerminalDashboardMessageFilterRoutesInterruptToCtrlC(t *testing.T) {
	t.Parallel()

	interrupt := terminalDashboardMessageFilter(nil, tea.InterruptMsg{})
	key, ok := interrupt.(tea.KeyPressMsg)
	if !ok {
		t.Fatalf("filtered interrupt type = %T, want tea.KeyPressMsg", interrupt)
	}
	if got := key.String(); got != "ctrl+c" {
		t.Fatalf("filtered interrupt = %q, want ctrl+c", got)
	}

	windowSize := tea.WindowSizeMsg{Width: 80, Height: 24}
	if got := terminalDashboardMessageFilter(nil, windowSize); got != windowSize {
		t.Fatalf("filtered window size = %#v, want unchanged", got)
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

func TestMergeRefreshResponseKeepsInProgressWhenSomeProjectsAccepted(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC)
	retryAt := now.Add(5 * time.Minute)
	current := web.RefreshResponse{
		RequestID:   "manual-refused",
		Status:      telemetry.RefreshAttemptStatusRefused,
		Refused:     true,
		RequestedAt: now,
		LastError:   "GitHub REST backoff is active",
		LastErrorAt: &now,
		RetryAt:     &retryAt,
		Operations:  []string{"poll"},
	}
	next := web.RefreshResponse{
		RequestID:   "manual-accepted",
		Status:      telemetry.RefreshAttemptStatusInProgress,
		Queued:      true,
		RequestedAt: now.Add(time.Second),
		Operations:  []string{"reconcile"},
	}

	got := mergeRefreshResponse(current, next)
	if got.Status != telemetry.RefreshAttemptStatusInProgress {
		t.Fatalf("Status = %q, want %q; response = %#v", got.Status, telemetry.RefreshAttemptStatusInProgress, got)
	}
	if !got.Refused || !got.Queued {
		t.Fatalf("Refused/Queued = %v/%v, want true/true; response = %#v", got.Refused, got.Queued, got)
	}
	if got.LastError != current.LastError || got.RetryAt == nil || !got.RetryAt.Equal(retryAt) {
		t.Fatalf("backoff fields = error %q retry %v, want preserved refusal detail", got.LastError, got.RetryAt)
	}
	if !hasOperation(got.Operations, "poll") || !hasOperation(got.Operations, "reconcile") {
		t.Fatalf("Operations = %#v, want merged operations", got.Operations)
	}
}

func TestRegistryRefresherTargetsProjectV2WithoutConfiguredRepository(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, startRefreshProject(t, "project-v2"))

	refresher := refresherForRegistry(registry)
	targeted, ok := refresher.(web.TargetedRefresher)
	if !ok {
		t.Fatal("refresherForRegistry() does not implement TargetedRefresher")
	}
	response, err := targeted.RequestTargetedRefresh(context.Background(), web.RefreshTarget{
		Repository:  "digitaldrywood/detent",
		Event:       "projects_v2_item",
		DeliveryID:  "delivery-1",
		IssueNumber: 666,
	})
	if err != nil {
		t.Fatalf("RequestTargetedRefresh() error = %v", err)
	}
	if !hasOperation(response.Operations, "target:digitaldrywood/detent") {
		t.Fatalf("Operations = %#v, want target marker", response.Operations)
	}
	if !hasOperation(response.Operations, "targeted_reconcile:digitaldrywood/detent") {
		t.Fatalf("Operations = %#v, want targeted reconcile marker", response.Operations)
	}
}

func TestRegistryRefresherTargetsBranchOnlyWebhook(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, startRefreshProject(t, "branch-target"))

	refresher := refresherForRegistry(registry)
	targeted, ok := refresher.(web.TargetedRefresher)
	if !ok {
		t.Fatal("refresherForRegistry() does not implement TargetedRefresher")
	}
	response, err := targeted.RequestTargetedRefresh(context.Background(), web.RefreshTarget{
		Repository: "digitaldrywood/detent",
		Branch:     "detent/digitaldrywood_detent_1133-feature",
		Event:      "check_run",
		DeliveryID: "delivery-branch",
	})
	if err != nil {
		t.Fatalf("RequestTargetedRefresh() error = %v", err)
	}
	if !hasOperation(response.Operations, "target:digitaldrywood/detent") {
		t.Fatalf("Operations = %#v, want target marker", response.Operations)
	}
	if !hasOperation(response.Operations, "targeted_reconcile:digitaldrywood/detent") {
		t.Fatalf("Operations = %#v, want targeted reconcile marker", response.Operations)
	}
}

func TestStartRunningBootsDashboardAndStopsOnContextCancel(t *testing.T) {
	port := 0
	output := newBootOutput()
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
			Host:   "127.0.0.1",
			Port:   &port,
			Output: output,
		})
	}()

	baseURL := waitForBootDashboardURL(t, output, done)
	body := waitForDashboard(t, baseURL+"/", done)
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

func TestStartRunningRefusesSharedRuntimeDatabase(t *testing.T) {
	port := 0
	root := t.TempDir()
	global, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"))
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}
	dbPath := filepath.Join(root, "detent.db")

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	t.Cleanup(cancelFirst)
	firstDone := make(chan error, 1)
	firstOutput := newBootOutput()
	go func() {
		firstDone <- startRunning(firstCtx, BootConfig{
			Mode:          BootModeRunning,
			Global:        global,
			Host:          "127.0.0.1",
			Port:          &port,
			RuntimeDBPath: dbPath,
			Output:        firstOutput,
		})
	}()
	waitForBootDashboardURL(t, firstOutput, firstDone)

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	t.Cleanup(cancelSecond)
	secondDone := make(chan error, 1)
	secondOutput := newBootOutput()
	go func() {
		secondDone <- startRunning(secondCtx, BootConfig{
			Mode:          BootModeRunning,
			Global:        global,
			Host:          "127.0.0.1",
			Port:          &port,
			RuntimeDBPath: dbPath,
			Output:        secondOutput,
		})
	}()

	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "another detent (pid ") {
			t.Fatalf("second startRunning() error = %v, want shared runtime database error", err)
		}
	case <-time.After(2 * time.Second):
		cancelSecond()
		t.Fatalf("second startRunning() did not refuse shared database; output:\n%s", secondOutput.String())
	}

	cancelFirst()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first startRunning() error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first runtime to stop")
	}
}

func TestStartupServerURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr *net.TCPAddr
		want string
	}{
		{name: "specific host", addr: &net.TCPAddr{IP: net.ParseIP("100.109.187.102"), Port: 4000}, want: "http://100.109.187.102:4000"},
		{name: "IPv4 wildcard", addr: &net.TCPAddr{IP: net.IPv4zero, Port: 4000}, want: "http://127.0.0.1:4000"},
		{name: "IPv6 wildcard", addr: &net.TCPAddr{IP: net.IPv6zero, Port: 4000}, want: "http://[::1]:4000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := startupServerURL(tt.addr); got != tt.want {
				t.Fatalf("startupServerURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRuntimeStoreLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		memory   bool
		lockPath string
	}{
		{name: "private memory", path: ":memory:", memory: true, lockPath: ":memory:.lock"},
		{name: "memory prefix is file", path: ":memory:extra", lockPath: ":memory:extra.lock"},
		{name: "memory text in file path", path: "/var/lib/detent/mode=memory/detent.db", lockPath: "/var/lib/detent/mode=memory/detent.db.lock"},
		{name: "named memory URI", path: "file:detent?mode=memory&cache=shared", memory: true, lockPath: "detent.lock"},
		{name: "special memory URI", path: "file::memory:?cache=shared", memory: true, lockPath: ":memory:.lock"},
		{name: "absolute file URI", path: "file:/tmp/detent.db?mode=rw", lockPath: filepath.FromSlash("/tmp/detent.db.lock")},
		{name: "localhost file URI", path: "file://localhost/tmp/detent.db?mode=rwc", lockPath: filepath.FromSlash("/tmp/detent.db.lock")},
		{name: "escaped file URI", path: "file:/tmp/detent%20runtime.db?mode=rwc#ignored", lockPath: filepath.FromSlash("/tmp/detent runtime.db.lock")},
		{name: "relative file URI", path: "file:detent.db?mode=rw", lockPath: "detent.db.lock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := runtimeStoreIsMemory(tt.path); got != tt.memory {
				t.Fatalf("runtimeStoreIsMemory(%q) = %v, want %v", tt.path, got, tt.memory)
			}
			if got := runtimeStoreLockPath(tt.path); got != tt.lockPath {
				t.Fatalf("runtimeStoreLockPath(%q) = %q, want %q", tt.path, got, tt.lockPath)
			}
		})
	}
}

func TestRuntimeBoardSnapshotPath(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	tests := []struct {
		name     string
		database string
		want     string
	}{
		{name: "memory database", database: ":memory:", want: filepath.Join(home, "detent-board-snapshot.json")},
		{name: "file database", database: filepath.Join(home, "runtime.db"), want: filepath.Join(home, "runtime-board-snapshot.json")},
		{name: "file URI", database: "file:" + filepath.ToSlash(filepath.Join(home, "runtime.db")) + "?mode=rwc", want: filepath.Join(home, "runtime-board-snapshot.json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := BootConfig{
				Global:        globalconfig.Config{Path: filepath.Join(home, "global.yaml")},
				RuntimeDBPath: tt.database,
			}
			if got := runtimeBoardSnapshotPath(cfg); got != tt.want {
				t.Fatalf("runtimeBoardSnapshotPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRuntimeUpdateStatePath(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	tests := []struct {
		name     string
		database string
		want     string
	}{
		{name: "memory database", database: ":memory:", want: filepath.Join(home, "detent-update-state.json")},
		{name: "file database", database: filepath.Join(home, "runtime.db"), want: filepath.Join(home, "runtime-update-state.json")},
		{name: "file URI", database: "file:" + filepath.ToSlash(filepath.Join(home, "runtime.db")) + "?mode=rwc", want: filepath.Join(home, "runtime-update-state.json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := BootConfig{
				Global:        globalconfig.Config{Path: filepath.Join(home, "global.yaml")},
				RuntimeDBPath: tt.database,
			}
			if got := runtimeUpdateStatePath(cfg); got != tt.want {
				t.Fatalf("runtimeUpdateStatePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAcquireRuntimeInstanceLockReportsLiveHolder(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	lock, err := instancelock.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	_, err = acquireRuntimeInstanceLock(path)
	if err == nil {
		t.Fatal("acquireRuntimeInstanceLock() error = nil, want live holder error")
	}
	for _, want := range []string{"another detent (pid ", "started ", ") holds " + path} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("acquireRuntimeInstanceLock() error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("acquireRuntimeInstanceLock() error = %q, want actionable contention error", err)
	}
}

func TestStartRunningBindsBeforeCreatingRuntimeDatabase(t *testing.T) {
	t.Parallel()

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("listener Close() error = %v", err)
		}
	})
	port := listener.Addr().(*net.TCPAddr).Port
	root := t.TempDir()
	dbPath := filepath.Join(root, "detent.db")
	global, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"))
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}

	err = startRunning(context.Background(), BootConfig{
		Mode:          BootModeRunning,
		Global:        global,
		Host:          "127.0.0.1",
		Port:          &port,
		RuntimeDBPath: dbPath,
		Output:        io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "bind Detent web listener") {
		t.Fatalf("startRunning() error = %v, want listener bind error", err)
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime database exists before listener bind, Stat() error = %v", err)
	}
}

func TestStartRunningUsesWorkflowKanbanModeForFleetActions(t *testing.T) {
	port := 0
	output := newBootOutput()
	configPath := filepath.Join(t.TempDir(), "global.yaml")
	alpha := createBootProjectFiles(t)
	writeBootKanbanWorkflow(t, alpha.workflowPath, workflowconfig.KanbanModeIntegration)
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
			Host:   "127.0.0.1",
			Port:   &port,
			Output: output,
		})
	}()

	baseURL := waitForBootDashboardURL(t, output, done)
	waitForDashboard(t, baseURL+"/kanban", done)
	status, body := postDashboardForm(t, baseURL+"/api/v1/kanban/move", done, url.Values{
		"issue_id":      {"issue-1"},
		"current_state": {"Ready"},
		"target_state":  {"Doing"},
	})
	if status == http.StatusForbidden && strings.Contains(body, "Kanban integration mode is not enabled.") {
		t.Fatalf("move API stayed read-only after workflow integration mode; status = %d body = %s", status, body)
	}
	if strings.Contains(body, "Kanban integration mode is not enabled.") {
		t.Fatalf("move API body kept read-only gate after workflow integration mode; status = %d body = %s", status, body)
	}
	if strings.Contains(body, "Target state is not configured for this board.") {
		t.Fatalf("move API lost workflow Kanban states; status = %d body = %s", status, body)
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

func TestStartRunningPublishesStartupSnapshotBeforeProjectStartCompletes(t *testing.T) {
	port := 0
	output := newBootOutput()
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
			Host:   "127.0.0.1",
			Port:   &port,
			Output: output,
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

	if err := awaitProjectProvisioningStart(provisionStarted, done, output, bootProvisioningStartTimeout); err != nil {
		t.Fatal(err)
	}

	baseURL := waitForBootDashboardURL(t, output, done)
	stateURL := baseURL + "/api/v1/state"
	body := waitForDashboardCondition(t, stateURL, done, "startup snapshot", func(body string) bool {
		return strings.Contains(body, `"generated_at"`) &&
			strings.Contains(body, `"alpha"`) &&
			!strings.Contains(body, "snapshot_unavailable")
	})
	if !strings.Contains(body, `"running":0`) {
		t.Fatalf("startup snapshot body missing zero running count:\n%s", body)
	}
	healthRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		t.Fatalf("create GET /health request error = %v", err)
	}
	healthResponse, err := (&http.Client{Timeout: time.Second}).Do(healthRequest)
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	healthBody, readErr := io.ReadAll(healthResponse.Body)
	closeErr := healthResponse.Body.Close()
	if readErr != nil {
		t.Fatalf("read /health response error = %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close /health response error = %v", closeErr)
	}
	if healthResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /health status = %d, want %d; body = %s", healthResponse.StatusCode, http.StatusServiceUnavailable, healthBody)
	}
	for _, want := range []string{`"status":"not_ready"`, `"ready":false`, `"lifecycle":"starting"`} {
		if !strings.Contains(string(healthBody), want) {
			t.Fatalf("startup health response missing %q:\n%s", want, healthBody)
		}
	}

	close(provisionRelease)
	healthBody = []byte(waitForDashboard(t, baseURL+"/health", done))
	for _, want := range []string{`"status":"ok"`, `"ready":true`, `"lifecycle":"ready"`} {
		if !strings.Contains(string(healthBody), want) {
			t.Fatalf("ready health response missing %q:\n%s", want, healthBody)
		}
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

func TestStartRunningSurvivesTransientConnectorProvisioningFailure(t *testing.T) {
	port := 0
	output := newBootOutput()
	configPath := filepath.Join(t.TempDir(), "global.yaml")
	alpha := createBootProjectFiles(t)
	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "alpha", Workflow: alpha.workflowPath, Workdir: alpha.workdirPath, Weight: 1},
	})
	global, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	firstAttempt := make(chan struct{})
	retryScheduled := make(chan struct{})
	releaseRetry := make(chan struct{})
	var firstAttemptOnce sync.Once
	var retryScheduledOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- startRunningWithDependencies(ctx, BootConfig{
			Mode:   BootModeRunning,
			Global: global,
			Host:   "127.0.0.1",
			Port:   &port,
			Output: output,
			ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
				return bootProvisioningConnector{
					provision: func(ctx context.Context) error {
						failed := false
						firstAttemptOnce.Do(func() {
							failed = true
							close(firstAttempt)
						})
						if failed {
							return githubconnector.ErrTransient
						}
						<-ctx.Done()
						return ctx.Err()
					},
				}, nil
			},
		}, startRunningDependencies{
			managerDependencies: projectpkg.ManagerDependencies{
				RetrySleep: func(ctx context.Context, _ time.Duration) error {
					retryScheduledOnce.Do(func() {
						close(retryScheduled)
					})
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-releaseRetry:
						return nil
					}
				},
			},
		})
	}()

	select {
	case <-firstAttempt:
	case err := <-done:
		t.Fatalf("startRunning() returned before first provisioning attempt: %v", err)
	case <-time.After(bootProvisioningStartTimeout):
		t.Fatal("timed out waiting for transient provisioning attempt")
	}

	select {
	case err := <-done:
		t.Fatalf("startRunning() returned after transient provisioning failure: %v", err)
	case <-retryScheduled:
	}

	baseURL := waitForBootDashboardURL(t, output, done)
	health := waitForDashboard(t, baseURL+"/health", done)
	for _, want := range []string{
		`"status":"ok"`,
		`"project_status":"degraded"`,
		`"project_id":"alpha"`,
		`"status":"degraded"`,
		`"next_retry_at"`,
		"github transient error",
	} {
		if !strings.Contains(health, want) {
			t.Fatalf("health response missing %q:\n%s", want, health)
		}
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

func TestStartRunningHotReloadsGlobalConfigProjects(t *testing.T) {
	port := 0
	output := newBootOutput()
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
			Host:   "127.0.0.1",
			Port:   &port,
			Output: output,
		})
	}()

	baseURL := waitForBootDashboardURL(t, output, done)
	settingsURL := baseURL + "/settings"
	waitForDashboardCondition(t, settingsURL, done, "initial alpha project", func(body string) bool {
		return strings.Contains(body, "alpha")
	})

	writeBootGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "alpha", Workflow: alpha.workflowPath, Workdir: alpha.workdirPath, Weight: 1},
		{ID: "bravo", Workflow: bravo.workflowPath, Workdir: bravo.workdirPath, Weight: 1},
	})
	body := waitForDashboardCondition(t, settingsURL, done, "bravo added", func(body string) bool {
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
	port := 0
	output := newBootOutput()
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
			Host:   "127.0.0.1",
			Port:   &port,
			Output: output,
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

	if err := awaitProjectProvisioningStart(provisionStarted, done, output, bootProvisioningStartTimeout); err != nil {
		t.Fatal(err)
	}

	baseURL := waitForBootDashboardURL(t, output, done)
	settingsURL := baseURL + "/settings"
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

func TestAwaitProjectProvisioningStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		started     bool
		done        bool
		doneErr     error
		timeout     time.Duration
		wantErrText string
	}{
		{
			name:    "provisioning starts",
			started: true,
			timeout: time.Second,
		},
		{
			name:        "startup error",
			done:        true,
			doneErr:     errors.New("store unavailable"),
			timeout:     time.Second,
			wantErrText: "startRunning returned before project provisioning started: store unavailable",
		},
		{
			name:        "startup stops without error",
			done:        true,
			timeout:     time.Second,
			wantErrText: "startRunning returned before project provisioning started without error",
		},
		{
			name:        "deadline while startup remains active",
			timeout:     0,
			wantErrText: "project provisioning did not start within 0s; startRunning remained active; output:\nDetent dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provisionStarted := make(chan struct{})
			if tt.started {
				close(provisionStarted)
			}
			done := make(chan error, 1)
			if tt.done {
				done <- tt.doneErr
			}
			output := newBootOutput()
			if _, err := output.Write([]byte("Detent dev\n")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			err := awaitProjectProvisioningStart(provisionStarted, done, output, tt.timeout)
			if tt.wantErrText == "" {
				if err != nil {
					t.Fatalf("awaitProjectProvisioningStart() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
				t.Fatalf("awaitProjectProvisioningStart() error = %v, want containing %q", err, tt.wantErrText)
			}
		})
	}
}

func TestAwaitBootDashboardURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		writes      []string
		done        bool
		doneErr     error
		timeout     time.Duration
		wantURL     string
		wantErrText string
	}{
		{
			name:    "dashboard banner",
			writes:  []string{"Detent dev\nDashboard: ", "http://127.0.0.1:12345\n"},
			timeout: time.Second,
			wantURL: "http://127.0.0.1:12345",
		},
		{
			name:        "startup error",
			done:        true,
			doneErr:     errors.New("store unavailable"),
			timeout:     time.Second,
			wantErrText: "startRunning returned before boot banner was written: store unavailable",
		},
		{
			name:        "startup stops without error",
			done:        true,
			timeout:     time.Second,
			wantErrText: "startRunning returned before boot banner was written without error",
		},
		{
			name:        "diagnostic timeout",
			writes:      []string{"Detent dev\nProject: https://github.com/digitaldrywood/detent\n"},
			timeout:     0,
			wantErrText: "timed out waiting for boot dashboard URL; output:\nDetent dev\nProject: https://github.com/digitaldrywood/detent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			output := newBootOutput()
			for _, write := range tt.writes {
				if _, err := output.Write([]byte(write)); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
			}
			done := make(chan error, 1)
			if tt.done {
				done <- tt.doneErr
			}

			url, err := awaitBootDashboardURL(output, done, tt.timeout)
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("awaitBootDashboardURL() error = %v, want containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("awaitBootDashboardURL() error = %v", err)
			}
			if url != tt.wantURL {
				t.Fatalf("awaitBootDashboardURL() = %q, want %q", url, tt.wantURL)
			}
		})
	}
}

func TestAwaitDashboard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(*testing.T) (context.Context, *http.Client, string, <-chan error)
		wantBody    string
		wantErrText string
	}{
		{
			name: "dashboard ready",
			setup: func(t *testing.T) (context.Context, *http.Client, string, <-chan error) {
				t.Helper()

				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					_, _ = writer.Write([]byte("ready"))
				}))
				t.Cleanup(server.Close)
				return context.Background(), server.Client(), server.URL, make(chan error)
			},
			wantBody: "ready",
		},
		{
			name: "runtime exits early",
			setup: func(*testing.T) (context.Context, *http.Client, string, <-chan error) {
				done := make(chan error, 1)
				done <- errors.New("store unavailable")
				return context.Background(), http.DefaultClient, "http://127.0.0.1:0/health", done
			},
			wantErrText: "startRunning returned before dashboard responded: store unavailable",
		},
		{
			name: "runtime exits without error",
			setup: func(*testing.T) (context.Context, *http.Client, string, <-chan error) {
				done := make(chan error, 1)
				done <- nil
				return context.Background(), http.DefaultClient, "http://127.0.0.1:0/health", done
			},
			wantErrText: "startRunning returned before dashboard responded without error",
		},
		{
			name: "deadlock guard expires",
			setup: func(t *testing.T) (context.Context, *http.Client, string, <-chan error) {
				t.Helper()

				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, http.DefaultClient, "http://127.0.0.1:0/health", make(chan error)
			},
			wantErrText: "timed out waiting for dashboard at http://127.0.0.1:0/health: context canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, client, url, done := tt.setup(t)
			body, err := awaitDashboard(ctx, client, url, done)
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("awaitDashboard() error = %v, want containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("awaitDashboard() error = %v", err)
			}
			if body != tt.wantBody {
				t.Fatalf("awaitDashboard() = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

const (
	bootProvisioningStartTimeout    = 45 * time.Second
	bootDashboardURLTimeout         = 45 * time.Second
	dashboardReadinessDeadlockGuard = 2 * time.Minute
)

type bootOutput struct {
	mu                    sync.Mutex
	buffer                bytes.Buffer
	dashboardURL          chan string
	dashboardURLPublished bool
}

func newBootOutput() *bootOutput {
	return &bootOutput{dashboardURL: make(chan string, 1)}
}

func (o *bootOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	n, err := o.buffer.Write(p)
	if err != nil || o.dashboardURLPublished {
		return n, err
	}
	url := bootDashboardURL(o.buffer.String())
	if url == "" {
		return n, nil
	}
	o.dashboardURLPublished = true
	o.dashboardURL <- url
	return n, nil
}

func (o *bootOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buffer.String()
}

func awaitProjectProvisioningStart(
	provisionStarted <-chan struct{},
	done <-chan error,
	output *bootOutput,
	timeout time.Duration,
) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-provisionStarted:
		return nil
	case err := <-done:
		return projectProvisioningStartExitError(err)
	case <-timer.C:
		select {
		case <-provisionStarted:
			return nil
		case err := <-done:
			return projectProvisioningStartExitError(err)
		default:
			return fmt.Errorf(
				"project provisioning did not start within %s; startRunning remained active; output:\n%s",
				timeout,
				output.String(),
			)
		}
	}
}

func projectProvisioningStartExitError(err error) error {
	if err == nil {
		return errors.New("startRunning returned before project provisioning started without error")
	}
	return fmt.Errorf("startRunning returned before project provisioning started: %w", err)
}

func waitForBootDashboardURL(t *testing.T, output *bootOutput, done <-chan error) string {
	t.Helper()

	url, err := awaitBootDashboardURL(output, done, bootDashboardURLTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func awaitBootDashboardURL(output *bootOutput, done <-chan error, timeout time.Duration) (string, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err == nil {
			return "", errors.New("startRunning returned before boot banner was written without error")
		}
		return "", fmt.Errorf("startRunning returned before boot banner was written: %w", err)
	case url := <-output.dashboardURL:
		return url, nil
	case <-timer.C:
		return "", fmt.Errorf("timed out waiting for boot dashboard URL; output:\n%s", output.String())
	}
}

func bootDashboardURL(output string) string {
	lines := strings.Split(output, "\n")
	for _, line := range lines[:len(lines)-1] {
		url, ok := strings.CutPrefix(line, "Dashboard: ")
		if ok && strings.TrimSpace(url) != "" {
			return strings.TrimSpace(url)
		}
	}
	return ""
}

func waitForDashboard(t *testing.T, url string, done <-chan error) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), dashboardReadinessDeadlockGuard)
	defer cancel()
	return waitForDashboardContext(t, ctx, url, done)
}

func waitForDashboardContext(t *testing.T, ctx context.Context, url string, done <-chan error) string {
	t.Helper()

	client := http.Client{Timeout: time.Second}
	body, err := awaitDashboard(ctx, &client, url, done)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func awaitDashboard(ctx context.Context, client *http.Client, url string, done <-chan error) (string, error) {
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()

	for {
		select {
		case err := <-done:
			return "", dashboardExitError(err)
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for dashboard at %s: %w", url, ctx.Err())
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("create dashboard readiness request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				return "", fmt.Errorf("read dashboard readiness response: %w", readErr)
			}
			if closeErr != nil {
				return "", fmt.Errorf("close dashboard readiness response: %w", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body), nil
			}
		}

		select {
		case err := <-done:
			return "", dashboardExitError(err)
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for dashboard at %s: %w", url, ctx.Err())
		case <-retry.C:
		}
	}
}

func dashboardExitError(err error) error {
	if err == nil {
		return errors.New("startRunning returned before dashboard responded without error")
	}
	return fmt.Errorf("startRunning returned before dashboard responded: %w", err)
}

func waitForDashboardCondition(t *testing.T, url string, done <-chan error, name string, ok func(string) bool) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	lastBody := ""
	for ctx.Err() == nil {
		body := waitForDashboardContext(t, ctx, url, done)
		lastBody = body
		if ok(body) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for dashboard condition %q at %s; last body:\n%s", name, url, lastBody)
	return ""
}

func postDashboardForm(t *testing.T, rawURL string, done <-chan error, form url.Values) (int, string) {
	t.Helper()

	client := http.Client{Timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("startRunning returned before form post completed: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
			return resp.StatusCode, string(body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting form to %s", rawURL)
	return 0, ""
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

func writeBootKanbanWorkflow(t *testing.T, path string, mode string) {
	t.Helper()

	content := `---
tracker:
  kind: memory
  observed_states: [Backlog, Blocked]
  active_states: [Ready, Doing]
  terminal_states: [Done]
server:
  kanban:
    mode: ` + mode + `
agent:
  auto_promote:
    no_progress_limit: 0
---
Test workflow prompt.
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
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

type terminalDashboardProgramProbe struct {
	started chan struct{}
	killed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	killSet bool
}

type terminalDashboardCompletedProgram struct {
	model tea.Model
}

func (p terminalDashboardCompletedProgram) Run() (tea.Model, error) {
	return p.model, nil
}

func (terminalDashboardCompletedProgram) Kill() {}

func newTerminalDashboardProgramProbe() *terminalDashboardProgramProbe {
	return &terminalDashboardProgramProbe{
		started: make(chan struct{}),
		killed:  make(chan struct{}),
	}
}

func (p *terminalDashboardProgramProbe) Run() (tea.Model, error) {
	close(p.started)
	<-p.killed
	return nil, nil //nolint:nilnil // Test probe exits without yielding another Bubble Tea model.
}

func (p *terminalDashboardProgramProbe) Kill() {
	p.mu.Lock()
	p.killSet = true
	p.mu.Unlock()
	p.once.Do(func() {
		close(p.killed)
	})
}

func (p *terminalDashboardProgramProbe) killCalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killSet
}

type terminalDashboardOptionModel struct{}

func (terminalDashboardOptionModel) Init() tea.Cmd {
	return tea.Quit
}

func (terminalDashboardOptionModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return terminalDashboardOptionModel{}, nil
}

func (terminalDashboardOptionModel) View() tea.View {
	view := tea.NewView("detent")
	view.AltScreen = true
	return view
}

type terminalDashboardQuitModel struct {
	tui.Model
}

func (m terminalDashboardQuitModel) Init() tea.Cmd {
	return tea.Quit
}

func (m terminalDashboardQuitModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return m, nil
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

type fakeSessionProjectBackfiller struct {
	attributions []store.SessionProjectAttribution
}

func (f *fakeSessionProjectBackfiller) BackfillSessionProjectIDs(_ context.Context, attributions []store.SessionProjectAttribution) (int64, error) {
	f.attributions = append([]store.SessionProjectAttribution(nil), attributions...)
	return int64(len(attributions)), nil
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
