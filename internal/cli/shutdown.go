package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	shutdownstate "github.com/digitaldrywood/detent/internal/shutdown"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var (
	ErrShutdownForced  = errors.New("shutdown forced")
	ErrShutdownTimeout = errors.New("shutdown drain timeout")
)

const (
	defaultShutdownProgressInterval = 15 * time.Second
	defaultShutdownHardTimeout      = 5 * time.Second
	shutdownDrainPollInterval       = 500 * time.Millisecond
)

type ShutdownRequest int

const (
	ShutdownRequestDrain ShutdownRequest = iota + 1
	ShutdownRequestForce
)

type ShutdownController struct {
	requests          chan ShutdownRequest
	active            atomic.Bool
	shutdownRequested atomic.Bool
}

func NewShutdownController() *ShutdownController {
	return &ShutdownController{requests: make(chan ShutdownRequest, 4)}
}

func (c *ShutdownController) RequestDrain() {
	if c != nil {
		c.shutdownRequested.Store(true)
	}
	c.request(ShutdownRequestDrain)
}

func (c *ShutdownController) RequestForce() {
	if c != nil {
		c.shutdownRequested.Store(true)
	}
	c.request(ShutdownRequestForce)
}

func (c *ShutdownController) Requests() <-chan ShutdownRequest {
	if c == nil {
		return nil
	}
	return c.requests
}

func (c *ShutdownController) Active() bool {
	return c != nil && c.active.Load()
}

func (c *ShutdownController) RequestInterrupt() bool {
	if c == nil || !c.Active() {
		return false
	}
	if c.shutdownRequested.CompareAndSwap(false, true) {
		c.request(ShutdownRequestDrain)
		return true
	}
	c.request(ShutdownRequestForce)
	return true
}

func (c *ShutdownController) activate() func() {
	if c == nil {
		return func() {}
	}
	c.shutdownRequested.Store(false)
	c.active.Store(true)
	return func() {
		c.active.Store(false)
		c.shutdownRequested.Store(false)
	}
}

func (c *ShutdownController) request(request ShutdownRequest) {
	if c == nil {
		return
	}
	select {
	case c.requests <- request:
	default:
	}
}

type shutdownServeFunc func(context.Context) error

type runningShutdownConfig struct {
	Controller       *ShutdownController
	Registry         *project.Registry
	SnapshotHub      *hub.Hub[telemetry.Snapshot]
	LifetimeSource   lifetimeTotalsSource
	DashboardURL     string
	Output           io.Writer
	Logger           *slog.Logger
	DrainTimeout     time.Duration
	ProgressInterval time.Duration
	HardTimeout      time.Duration
	Now              func() time.Time
}

func runWithShutdown(ctx context.Context, cfg runningShutdownConfig, serve shutdownServeFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if serve == nil {
		serve = func(context.Context) error { return nil }
	}
	if cfg.Controller == nil {
		return serve(ctx)
	}
	deactivate := cfg.Controller.activate()
	defer deactivate()

	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	serveErrs := make(chan error, 1)
	go func() {
		serveErrs <- serve(serveCtx)
	}()

	machine := shutdownstate.NewMachine()
	for {
		select {
		case err := <-serveErrs:
			return unexpectedShutdownServeError(err)
		case <-ctx.Done():
			cancelServe()
			if err := waitForServeExit(context.Background(), serveErrs); err != nil {
				return err
			}
			return ctx.Err()
		case request := <-cfg.Controller.Requests():
			switch request {
			case ShutdownRequestDrain:
				machine = machine.Apply(shutdownstate.EventDrainRequested)
				return runDrainShutdown(ctx, cfg, cancelServe, serveErrs, machine)
			case ShutdownRequestForce:
				machine = machine.Apply(shutdownstate.EventForceRequested)
				return runForceShutdown(ctx, cfg, cancelServe, serveErrs, machine, "forced")
			}
		}
	}
}

