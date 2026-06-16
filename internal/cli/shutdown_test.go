package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hub"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestShutdownControllerQueuesRequests(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	controller.RequestDrain()
	controller.RequestForce()

	if got := <-controller.Requests(); got != ShutdownRequestDrain {
		t.Fatalf("first request = %v, want drain", got)
	}
	if got := <-controller.Requests(); got != ShutdownRequestForce {
		t.Fatalf("second request = %v, want force", got)
	}
}

func TestShutdownControllerInterruptRequestsDrainThenForce(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	if controller.RequestInterrupt() {
		t.Fatal("inactive interrupt was handled")
	}
	if request, handled := controller.RequestInterruptKind(); handled || request != 0 {
		t.Fatalf("inactive interrupt kind = %v, %v, want 0, false", request, handled)
	}

	deactivate := controller.activate()
	defer deactivate()

	request, handled := controller.RequestInterruptKind()
	if !handled {
		t.Fatal("active interrupt kind was not handled")
	}
	if request != ShutdownRequestDrain {
		t.Fatalf("first interrupt kind = %v, want drain", request)
	}
	if got := <-controller.Requests(); got != ShutdownRequestDrain {
		t.Fatalf("first interrupt = %v, want drain", got)
	}

	request, handled = controller.RequestInterruptKind()
	if !handled {
		t.Fatal("second interrupt kind was not handled")
	}
	if request != ShutdownRequestForce {
		t.Fatalf("second interrupt kind = %v, want force", request)
	}
	if got := <-controller.Requests(); got != ShutdownRequestForce {
		t.Fatalf("second interrupt = %v, want force", got)
	}
}

func TestShutdownControllerDrainMakesNextInterruptForce(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	deactivate := controller.activate()
	defer deactivate()

	controller.RequestDrain()
	if got := <-controller.Requests(); got != ShutdownRequestDrain {
		t.Fatalf("explicit drain = %v, want drain", got)
	}

	if !controller.RequestInterrupt() {
		t.Fatal("interrupt after drain was not handled")
	}
	if got := <-controller.Requests(); got != ShutdownRequestForce {
		t.Fatalf("interrupt after drain = %v, want force", got)
	}
}

func TestRequestTerminalShutdownInterruptHardExitsOnSecondInterrupt(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	deactivate := controller.activate()
	defer deactivate()

	var exits []int
	hardExit := func(code int) {
		exits = append(exits, code)
	}

	if !requestTerminalShutdownInterrupt(controller, hardExit) {
		t.Fatal("first interrupt was not handled")
	}
	if len(exits) != 0 {
		t.Fatalf("first interrupt exit codes = %v, want none", exits)
	}
	if got := <-controller.Requests(); got != ShutdownRequestDrain {
		t.Fatalf("first interrupt request = %v, want drain", got)
	}

	if !requestTerminalShutdownInterrupt(controller, hardExit) {
		t.Fatal("second interrupt was not handled")
	}
	if got := <-controller.Requests(); got != ShutdownRequestForce {
		t.Fatalf("second interrupt request = %v, want force", got)
	}
	if len(exits) != 1 || exits[0] != ExitGeneral {
		t.Fatalf("second interrupt exit codes = %v, want [%d]", exits, ExitGeneral)
	}
}

func TestRunWithShutdownZeroSessionsExitsGracefully(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	var output bytes.Buffer
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:       controller,
			Registry:         projectpkg.NewRegistry(),
			SnapshotHub:      hub.New[telemetry.Snapshot](),
			Output:           &output,
			ProgressInterval: time.Millisecond,
			HardTimeout:      time.Second,
			Now: func() time.Time {
				return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
			},
		}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	controller.RequestDrain()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runWithShutdown() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
	if got := output.String(); !strings.Contains(got, "shutdown requested — no agent sessions in flight") {
		t.Fatalf("output missing zero-session notice:\n%s", got)
	}
}

func TestRunWithShutdownTerminalDashboardSuppressesPlainOutput(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	var output bytes.Buffer
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:        controller,
			Registry:          projectpkg.NewRegistry(),
			SnapshotHub:       hub.New[telemetry.Snapshot](),
			Output:            &output,
			TerminalDashboard: true,
			HardTimeout:       time.Second,
			Now: func() time.Time {
				return time.Date(2026, 6, 16, 16, 0, 0, 0, time.UTC)
			},
		}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	controller.RequestDrain()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runWithShutdown() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
	if got := output.String(); got != "" {
		t.Fatalf("terminal dashboard shutdown wrote plain output:\n%s", got)
	}
}

