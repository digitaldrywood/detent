package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tui"
	"github.com/digitaldrywood/detent/internal/web"
)

const (
	defaultWorkflowFile = "WORKFLOW.md"
	defaultProjectID    = "default"
	defaultWebHost      = "127.0.0.1"
	defaultWebPort      = 4000
	dashboardHost       = "localhost"
	projectURL          = "https://github.com/digitaldrywood/detent"
)

func resolveBootConfig(ctx context.Context, configPath string, host string, flags runtimeFlags, opts options) (BootConfig, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return BootConfig{}, err
	}
	path := resolution.Path

	cfg, err := opts.read(path)
	if err == nil {
		workflowPath := firstGlobalWorkflowPath(cfg)
		runtime, err := resolveRuntimeSettings(ctx, runtimeInput{
			Config:     &cfg,
			ConfigPath: resolution,
			Workflow:   workflowPath,
			Flags:      flags,
		}, runtimeDepsFromOptions(opts))
		if err != nil {
			return BootConfig{}, err
		}
		resolvedPort := runtime.Port.Value
		return BootConfig{
			Mode:           BootModeRunning,
			Global:         cfg,
			ConfigPathRule: resolution.Rule,
			Runtime:        runtime,
			Host:           bootHost(ctx, host, firstGlobalProject(cfg)),
			Port:           &resolvedPort,
			Version:        opts.version,
			Build:          opts.build,
		}, nil
	}
	if !missingGlobalConfig(err) {
		return BootConfig{}, err
	}

	workflowPath := filepath.Join(mustGetwd(), defaultWorkflowFile)
	if validWorkflowFile(workflowPath) {
		cfg, err := globalConfigFromWorkflow(path, workflowPath)
		if err != nil {
			return BootConfig{}, err
		}
		runtime, err := resolveRuntimeSettings(ctx, runtimeInput{
			Config:     &cfg,
			ConfigPath: resolution,
			Workflow:   workflowPath,
			Flags:      flags,
		}, runtimeDepsFromOptions(opts))
		if err != nil {
			return BootConfig{}, err
		}
		resolvedPort := runtime.Port.Value
		return BootConfig{
			Mode:           BootModeRunning,
			Global:         cfg,
			ConfigPathRule: resolution.Rule,
			Runtime:        runtime,
			WorkflowPath:   workflowPath,
			Host:           bootHost(ctx, host, firstGlobalProject(cfg)),
			Port:           &resolvedPort,
			Version:        opts.version,
			Build:          opts.build,
		}, nil
	}

	cfg, err = globalconfig.DefaultAt(path)
	if err != nil {
		return BootConfig{}, err
	}
	runtime, err := resolveRuntimeSettings(ctx, runtimeInput{
		Config:     &cfg,
		ConfigPath: resolution,
		Workflow:   workflowPath,
		Flags:      flags,
	}, runtimeDepsFromOptions(opts))
	if err != nil {
		return BootConfig{}, err
	}
	resolvedPort := runtime.Port.Value
	return BootConfig{
		Mode:           BootModeOnboarding,
		Global:         cfg,
		ConfigPathRule: resolution.Rule,
		Runtime:        runtime,
		WorkflowPath:   workflowPath,
		Host:           strings.TrimSpace(host),
		Port:           &resolvedPort,
		Version:        opts.version,
		Build:          opts.build,
	}, nil
}

func defaultBoot(ctx context.Context, cfg BootConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}

	switch cfg.Mode {
	case BootModeOnboarding:
		return startOnboarding(ctx, cfg)
	default:
		return startRunning(ctx, cfg)
	}
}