func runDrainShutdown(
	ctx context.Context,
	cfg runningShutdownConfig,
	cancelServe context.CancelFunc,
	serveErrs <-chan error,
	machine shutdownstate.Machine,
) error {
	startedAt := shutdownNow(cfg)
	sessions := shutdownRunningSessions(ctx, cfg.Registry, startedAt)
	writeShutdownBanner(shutdownOutput(cfg), sessions)
	shutdownLogger(cfg).Info("shutdown requested", "sessions", len(sessions))

	hardTimeout := cfg.HardTimeout
	if hardTimeout <= 0 {
		hardTimeout = defaultShutdownHardTimeout
	}
	drainCtx, cancelDrain := context.WithTimeout(ctx, hardTimeout)
	if err := runShutdownStep(drainCtx, func(stepCtx context.Context) error {
		return drainProjects(stepCtx, cfg.Registry)
	}); err != nil {
		shutdownLogger(cfg).Warn("drain projects during shutdown failed", "error", err)
	}
	cancelDrain()
	publishShutdownSnapshot(ctx, cfg, startedAt)

	if len(sessions) == 0 {
		machine = machine.Apply(shutdownstate.EventDrained)
		return completeShutdown(ctx, cfg, cancelServe, serveErrs, startedAt, machine, nil)
	}

	progressInterval := cfg.ProgressInterval
	if progressInterval <= 0 {
		progressInterval = defaultShutdownProgressInterval
	}
	poll := time.NewTicker(shutdownDrainPollInterval)
	defer poll.Stop()
	progress := time.NewTicker(progressInterval)
	defer progress.Stop()

	var timeout <-chan time.Time
	var timeoutTimer *time.Timer
	if cfg.DrainTimeout > 0 {
		timeoutTimer = time.NewTimer(cfg.DrainTimeout)
		timeout = timeoutTimer.C
		defer timeoutTimer.Stop()
	}

	for {
		sessions = shutdownRunningSessions(ctx, cfg.Registry, shutdownNow(cfg))
		if len(sessions) == 0 {
			machine = machine.Apply(shutdownstate.EventDrained)
			return completeShutdown(ctx, cfg, cancelServe, serveErrs, startedAt, machine, nil)
		}

		select {
		case err := <-serveErrs:
			return unexpectedShutdownServeError(err)
		case <-ctx.Done():
			cancelServe()
			if err := waitForServeExit(context.Background(), serveErrs); err != nil {
				return err
			}
			return ctx.Err()
		case request := <-cfg.Controller.Requests():
			if request == ShutdownRequestForce {
				machine = machine.Apply(shutdownstate.EventForceRequested)
				return runForceShutdown(ctx, cfg, cancelServe, serveErrs, machine, "forced")
			}
		case <-poll.C:
		case <-progress.C:
			writeShutdownProgress(shutdownOutput(cfg), sessions)
		case <-timeout:
			machine = machine.Apply(shutdownstate.EventDrainTimedOut)
			return runForceShutdownWithDeadline(ctx, cfg, cancelServe, serveErrs, machine, "drain timeout", ErrShutdownTimeout, hardTimeout)
		}
	}
}

func runForceShutdown(
	ctx context.Context,
	cfg runningShutdownConfig,
	cancelServe context.CancelFunc,
	serveErrs <-chan error,
	machine shutdownstate.Machine,
	result string,
) error {
	hardTimeout := cfg.HardTimeout
	if hardTimeout <= 0 {
		hardTimeout = defaultShutdownHardTimeout
	}
	return runForceShutdownWithDeadline(ctx, cfg, cancelServe, serveErrs, machine, result, ErrShutdownForced, hardTimeout)
}

