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
	requests                chan ShutdownRequest
	active                  atomic.Bool
	shutdownRequested       atomic.Bool
	signalNoticesSuppressed atomic.Bool
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

func (c *ShutdownController) SignalNoticesSuppressed() bool {
	return c != nil && c.signalNoticesSuppressed.Load()
}

func (c *ShutdownController) RequestInterrupt() bool {
	_, handled := c.RequestInterruptKind()
	return handled
}

func (c *ShutdownController) RequestInterruptKind() (ShutdownRequest, bool) {
	if c == nil || !c.Active() {
		return 0, false
	}
	if c.shutdownRequested.CompareAndSwap(false, true) {
		c.request(ShutdownRequestDrain)
		return ShutdownRequestDrain, true
	}
	c.request(ShutdownRequestForce)
	return ShutdownRequestForce, true
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

func (c *ShutdownController) suppressSignalNotices(suppress bool) func() {
	if c == nil {
		return func() {}
	}
	previous := c.signalNoticesSuppressed.Load()
	c.signalNoticesSuppressed.Store(suppress)
	return func() {
		c.signalNoticesSuppressed.Store(previous)
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
	Controller         *ShutdownController
	Registry           *project.Registry
	SnapshotHub        *hub.Hub[telemetry.Snapshot]
	LifetimeSource     lifetimeTotalsSource
	DashboardURL       string
	Output             io.Writer
	Logger             *slog.Logger
	TerminalDashboard  bool
	DrainTimeout       time.Duration
	DrainTimeoutSource func() time.Duration
	ProgressInterval   time.Duration
	HardTimeout        time.Duration
	Now                func() time.Time
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
	restoreSignalNotices := cfg.Controller.suppressSignalNotices(cfg.TerminalDashboard)
	defer restoreSignalNotices()

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
			if ctx.Err() != nil && unexpectedShutdownServeError(err) == nil {
				return ctx.Err()
			}
			return unexpectedShutdownServeError(err)
		case <-ctx.Done():
			cancelServe()
			if err := loggedWaitForServeExit(context.Background(), cfg, serveErrs); err != nil {
				return err
			}
			return ctx.Err()
		case request := <-cfg.Controller.Requests():
			shutdownLogger(cfg).Debug("shutdown request received", "operation", "shutdown_request", "request", request.String())
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
	drainTimeout := shutdownDrainTimeoutForConfig(cfg)
	inventoryStarted := logShutdownBoundaryBegin(shutdownLogger(cfg), "initial_running_session_inventory")
	sessions := shutdownRunningSessions(ctx, cfg.Registry, startedAt)
	logShutdownBoundaryEnd(shutdownLogger(cfg), "initial_running_session_inventory", inventoryStarted, nil, "sessions", len(sessions))
	logShutdownDrainBlockers(cfg, "initial", sessions, drainTimeout)
	writeShutdownBanner(shutdownOutput(cfg), sessions)
	shutdownLogger(cfg).Info("shutdown requested", "sessions", len(sessions))

	hardTimeout := cfg.HardTimeout
	if hardTimeout <= 0 {
		hardTimeout = defaultShutdownHardTimeout
	}
	drainCtx, cancelDrain := context.WithTimeout(ctx, hardTimeout)
	if err := runLoggedShutdownStep(drainCtx, cfg, "drain_projects", func(stepCtx context.Context) error {
		return drainProjects(stepCtx, cfg.Registry, shutdownLogger(cfg))
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
	if drainTimeout > 0 {
		timeoutTimer = time.NewTimer(drainTimeout)
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
			if err := loggedWaitForServeExit(context.Background(), cfg, serveErrs); err != nil {
				return err
			}
			return ctx.Err()
		case request := <-cfg.Controller.Requests():
			shutdownLogger(cfg).Debug("shutdown request received", "operation", "shutdown_request", "request", request.String())
			if request == ShutdownRequestForce {
				machine = machine.Apply(shutdownstate.EventForceRequested)
				return runForceShutdown(ctx, cfg, cancelServe, serveErrs, machine, "forced")
			}
		case <-poll.C:
		case <-progress.C:
			logShutdownDrainBlockers(cfg, "progress", sessions, drainTimeout)
			writeShutdownProgress(shutdownOutput(cfg), sessions)
		case <-timeout:
			machine = machine.Apply(shutdownstate.EventDrainTimedOut)
			logShutdownDrainTimeout(cfg, sessions, drainTimeout)
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
	inventoryStarted := logShutdownBoundaryBegin(shutdownLogger(cfg), "force_running_session_inventory")
	sessions := shutdownRunningSessions(ctx, cfg.Registry, startedAt)
	logShutdownBoundaryEnd(shutdownLogger(cfg), "force_running_session_inventory", inventoryStarted, nil, "sessions", len(sessions))
	if result == "drain timeout" {
		fmt.Fprintf(shutdownOutput(cfg), "drain timeout reached — interrupting %d %s\n", len(sessions), shutdownSessionNoun(len(sessions)))
	} else {
		fmt.Fprintf(shutdownOutput(cfg), "force quit requested — interrupting %d %s\n", len(sessions), shutdownSessionNoun(len(sessions)))
	}

	forceCtx, cancel := context.WithTimeout(context.Background(), hardTimeout)
	defer cancel()
	if err := runLoggedShutdownStep(forceCtx, cfg, "force_projects", func(stepCtx context.Context) error {
		return forceProjects(stepCtx, cfg.Registry, shutdownLogger(cfg))
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
	if err := runLoggedShutdownStep(stopCtx, cfg, "stop_projects", func(stepCtx context.Context) error {
		return stopProjects(stepCtx, cfg.Registry, shutdownLogger(cfg))
	}); err != nil {
		shutdownLogger(cfg).Warn("stop projects during shutdown failed", "error", err)
	}

	cancelStarted := logShutdownBoundaryBegin(shutdownLogger(cfg), "serve_cancel", "component", "serve")
	cancelServe()
	logShutdownBoundaryEnd(shutdownLogger(cfg), "serve_cancel", cancelStarted, nil, "component", "serve")
	if err := loggedWaitForServeExit(stopCtx, cfg, serveErrs); err != nil {
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

func runLoggedShutdownStep(ctx context.Context, cfg runningShutdownConfig, operation string, step func(context.Context) error, attrs ...any) error {
	started := logShutdownBoundaryBegin(shutdownLogger(cfg), operation, attrs...)
	err := runShutdownStep(ctx, step)
	logShutdownBoundaryEnd(shutdownLogger(cfg), operation, started, err, attrs...)
	return err
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

func drainProjects(ctx context.Context, registry *project.Registry, logger *slog.Logger) error {
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
		operationStarted := logShutdownBoundaryBegin(logger, "orchestrator_drain", "component", "orchestrator", "project_id", trackedProject.ID())
		err := orch.Drain(ctx)
		if errors.Is(err, orchestrator.ErrStopped) {
			logShutdownBoundaryEndResult(logger, "orchestrator_drain", operationStarted, "stopped", nil, "component", "orchestrator", "project_id", trackedProject.ID())
			continue
		}
		logShutdownBoundaryEnd(logger, "orchestrator_drain", operationStarted, err, "component", "orchestrator", "project_id", trackedProject.ID())
		if err != nil {
			return err
		}
	}
	return nil
}

func forceProjects(ctx context.Context, registry *project.Registry, logger *slog.Logger) error {
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
		operationStarted := logShutdownBoundaryBegin(logger, "orchestrator_force_quit", "component", "orchestrator", "project_id", trackedProject.ID())
		err := orch.ForceQuit(ctx)
		if errors.Is(err, orchestrator.ErrStopped) {
			logShutdownBoundaryEndResult(logger, "orchestrator_force_quit", operationStarted, "stopped", nil, "component", "orchestrator", "project_id", trackedProject.ID())
			continue
		}
		logShutdownBoundaryEnd(logger, "orchestrator_force_quit", operationStarted, err, "component", "orchestrator", "project_id", trackedProject.ID())
		if err != nil {
			return err
		}
	}
	return nil
}

func stopProjects(ctx context.Context, registry *project.Registry, logger *slog.Logger) error {
	if registry == nil {
		return nil
	}
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		operationStarted := logShutdownBoundaryBegin(logger, "project_stop", "component", "project", "project_id", trackedProject.ID())
		err := trackedProject.Stop(ctx)
		if errors.Is(err, project.ErrNotRunning) {
			logShutdownBoundaryEndResult(logger, "project_stop", operationStarted, "not_running", nil, "component", "project", "project_id", trackedProject.ID())
			continue
		}
		logShutdownBoundaryEnd(logger, "project_stop", operationStarted, err, "component", "project", "project_id", trackedProject.ID())
		if err != nil {
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

func shutdownDrainTimeoutForConfig(cfg runningShutdownConfig) time.Duration {
	timeout := cfg.DrainTimeout
	if cfg.DrainTimeoutSource != nil {
		timeout = cfg.DrainTimeoutSource()
	}
	if timeout <= 0 {
		return defaultShutdownDrainTimeout()
	}
	return timeout
}

func publishShutdownSnapshot(ctx context.Context, cfg runningShutdownConfig, now time.Time) {
	started := logShutdownBoundaryBegin(shutdownLogger(cfg), "shutdown_snapshot_publish")
	if cfg.Registry == nil || cfg.SnapshotHub == nil {
		logShutdownBoundaryEndResult(shutdownLogger(cfg), "shutdown_snapshot_publish", started, "skipped", nil)
		return
	}
	if err := publishSnapshotOnce(ctx, cfg.Registry, cfg.SnapshotHub, now, nil, cfg.LifetimeSource, cfg.DashboardURL); err != nil {
		logShutdownBoundaryEnd(shutdownLogger(cfg), "shutdown_snapshot_publish", started, err)
		shutdownLogger(cfg).Warn("publish shutdown telemetry snapshot failed", "error", err)
		return
	}
	logShutdownBoundaryEnd(shutdownLogger(cfg), "shutdown_snapshot_publish", started, nil)
}

func shutdownDrainTimeout(registry *project.Registry) time.Duration {
	timeoutMS := workflowconfig.DefaultShutdownDrainTimeoutMS
	if registry == nil {
		return defaultShutdownDrainTimeout()
	}

	found := false
	for _, trackedProject := range registry.List() {
		workflow := trackedProject.Workflow()
		next := workflow.Config.Agent.Shutdown.DrainTimeoutMS
		if next <= 0 {
			next = workflowconfig.DefaultShutdownDrainTimeoutMS
		}
		if !found || next > timeoutMS {
			timeoutMS = next
		}
		found = true
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func defaultShutdownDrainTimeout() time.Duration {
	return time.Duration(workflowconfig.DefaultShutdownDrainTimeoutMS) * time.Millisecond
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
		details := shutdownSessionDetail(session)
		if details == "" {
			fmt.Fprintf(out, "  %-30s %-14s %8s\n", shutdownIssueLabel(session), defaultShutdownString(session.State, "running"), formatShutdownDuration(session.RuntimeSeconds))
			continue
		}
		fmt.Fprintf(out, "  %-30s %-14s %8s  %s\n", shutdownIssueLabel(session), defaultShutdownString(session.State, "running"), formatShutdownDuration(session.RuntimeSeconds), details)
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
		details := shutdownSessionDetail(session)
		if details == "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", shutdownIssueNumber(session), formatShutdownDuration(session.RuntimeSeconds)))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", shutdownIssueNumber(session), formatShutdownDuration(session.RuntimeSeconds), details))
	}
	fmt.Fprintf(out, "%d %s remaining — %s\n", len(sessions), shutdownSessionNoun(len(sessions)), strings.Join(parts, ", "))
}

func logShutdownDrainBlockers(cfg runningShutdownConfig, phase string, sessions []telemetry.Running, timeout time.Duration) {
	args := shutdownDrainBlockerLogArgs("shutdown_drain_blockers", phase, sessions, timeout)
	shutdownLogger(cfg).Info("shutdown drain blockers", args...)
}

func logShutdownDrainTimeout(cfg runningShutdownConfig, sessions []telemetry.Running, timeout time.Duration) {
	args := shutdownDrainBlockerLogArgs("shutdown_drain_timeout", "timeout", sessions, timeout)
	shutdownLogger(cfg).Warn("shutdown drain timeout reached", args...)
}

func shutdownDrainBlockerLogArgs(operation string, phase string, sessions []telemetry.Running, timeout time.Duration) []any {
	args := []any{
		"operation", operation,
		"phase", phase,
		"blockers", len(sessions),
	}
	if timeout > 0 {
		args = append(args, "timeout", timeout)
	}
	if len(sessions) > 0 {
		args = append(args, "details", strings.Join(shutdownSessionSummaries(sessions), "; "))
	}
	return args
}

func shutdownSessionSummaries(sessions []telemetry.Running) []string {
	summaries := make([]string, 0, len(sessions))
	for _, session := range sessions {
		summaries = append(summaries, shutdownSessionSummary(session))
	}
	return summaries
}

func shutdownSessionSummary(session telemetry.Running) string {
	parts := []string{defaultShutdownString(session.Identifier, session.ID)}
	if title := strings.TrimSpace(session.Title); title != "" {
		parts = append(parts, "title="+title)
	}
	if state := strings.TrimSpace(session.State); state != "" {
		parts = append(parts, "state="+state)
	}
	if details := shutdownSessionDetail(session); details != "" {
		parts = append(parts, details)
	}
	return strings.Join(parts, " ")
}

func shutdownSessionDetail(session telemetry.Running) string {
	parts := make([]string, 0, 3)
	if sessionID := strings.TrimSpace(session.SessionID); sessionID != "" {
		parts = append(parts, "session="+sessionID)
	}
	if process := strings.TrimSpace(session.ProcessIdentity); process != "" {
		parts = append(parts, "process="+process)
	}
	if worker := strings.TrimSpace(session.WorkerHost); worker != "" {
		parts = append(parts, "worker="+worker)
	}
	return strings.Join(parts, " ")
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
	if cfg.TerminalDashboard {
		return io.Discard
	}
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

func loggedWaitForServeExit(ctx context.Context, cfg runningShutdownConfig, serveErrs <-chan error) error {
	started := logShutdownBoundaryBegin(shutdownLogger(cfg), "wait_for_serve_exit", "component", "serve")
	err := waitForServeExit(ctx, serveErrs)
	logShutdownBoundaryEnd(shutdownLogger(cfg), "wait_for_serve_exit", started, err, "component", "serve")
	return err
}

func unexpectedShutdownServeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (request ShutdownRequest) String() string {
	switch request {
	case ShutdownRequestDrain:
		return "drain"
	case ShutdownRequestForce:
		return "force"
	default:
		return "unknown"
	}
}

func logShutdownBoundaryBegin(logger *slog.Logger, operation string, attrs ...any) time.Time {
	if logger == nil {
		logger = slog.Default()
	}
	started := time.Now()
	args := shutdownLogArgs(operation, append([]any{"phase", "begin"}, attrs...)...)
	logger.Debug("shutdown boundary begin", args...)
	return started
}

func logShutdownBoundaryEnd(logger *slog.Logger, operation string, started time.Time, err error, attrs ...any) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	logShutdownBoundaryEndResult(logger, operation, started, result, err, attrs...)
}

func logShutdownBoundaryEndResult(logger *slog.Logger, operation string, started time.Time, result string, err error, attrs ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	if result == "" {
		result = "ok"
	}
	args := shutdownLogArgs(operation,
		"phase", "end",
		"duration", time.Since(started),
		"result", result,
	)
	if err != nil {
		args = append(args, "error", err)
	}
	args = append(args, attrs...)
	logger.Debug("shutdown boundary end", args...)
}

func shutdownLogArgs(operation string, attrs ...any) []any {
	args := make([]any, 0, 2+len(attrs))
	args = append(args, "operation", operation)
	args = append(args, attrs...)
	return args
}