func startRunning(ctx context.Context, cfg BootConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	useDashboard := shouldLaunchTerminalDashboard(cfg)
	if useDashboard {
		restoreLogger, err := redirectDefaultLogger(runtimeLogPath(cfg), cfg.Runtime.LogLevel.Value)
		if err != nil {
			return err
		}
		defer restoreLogger()
	}

	logger := slog.Default()
	if useDashboard {
		logger.Info("resolved global config", "path", cfg.Global.Path, "rule", cfg.ConfigPathRule)
		for _, warning := range cfg.Runtime.Warnings {
			logger.Warn(warning.Detail, "check", warning.Name, "hint", warning.Hint)
		}
	}
	runtimeStore, err := openRuntimeStore(runCtx, cfg)
	if err != nil {
		return err
	}
	if cfg.Isolated != nil && cfg.Isolated.Demo == "screenshots" {
		if err := web.SeedDemoUsageEvents(runCtx, runtimeStore); err != nil {
			return err
		}
	}
	defer func() {
		stop()
		closeStarted := logShutdownBoundaryBegin(logger, "runtime_store_close", "component", "runtime_store")
		if err := runtimeStore.Close(); err != nil {
			logShutdownBoundaryEnd(logger, "runtime_store_close", closeStarted, err, "component", "runtime_store")
			logger.Warn("close runtime store failed", "error", err)
		} else {
			logShutdownBoundaryEnd(logger, "runtime_store_close", closeStarted, nil, "component", "runtime_store")
		}
	}()

	listener, displayURL, err := listenForBoot(cfg)
	if err != nil {
		return err
	}
	listenerOwned := true
	defer func() {
		if listenerOwned {
			if err := listener.Close(); err != nil {
				logger.Warn("close web listener failed", "error", err)
			}
		}
	}()

	events := hub.New[project.Event]()
	globalScheduler, err := buildGlobalScheduler(cfg.Global.Global, runtimeStore)
	if err != nil {
		return err
	}
	globalDispatchGate := scheduler.NewGlobalDispatchGate(globalScheduler)
	runtimeGitHubToken := newRuntimeGitHubTokenState(runtimeGlobalGitHubToken(cfg.Runtime.GitHubToken))
	projectFactory := withRunnerFactory(project.Dependencies{
		Events:             events,
		Logger:             logger,
		GlobalDispatchGate: globalDispatchGate,
		GitHubToken:        runtimeGitHubToken.get(),
		ConnectorFactory:   cfg.ConnectorFactory,
		Runner:             cfg.Runner,
	}, runtimeStore, nil, runtimeGitHubToken.get)
	manager, err := project.NewManager(managerConfigWithRuntimeGitHubToken(cfg.Global, runtimeGitHubToken.get()), project.ManagerDependencies{
		ProjectFactory: projectFactory,
		Events:         events,
		Logger:         logger,
	})
	if err != nil {
		return err
	}
	globalConfigState := newGlobalConfigState(cfg.Global)
	globalWatcherStarted := make(chan (<-chan struct{}), 1)
	defer func() {
		stop()
		select {
		case globalWatcherDone := <-globalWatcherStarted:
			waitStarted := logShutdownBoundaryBegin(logger, "global_config_watcher_wait", "component", "global_config_watcher")
			waitGlobalConfigWatcher(globalWatcherDone)
			logShutdownBoundaryEnd(logger, "global_config_watcher_wait", waitStarted, nil, "component", "global_config_watcher")
		default:
		}
	}()

	snapshotHub := hub.New[telemetry.Snapshot]()
	if err := publishStartupSnapshotOnce(runCtx, cfg.Global, snapshotHub, runtimeStore, displayURL, time.Now()); err != nil {
		return err
	}
	go publishSnapshots(runCtx, manager.Registry(), snapshotHub, runtimeStore, displayURL, defaultSnapshotInterval, time.Now)
	server, err := web.NewServer(web.Config{
		Mode:               web.ModeRunning,
		WorkflowPath:       firstWorkflowPath(cfg),
		Version:            cfg.Version,
		Build:              cfg.Build,
		DashboardURL:       displayURL,
		GlobalConfig:       cfg.Global,
		GlobalConfigSource: globalConfigState.get,
		ConfigPathRule:     cfg.ConfigPathRule,
		RuntimeDBPath:      runtimeStorePath(cfg),
		RuntimeLogPath:     runtimeLogPath(cfg),
		ServerAddress:      listener.Addr().String(),
		Demo: web.DemoConfig{
			Mode:  isolatedDemo(cfg),
			Clock: isolatedDemoClock(cfg),
		},
	}, web.Dependencies{
		Hub:       snapshotHub,
		Store:     runtimeStore,
		Registry:  manager.Registry(),
		Connector: firstConnector(manager),
		Refresher: refresherForRegistry(manager.Registry()),
	})
	if err != nil {
		return err
	}

	startProjects := func(ctx context.Context) error {
		if err := manager.Start(ctx); err != nil {
			return err
		}
		globalWatcherDone := startGlobalConfigWatcher(ctx, cfg.Global, manager, logger, runtimeGitHubToken, globalConfigState.set)
		select {
		case globalWatcherStarted <- globalWatcherDone:
		default:
		}
		return nil
	}

	if useDashboard {
		if err := printBootBanner(cfg, displayURL); err != nil {
			return err
		}
		listenerOwned = false
		if cfg.Shutdown == nil {
			return runStartupAndServe(runCtx, startProjects, func(ctx context.Context) error {
				return serveWithTerminalDashboard(ctx, server, listener, snapshotHub, cfg.Build, nil)
			})
		}
		return runStartupAndServe(runCtx, startProjects, func(ctx context.Context) error {
			return runWithShutdown(ctx, runningShutdownConfig{
				Controller:        cfg.Shutdown,
				Registry:          manager.Registry(),
				SnapshotHub:       snapshotHub,
				LifetimeSource:    runtimeStore,
				DashboardURL:      displayURL,
				Output:            cfg.Output,
				Logger:            logger,
				TerminalDashboard: true,
				DrainTimeoutSource: func() time.Duration {
					return shutdownDrainTimeout(manager.Registry())
				},
				ProgressInterval: defaultShutdownProgressInterval,
				HardTimeout:      defaultShutdownHardTimeout,
			}, func(ctx context.Context) error {
				return serveWithTerminalDashboard(ctx, server, listener, snapshotHub, cfg.Build, func() {
					requestTerminalShutdownInterrupt(cfg.Shutdown, cfg.HardExit)
				})
			})
		})
	}
	if err := printBootBanner(cfg, displayURL); err != nil {
		return err
	}
	listenerOwned = false
	if cfg.Shutdown == nil {
		return runStartupAndServe(runCtx, startProjects, func(ctx context.Context) error {
			return serve(ctx, server, listener)
		})
	}
	return runStartupAndServe(runCtx, startProjects, func(ctx context.Context) error {
		return runWithShutdown(ctx, runningShutdownConfig{
			Controller:     cfg.Shutdown,
			Registry:       manager.Registry(),
			SnapshotHub:    snapshotHub,
			LifetimeSource: runtimeStore,
			DashboardURL:   displayURL,
			Output:         cfg.Output,
			Logger:         logger,
			DrainTimeoutSource: func() time.Duration {
				return shutdownDrainTimeout(manager.Registry())
			},
			ProgressInterval: defaultShutdownProgressInterval,
			HardTimeout:      defaultShutdownHardTimeout,
		}, func(ctx context.Context) error {
			return serve(ctx, server, listener)
		})
	})
}