func runForceShutdownWithDeadline(
	ctx context.Context,
	cfg runningShutdownConfig,
	cancelServe context.CancelFunc,
	serveErrs <-chan error,
	machine shutdownstate.Machine,
	result string,
	returnErr error,
	hardTimeout time.Duration,
) error {
	startedAt := shutdownNow(cfg)
	sessions := shutdownRunningSessions(ctx, cfg.Registry, startedAt)
	if result == "drain timeout" {
		fmt.Fprintf(shutdownOutput(cfg), "drain timeout reached — interrupting %d %s\n", len(sessions), shutdownSessionNoun(len(sessions)))
	} else {
		fmt.Fprintf(shutdownOutput(cfg), "force quit requested — interrupting %d %s\n", len(sessions), shutdownSessionNoun(len(sessions)))
	}

	forceCtx, cancel := context.WithTimeout(context.Background(), hardTimeout)
	defer cancel()
	if err := runShutdownStep(forceCtx, func(stepCtx context.Context) error {
		return forceProjects(stepCtx, cfg.Registry)
	}); err != nil {
		shutdownLogger(cfg).Warn("force shutdown cleanup failed", "error", err)
	}
	publishShutdownSnapshot(forceCtx, cfg, startedAt)

	return completeShutdown(forceCtx, cfg, cancelServe, serveErrs, startedAt, machine, returnErr)
}

func completeShutdown(
	ctx context.Context,
	cfg runningShutdownConfig,
	cancelServe context.CancelFunc,
	serveErrs <-chan error,
	startedAt time.Time,
	machine shutdownstate.Machine,
	returnErr error,
) error {
	hardTimeout := cfg.HardTimeout
	if hardTimeout <= 0 {
		hardTimeout = defaultShutdownHardTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopCtx, cancel := context.WithTimeout(ctx, hardTimeout)
	defer cancel()
	if err := runShutdownStep(stopCtx, func(stepCtx context.Context) error {
		return stopProjects(stepCtx, cfg.Registry)
	}); err != nil {
		shutdownLogger(cfg).Warn("stop projects during shutdown failed", "error", err)
	}

	cancelServe()
	if err := waitForServeExit(stopCtx, serveErrs); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			shutdownLogger(cfg).Warn("serve did not exit before shutdown deadline", "error", err)
			return returnErr
		}
		return err
	}

	result := shutdownResultLabel(machine)
	shutdownLogger(cfg).Info("shutdown complete ("+result+")", "duration", shutdownNow(cfg).Sub(startedAt))
	return returnErr
}

func runShutdownStep(ctx context.Context, step func(context.Context) error) error {
	if step == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- step(ctx)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func shutdownResultLabel(machine shutdownstate.Machine) string {
	switch machine.Result {
	case shutdownstate.ResultForced:
		return "forced"
	case shutdownstate.ResultTimeout:
		return "drain timeout"
	default:
		return "graceful"
	}
}

func drainProjects(ctx context.Context, registry *project.Registry) error {
	if registry == nil {
		return nil
	}
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		orch := trackedProject.Orchestrator()
		if orch == nil {
			continue
		}
		if err := orch.Drain(ctx); err != nil && !errors.Is(err, orchestrator.ErrStopped) {
			return err
		}
	}
	return nil
}

func forceProjects(ctx context.Context, registry *project.Registry) error {
	if registry == nil {
		return nil
	}
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		orch := trackedProject.Orchestrator()
		if orch == nil {
			continue
		}
		if err := orch.ForceQuit(ctx); err != nil && !errors.Is(err, orchestrator.ErrStopped) {
			return err
		}
	}
	return nil
}

func stopProjects(ctx context.Context, registry *project.Registry) error {
	if registry == nil {
		return nil
	}
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		if err := trackedProject.Stop(ctx); err != nil && !errors.Is(err, project.ErrNotRunning) {
			return err
		}
	}
	return nil
}

func shutdownRunningSessions(ctx context.Context, registry *project.Registry, now time.Time) []telemetry.Running {
	if registry == nil {
		return nil
	}

	var sessions []telemetry.Running
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		orch := trackedProject.Orchestrator()
		if orch == nil {
			continue
		}
		state, err := orch.State(ctx)
		if err != nil {
			continue
		}
		sessions = append(sessions, state.Snapshot(now).Running...)
	}
	sort.Slice(sessions, func(i int, j int) bool {
		return shutdownIssueLabel(sessions[i]) < shutdownIssueLabel(sessions[j])
	})
	return sessions
}