func TestRunWithShutdownZeroSessionsLogsCleanupBoundaries(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	var logs bytes.Buffer
	errs := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:       controller,
			Registry:         projectpkg.NewRegistry(),
			SnapshotHub:      hub.New[telemetry.Snapshot](),
			Output:           io.Discard,
			Logger:           logger,
			DrainTimeout:     time.Hour,
			ProgressInterval: time.Hour,
			HardTimeout:      time.Second,
			Now: func() time.Time {
				return time.Date(2026, 6, 16, 16, 0, 0, 0, time.UTC)
			},
		}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	start := time.Now()
	controller.RequestDrain()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runWithShutdown() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
	if elapsed := time.Since(start); elapsed >= shutdownDrainPollInterval {
		t.Fatalf("zero-session shutdown waited %s, want less than %s", elapsed, shutdownDrainPollInterval)
	}

	got := logs.String()
	for _, want := range []string{
		"operation=initial_running_session_inventory",
		"operation=drain_projects",
		"operation=shutdown_snapshot_publish",
		"operation=stop_projects",
		"operation=serve_cancel",
		"operation=wait_for_serve_exit",
		"sessions=0",
		"duration=",
		"result=ok",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("debug logs missing %q:\n%s", want, got)
		}
	}
}

func TestRunWithShutdownZeroSessionsExitsWhenServeIgnoresCancellation(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:       controller,
			Registry:         projectpkg.NewRegistry(),
			SnapshotHub:      hub.New[telemetry.Snapshot](),
			Output:           io.Discard,
			ProgressInterval: time.Millisecond,
			HardTimeout:      20 * time.Millisecond,
			Now: func() time.Time {
				return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
			},
		}, func(context.Context) error {
			close(started)
			select {}
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	controller.RequestDrain()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runWithShutdown() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
}

func TestRunWithShutdownMarksControllerActive(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	ctx, cancel := context.WithCancel(context.Background())
	active := make(chan bool, 1)
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(ctx, runningShutdownConfig{
			Controller:  controller,
			Registry:    projectpkg.NewRegistry(),
			SnapshotHub: hub.New[telemetry.Snapshot](),
			HardTimeout: time.Second,
		}, func(ctx context.Context) error {
			active <- controller.Active()
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case got := <-active:
		if !got {
			t.Fatal("controller active = false while runWithShutdown is serving")
		}
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runWithShutdown() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown")
	}
	if controller.Active() {
		t.Fatal("controller active = true after runWithShutdown returned")
	}
}

func TestRunWithShutdownForceReturnsForcedError(t *testing.T) {
	t.Parallel()

	controller := NewShutdownController()
	started := make(chan struct{})
	var output bytes.Buffer
	errs := make(chan error, 1)

	go func() {
		errs <- runWithShutdown(context.Background(), runningShutdownConfig{
			Controller:  controller,
			Registry:    projectpkg.NewRegistry(),
			SnapshotHub: hub.New[telemetry.Snapshot](),
			Output:      &output,
			HardTimeout: time.Second,
			Now: func() time.Time {
				return time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
			},
		}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start")
	}
	controller.RequestForce()

	select {
	case err := <-errs:
		if !errors.Is(err, ErrShutdownForced) {
			t.Fatalf("runWithShutdown() error = %v, want ErrShutdownForced", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for force shutdown")
	}
	if got := output.String(); !strings.Contains(got, "force quit requested — interrupting 0 agent sessions") {
		t.Fatalf("output missing force notice:\n%s", got)
	}
}

func TestRunningShutdownConfigComputesDrainTimeoutFromCurrentRegistry(t *testing.T) {
	t.Parallel()

	registry := projectpkg.NewRegistry()
	cfg := runningShutdownConfig{
		Registry: registry,
		DrainTimeoutSource: func() time.Duration {
			return shutdownDrainTimeout(registry)
		},
	}

	wantDefault := time.Duration(workflowconfig.DefaultShutdownDrainTimeoutMS) * time.Millisecond
	if got := shutdownDrainTimeoutForConfig(cfg); got != wantDefault {
		t.Fatalf("shutdownDrainTimeoutForConfig() = %v, want %v", got, wantDefault)
	}

	project := newShutdownProject(t, "alpha", 0)
	if err := registry.Set(project); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}

	if got := shutdownDrainTimeoutForConfig(cfg); got != 0 {
		t.Fatalf("shutdownDrainTimeoutForConfig() after registry update = %v, want 0", got)
	}
}

func TestShutdownBannerFormatsRunningSessions(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	writeShutdownBanner(&output, []telemetry.Running{
		{
			Issue: telemetry.Issue{
				ID:         "issue-365",
				Identifier: "digitaldrywood/detent#365",
				Title:      "docs(onboarding): tighten setup",
				State:      "In Progress",
			},
			RuntimeSeconds: 724,
		},
	})

	got := output.String()
	for _, want := range []string{
		"shutdown requested — 1 agent session in flight",
		"#365 docs(onboarding)",
		"In Progress",
		"12m 4s",
		"draining: no new work will be dispatched",
		"press Ctrl+C again to force quit",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("banner missing %q:\n%s", want, got)
		}
	}
}

func newShutdownProject(t *testing.T, id string, drainTimeoutMS int) *projectpkg.Project {
	t.Helper()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerMemory
	cfg.Agent.Shutdown.DrainTimeoutMS = drainTimeoutMS
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