func startOnboarding(ctx context.Context, cfg BootConfig) error {
	logger := slog.Default()
	listener, displayURL, err := listenForBoot(cfg)
	if err != nil {
		return err
	}
	listenerOwned := true
	defer func() {
		if listenerOwned {
			if err := listener.Close(); err != nil {
				logger.Warn("close web listener failed", "error", err)
			}
		}
	}()

	server, err := web.NewServer(web.Config{
		Mode:         web.ModeOnboarding,
		WorkflowPath: firstWorkflowPath(cfg),
		Version:      cfg.Version,
		Build:        cfg.Build,
		DashboardURL: displayURL,
		GlobalConfig: cfg.Global,
	}, web.Dependencies{})
	if err != nil {
		return err
	}

	if err := printBootBanner(cfg, displayURL); err != nil {
		return err
	}
	listenerOwned = false
	if cfg.Shutdown != nil {
		return runWithShutdown(ctx, runningShutdownConfig{
			Controller:  cfg.Shutdown,
			Output:      cfg.Output,
			Logger:      logger,
			HardTimeout: defaultShutdownHardTimeout,
		}, func(ctx context.Context) error {
			return serve(ctx, server, listener)
		})
	}
	return serve(ctx, server, listener)
}

func serve(ctx context.Context, server *web.Server, listener net.Listener) error {
	logger := slog.Default()
	errs := make(chan error, 1)
	go func() {
		errs <- server.StartListener(listener)
	}()

	select {
	case <-ctx.Done():
		contextStarted := logShutdownBoundaryBegin(logger, "serve_context_done", "component", "serve")
		logShutdownBoundaryEnd(logger, "serve_context_done", contextStarted, nil, "component", "serve")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownStarted := logShutdownBoundaryBegin(logger, "web_server_shutdown", "component", "web_server", "timeout", 5*time.Second)
		if err := server.Shutdown(shutdownCtx); err != nil {
			logShutdownBoundaryEnd(logger, "web_server_shutdown", shutdownStarted, err, "component", "web_server", "timeout", 5*time.Second)
			return err
		}
		logShutdownBoundaryEnd(logger, "web_server_shutdown", shutdownStarted, nil, "component", "web_server", "timeout", 5*time.Second)
		waitStarted := logShutdownBoundaryBegin(logger, "web_server_listener_wait", "component", "web_server")
		err := <-errs
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logShutdownBoundaryEnd(logger, "web_server_listener_wait", waitStarted, err, "component", "web_server")
			return err
		}
		logShutdownBoundaryEnd(logger, "web_server_listener_wait", waitStarted, nil, "component", "web_server")
		return ctx.Err()
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type startupServeResult struct {
	name string
	err  error
}

func runStartupAndServe(
	ctx context.Context,
	startup func(context.Context) error,
	serveApp func(context.Context) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan startupServeResult, 2)
	go func() {
		results <- startupServeResult{name: "startup", err: startup(runCtx)}
	}()
	go func() {
		results <- startupServeResult{name: "serve", err: serveApp(runCtx)}
	}()

	startupDone := false
	for {
		result := <-results
		switch result.name {
		case "startup":
			if result.err != nil {
				cancel()
				serveResult := <-results
				if unexpected := unexpectedBootServeError(serveResult.err); unexpected != nil {
					return errors.Join(result.err, unexpected)
				}
				return result.err
			}
			startupDone = true
		case "serve":
			cancel()
			if !startupDone {
				startupResult := <-results
				if startupResult.err != nil {
					if unexpected := unexpectedBootServeError(result.err); unexpected != nil {
						return errors.Join(unexpected, startupResult.err)
					}
					return startupResult.err
				}
			}
			return result.err
		}
	}
}

func unexpectedBootServeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func serveWithTerminalDashboard(ctx context.Context, server *web.Server, listener net.Listener, snapshots *hub.Hub[telemetry.Snapshot], build buildinfo.Info, interrupt func()) error {
	if ctx == nil {
		ctx = context.Background()
	}
	listenerOwned := true
	defer func() {
		if listenerOwned && listener != nil {
			if err := listener.Close(); err != nil {
				slog.Default().Warn("close web listener failed", "error", err)
			}
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	model, err := tui.NewModel(runCtx, snapshots, tui.WithBuild(build), tui.WithInterruptFunc(interrupt))
	if err != nil {
		return err
	}
	defer model.Close()

	errs := make(chan error, 2)
	listenerOwned = false
	go func() {
		errs <- serve(runCtx, server, listener)
	}()
	go func() {
		errs <- runTerminalDashboardProgram(runCtx, tea.NewProgram(model, terminalDashboardProgramOptions()...))
	}()

	first := <-errs
	cancel()
	second := <-errs
	return terminalDashboardError(first, second)
}

func terminalDashboardError(first error, second error) error {
	if err := unexpectedTerminalDashboardError(first); err != nil {
		return err
	}
	if err := unexpectedTerminalDashboardError(second); err != nil {
		return err
	}
	if first == nil || second == nil {
		return nil
	}
	if errors.Is(first, context.Canceled) || errors.Is(second, context.Canceled) {
		return context.Canceled
	}
	return nil
}

func unexpectedTerminalDashboardError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) || errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

type terminalDashboardProgram interface {
	Run() (tea.Model, error)
	Kill()
}

func terminalDashboardProgramOptions() []tea.ProgramOption {
	return []tea.ProgramOption{
		tea.WithOutput(newTerminalDashboardOutputFilter(os.Stdout)),
		tea.WithoutSignalHandler(),
		tea.WithoutBracketedPaste(),
	}
}

func runTerminalDashboardProgram(ctx context.Context, program terminalDashboardProgram) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if program == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			program.Kill()
		case <-done:
		}
	}()

	_, err := program.Run()
	close(done)
	return err
}