func publishShutdownSnapshot(ctx context.Context, cfg runningShutdownConfig, now time.Time) {
	if cfg.Registry == nil || cfg.SnapshotHub == nil {
		return
	}
	if err := publishSnapshotOnce(ctx, cfg.Registry, cfg.SnapshotHub, now, nil, cfg.LifetimeSource, cfg.DashboardURL); err != nil {
		shutdownLogger(cfg).Warn("publish shutdown telemetry snapshot failed", "error", err)
	}
}

func shutdownDrainTimeout(registry *project.Registry) time.Duration {
	timeoutMS := workflowconfig.DefaultShutdownDrainTimeoutMS
	if registry == nil {
		return time.Duration(timeoutMS) * time.Millisecond
	}

	found := false
	for _, trackedProject := range registry.List() {
		workflow := trackedProject.Workflow()
		next := workflow.Config.Agent.Shutdown.DrainTimeoutMS
		if next == 0 {
			return 0
		}
		if !found || next > timeoutMS {
			timeoutMS = next
		}
		found = true
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func writeShutdownBanner(out io.Writer, sessions []telemetry.Running) {
	if out == nil {
		out = io.Discard
	}
	count := len(sessions)
	if count == 0 {
		fmt.Fprintln(out, "shutdown requested — no agent sessions in flight")
		return
	}

	fmt.Fprintf(out, "shutdown requested — %d %s in flight\n", count, shutdownSessionNoun(count))
	for _, session := range sessions {
		fmt.Fprintf(out, "  %-30s %-14s %8s\n", shutdownIssueLabel(session), defaultShutdownString(session.State, "running"), formatShutdownDuration(session.RuntimeSeconds))
	}
	fmt.Fprintln(out, "draining: no new work will be dispatched; waiting for running sessions to finish")
	fmt.Fprintln(out, "press Ctrl+C again to force quit (sessions will be interrupted and issues re-queued)")
}

func writeShutdownProgress(out io.Writer, sessions []telemetry.Running) {
	if out == nil || len(sessions) == 0 {
		return
	}
	parts := make([]string, 0, len(sessions))
	for _, session := range sessions {
		parts = append(parts, fmt.Sprintf("%s (%s)", shutdownIssueNumber(session), formatShutdownDuration(session.RuntimeSeconds)))
	}
	fmt.Fprintf(out, "%d %s remaining — %s\n", len(sessions), shutdownSessionNoun(len(sessions)), strings.Join(parts, ", "))
}

func shutdownIssueLabel(session telemetry.Running) string {
	issue := shutdownIssueNumber(session)
	title := strings.TrimSpace(session.Title)
	if title == "" {
		return issue
	}
	return truncateShutdownText(issue+" "+title, 30)
}

func shutdownIssueNumber(session telemetry.Running) string {
	identifier := strings.TrimSpace(session.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(session.ID)
	}
	if index := strings.LastIndex(identifier, "#"); index >= 0 {
		return identifier[index:]
	}
	return identifier
}

func shutdownSessionNoun(count int) string {
	if count == 1 {
		return "agent session"
	}
	return "agent sessions"
}

func truncateShutdownText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func formatShutdownDuration(seconds float64) string {
	duration := time.Duration(seconds * float64(time.Second)).Round(time.Second)
	if duration < time.Second {
		return "0s"
	}
	hours := int(duration / time.Hour)
	duration -= time.Duration(hours) * time.Hour
	minutes := int(duration / time.Minute)
	duration -= time.Duration(minutes) * time.Minute
	secs := int(duration / time.Second)
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

func defaultShutdownString(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func shutdownOutput(cfg runningShutdownConfig) io.Writer {
	if cfg.Output == nil {
		return io.Discard
	}
	return cfg.Output
}

func shutdownLogger(cfg runningShutdownConfig) *slog.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return slog.Default()
}

func shutdownNow(cfg runningShutdownConfig) time.Time {
	if cfg.Now != nil {
		return cfg.Now()
	}
	return time.Now()
}

func waitForServeExit(ctx context.Context, serveErrs <-chan error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case err := <-serveErrs:
		return unexpectedShutdownServeError(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unexpectedShutdownServeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