func shouldLaunchTerminalDashboard(cfg BootConfig) bool {
	return cfg.Mode == BootModeRunning && cfg.StdoutTTY && !cfg.Headless
}

func requestTerminalShutdownInterrupt(controller *ShutdownController, hardExit func(int)) bool {
	request, handled := controller.RequestInterruptKind()
	slog.Default().Debug("shutdown interrupt request", "operation", "shutdown_interrupt_request", "source", "terminal_dashboard", "request", request.String(), "handled", handled)
	if !handled {
		return false
	}
	if request == ShutdownRequestForce {
		hardExitProcess(hardExit)
	}
	return true
}

func hardExitProcess(hardExit func(int)) {
	if hardExit == nil {
		hardExit = os.Exit
	}
	hardExit(ExitGeneral)
}

func redirectDefaultLogger(path string, level string) (func(), error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{
		Level: parseSlogLevel(level),
	})))

	return func() {
		slog.SetDefault(previous)
		if err := file.Close(); err != nil {
			previous.Warn("close log file failed", "path", path, "error", err)
		}
	}, nil
}

func missingGlobalConfig(err error) bool {
	var missing globalconfig.MissingFileError
	return errors.As(err, &missing) && errors.Is(missing.Err, os.ErrNotExist)
}

func validWorkflowFile(path string) bool {
	workflow, err := workflowconfig.LoadWorkflow(path)
	if err != nil {
		return false
	}
	return workflow.Config.Validate() == nil
}

func globalConfigFromWorkflow(globalPath string, workflowPath string) (globalconfig.Config, error) {
	cfg, err := globalconfig.DefaultAt(globalPath)
	if err != nil {
		return globalconfig.Config{}, err
	}
	workflow, err := workflowconfig.LoadWorkflow(workflowPath)
	if err != nil {
		return globalconfig.Config{}, err
	}

	cfg.Global.Identity = workflow.Config.Identity
	workdir := filepath.Dir(workflowPath)
	cfg.Projects = []globalconfig.Project{
		{
			ID:       defaultProjectID,
			Workflow: workflowPath,
			Workdir:  workdir,
			Weight:   1,
			Priority: 0,
		},
	}
	return cfg, nil
}

func buildGlobalScheduler(settings globalconfig.Settings, fairShareStore scheduler.FairShareStore) (scheduler.GlobalScheduler, error) {
	halfLife, err := globalFairShareHalfLife(settings.FairShare)
	if err != nil {
		return nil, err
	}

	schedulerConfig := scheduler.Config{
		Kind:          settings.Scheduling,
		Capacity:      settings.MaxConcurrentAgents,
		DecayHalfLife: halfLife,
	}
	if settings.Scheduling == globalconfig.SchedulingFairShare {
		schedulerConfig.FairShareStore = fairShareStore
	}

	sched, err := scheduler.NewFromConfig(schedulerConfig)
	if err != nil {
		return nil, fmt.Errorf("create global scheduler: %w", err)
	}
	global, ok := sched.(scheduler.GlobalScheduler)
	if !ok {
		return nil, fmt.Errorf("create global scheduler: %w", scheduler.ErrUnsupportedBackend)
	}
	return global, nil
}

func globalFairShareHalfLife(settings map[string]any) (time.Duration, error) {
	value, ok := settings["half_life"]
	if !ok || value == nil {
		return 0, nil
	}

	switch halfLife := value.(type) {
	case string:
		text := strings.TrimSpace(halfLife)
		if text == "" {
			return 0, nil
		}
		duration, err := time.ParseDuration(text)
		if err != nil {
			return 0, fmt.Errorf("global.fair_share.half_life: %w", err)
		}
		return duration, nil
	case time.Duration:
		return halfLife, nil
	default:
		return 0, fmt.Errorf("global.fair_share.half_life: must be a duration string")
	}
}

func openRuntimeStore(ctx context.Context, cfg BootConfig) (store.Store, error) {
	return store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    runtimeStorePath(cfg),
	})
}

func runtimeStorePath(cfg BootConfig) string {
	if path := strings.TrimSpace(cfg.RuntimeDBPath); path != "" {
		return path
	}
	path := filepath.Join(filepath.Dir(cfg.Global.Path), "detent.db")
	if strings.TrimSpace(cfg.Global.Path) == "" {
		path = filepath.Join(mustGetwd(), ".detent", "detent.db")
	}
	return path
}

func runtimeLogPath(cfg BootConfig) string {
	if path := strings.TrimSpace(cfg.RuntimeLogPath); path != "" {
		return path
	}
	return filepath.Join(filepath.Dir(runtimeStorePath(cfg)), "detent.log")
}

func parseSlogLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func firstConnector(manager *project.Manager) connector.Connector {
	for _, project := range manager.Registry().List() {
		if projectConnector := project.Connector(); projectConnector != nil {
			return projectConnector
		}
	}
	return memory.New(memory.Config{})
}

func refresherForRegistry(registry *project.Registry) web.Refresher {
	if registry == nil {
		return nil
	}
	return registryRefresher{registry: registry}
}

type registryRefresher struct {
	registry *project.Registry
}

func (r registryRefresher) RequestRefresh(ctx context.Context) (web.RefreshResponse, error) {
	var response web.RefreshResponse
	refreshed := false
	for _, trackedProject := range r.registry.List() {
		if !trackedProject.Running() {
			continue
		}
		orch := trackedProject.Orchestrator()
		if orch == nil {
			continue
		}

		next, err := orch.RequestRefresh(ctx)
		if err != nil {
			return web.RefreshResponse{}, err
		}
		if !refreshed {
			response = next
			refreshed = true
			continue
		}
		response = mergeRefreshResponse(response, next)
	}
	if !refreshed {
		return web.RefreshResponse{}, project.ErrProjectNotFound
	}
	return response, nil
}

func mergeRefreshResponse(current web.RefreshResponse, next web.RefreshResponse) web.RefreshResponse {
	current.Queued = current.Queued || next.Queued
	current.Coalesced = current.Coalesced || next.Coalesced
	if current.RequestedAt.IsZero() || (!next.RequestedAt.IsZero() && next.RequestedAt.Before(current.RequestedAt)) {
		current.RequestedAt = next.RequestedAt
	}
	current.Operations = appendOperations(current.Operations, next.Operations)
	return current
}

func appendOperations(operations []string, next []string) []string {
	for _, operation := range next {
		if !hasOperation(operations, operation) {
			operations = append(operations, operation)
		}
	}
	return operations
}

func hasOperation(operations []string, operation string) bool {
	return slices.Contains(operations, operation)
}

func firstWorkflowPath(cfg BootConfig) string {
	if strings.TrimSpace(cfg.WorkflowPath) != "" {
		return cfg.WorkflowPath
	}
	if len(cfg.Global.Projects) == 0 {
		return filepath.Join(mustGetwd(), defaultWorkflowFile)
	}
	return cfg.Global.Projects[0].Workflow
}

func firstGlobalWorkflowPath(cfg globalconfig.Config) string {
	return firstGlobalProject(cfg).Workflow
}

func firstGlobalProject(cfg globalconfig.Config) globalconfig.Project {
	if len(cfg.Projects) == 0 {
		return globalconfig.Project{}
	}
	return cfg.Projects[0]
}

func bootHost(ctx context.Context, host string, cfg globalconfig.Project) string {
	resolvedHost := strings.TrimSpace(host)
	if resolvedHost != "" {
		return resolvedHost
	}

	if strings.TrimSpace(cfg.Workflow) == "" {
		return ""
	}
	workflow, err := project.LoadWorkflowContext(ctx, cfg)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(workflow.Config.Server.Host)
}

func bootPort(port int) *int {
	if port < 0 {
		return nil
	}
	value := port
	return &value
}

func serverAddr(cfg BootConfig) string {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		host = defaultWebHost
	}
	host = unbracketIPv6Host(host)
	port := defaultWebPort
	if cfg.Port != nil {
		port = *cfg.Port
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func unbracketIPv6Host(host string) string {
	if len(host) < 2 || host[0] != '[' || host[len(host)-1] != ']' {
		return host
	}
	unbracketed := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if net.ParseIP(unbracketed) == nil {
		return host
	}
	return unbracketed
}

func listenForBoot(cfg BootConfig) (net.Listener, string, error) {
	listener, err := net.Listen("tcp", serverAddr(cfg))
	if err != nil {
		return nil, "", err
	}
	return listener, dashboardURL(listener.Addr()), nil
}

func dashboardURL(addr net.Addr) string {
	port := dashboardPort(addr)
	return "http://" + net.JoinHostPort(dashboardHost, strconv.Itoa(port))
}

func dashboardPort(addr net.Addr) int {
	if tcpAddr, ok := addr.(*net.TCPAddr); ok && tcpAddr.Port > 0 {
		return tcpAddr.Port
	}
	if addr == nil {
		return defaultWebPort
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return defaultWebPort
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return defaultWebPort
	}
	return value
}

func printBootBanner(cfg BootConfig, displayURL string) error {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}
	_, err := io.WriteString(out, bootBanner(cfg.Version, displayURL, cfg.Isolated))
	return err
}

func isolatedDemo(cfg BootConfig) string {
	if cfg.Isolated == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Isolated.Demo)
}

func isolatedDemoClock(cfg BootConfig) string {
	if cfg.Isolated == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Isolated.DemoClock)
}

func bootBanner(version string, displayURL string, isolated *IsolatedRuntimeInfo) string {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	displayURL = strings.TrimSpace(displayURL)
	if displayURL == "" {
		displayURL = "http://" + net.JoinHostPort(dashboardHost, strconv.Itoa(defaultWebPort))
	}

	var out strings.Builder
	out.WriteString("Detent ")
	out.WriteString(version)
	out.WriteByte('\n')
	out.WriteString("Project: ")
	out.WriteString(projectURL)
	out.WriteByte('\n')
	out.WriteString("Dashboard: ")
	out.WriteString(displayURL)
	out.WriteByte('\n')
	if isolated != nil {
		out.WriteString("Mode: isolated dev runtime\n")
		writeBootBannerLine(&out, "Home", isolated.Home)
		writeBootBannerLine(&out, "Config", isolated.ConfigPath)
		writeBootBannerLine(&out, "Workflow", isolated.WorkflowPath)
		writeBootBannerLine(&out, "Workspace root", isolated.WorkspaceRoot)
		writeBootBannerLine(&out, "DB", isolated.DBPath)
		writeBootBannerLine(&out, "DB mode", isolated.DBMode)
		writeBootBannerLine(&out, "Tracker", isolated.TrackerMode)
		writeBootBannerLine(&out, "Demo", isolated.Demo)
		writeBootBannerLine(&out, "Demo clock", isolated.DemoClock)
		writeBootBannerLine(&out, "Scenario manifest", isolated.ManifestPath)
		writeBootBannerLine(&out, "Fixture", isolated.FixturePath)
	}
	return out.String()
}

func writeBootBannerLine(out *strings.Builder, label string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	out.WriteString(label)
	out.WriteString(": ")
	out.WriteString(value)
	out.WriteByte('\n')
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
