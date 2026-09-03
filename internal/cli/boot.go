package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/digitaldrywood/detent/internal/activehours"
	"github.com/digitaldrywood/detent/internal/activity"
	"github.com/digitaldrywood/detent/internal/boardsnapshot"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/healthnotify"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/notify"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/project"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/statuspage"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tmuxstatus"
	"github.com/digitaldrywood/detent/internal/tui"
	"github.com/digitaldrywood/detent/internal/web"
	"github.com/digitaldrywood/detent/internal/web/demofixtures"
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

type startRunningDependencies struct {
	boardSnapshotStore    boardsnapshot.Store
	boardSnapshotInterval time.Duration
	backgroundWaitStarted func()
	buildDriftInterval    time.Duration
	readInstalledBuild    installedBuildReader
	managerDependencies   project.ManagerDependencies
	providerStatusManager *statuspage.Manager
}

func startRunning(ctx context.Context, cfg BootConfig) error {
	return startRunningWithDependencies(ctx, cfg, startRunningDependencies{})
}

func startRunningWithDependencies(ctx context.Context, cfg BootConfig, deps startRunningDependencies) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	useDashboard := shouldLaunchTerminalDashboard(cfg)
	if useDashboard {
		restoreLogger, err := redirectDefaultLoggerWithRotation(runtimeLogPath(cfg), cfg.Runtime.LogLevel.Value, runtimeLogRotation(cfg.Runtime), cfg.LogLevel)
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
	var instanceLock *instancelock.Lock
	runtimeDBPath := runtimeStorePath(cfg)
	if !runtimeStoreIsMemory(runtimeDBPath) {
		acquiredLock, err := acquireRuntimeInstanceLock(runtimeStoreLockPath(runtimeDBPath))
		if err != nil {
			return err
		}
		instanceLock = acquiredLock
		if recovery, recovered := instanceLock.Recovery(); recovered {
			attrs := []any{"path", runtimeStoreLockPath(runtimeDBPath)}
			if recovery.Owner.PID > 0 {
				attrs = append(attrs, "pid", recovery.Owner.PID)
			}
			if recovery.Owner.Hostname != "" {
				attrs = append(attrs, "hostname", recovery.Owner.Hostname)
			}
			if !recovery.Owner.StartedAt.IsZero() {
				attrs = append(attrs, "started_at", recovery.Owner.StartedAt)
			}
			if recovery.MetadataError != nil {
				attrs = append(attrs, "metadata_error", recovery.MetadataError)
			}
			logger.Info("self-healed stale runtime instance lock", attrs...)
		}
	}
	if instanceLock != nil {
		defer func() {
			if err := instanceLock.Close(); err != nil {
				logger.Warn("release runtime instance lock failed", "error", err)
			}
		}()
	}

	listener, displayURL, err := listenForBoot(runCtx, cfg)
	if err != nil {
		return fmt.Errorf("bind Detent web listener %s: %w", serverAddr(cfg), err)
	}
	listenerOwned := true
	defer func() {
		if listenerOwned {
			if err := listener.Close(); err != nil {
				logger.Warn("close web listener failed", "error", err)
			}
		}
	}()

	runtimeStore, err := openRuntimeStore(runCtx, cfg)
	if err != nil {
		return err
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
	if err := reapWorkerProcesses(runCtx, runtimeStore, logger, "startup", procgroup.DefaultTerminationGrace, time.Now, nil); err != nil {
		return fmt.Errorf("reap worker processes from prior instance: %w", err)
	}
	if cfg.Isolated != nil && cfg.Isolated.Demo == "screenshots" {
		if err := demofixtures.SeedUsageEvents(runCtx, runtimeStore); err != nil {
			return err
		}
	}
	if err := backfillRuntimeSessionProjects(runCtx, cfg.Global.Projects, runtimeStore, project.LoadWorkflow); err != nil {
		return err
	}

	events := hub.New[project.Event]()
	activityBroker := activity.NewBroker()
	globalDispatchGate, err := buildGlobalDispatchPools(cfg.Global, runtimeStore)
	if err != nil {
		return err
	}
	runtimeGitHubToken := newRuntimeGitHubTokenState(runtimeGlobalGitHubToken(cfg.Runtime.GitHubToken))
	globalConfigState := newGlobalConfigState(cfg.Global)
	refreshGitHubToken := runtimeGitHubTokenRefresher(globalConfigState, runtimeGitHubToken)
	managerConfig := managerConfigWithRuntimeGitHubToken(cfg.Global, runtimeGitHubToken.get())
	snapshotHub := hub.New[telemetry.Snapshot]()
	projectIDs := make([]string, 0, len(cfg.Global.Projects))
	for _, projectConfig := range cfg.Global.Projects {
		projectIDs = append(projectIDs, projectConfig.ID)
	}
	stalenessAcknowledgements, err := staleness.NewAcknowledgements(runCtx, runtimeStore, snapshotHub, projectIDs)
	if err != nil {
		return err
	}
	dispatchPacer := runnerpkg.NewStartupDispatchPacer(runnerpkg.StartupDispatchPacerConfig{
		MaxStartsPerSecond: managerConfig.Startup.MaxSpawnPerSecond,
		Jitter:             managerConfig.Startup.Jitter,
		RampStarts:         startupDispatchRampStarts(globalDispatchGate),
	})
	var hubScheduling orchestrator.SchedulingSource
	if cfg.Global.Client.Configured() {
		hubScheduling, err = newHubScheduling(cfg.Global, cfg.Version)
		if err != nil {
			return err
		}
	}
	projectFactory := withRunnerFactory(project.Dependencies{
		Events:             events,
		Scheduling:         hubScheduling,
		Logger:             logger,
		GlobalDispatchGate: globalDispatchGate,
		DispatchPacer:      dispatchPacer,
		WorkflowMetrics:    runtimeStore,
		Efficiency:         runtimeStore,
		WorkAttempts:       runtimeStore,
		ProgressSpend:      runtimeStore,
		AgentResume:        runtimeStore,
		ValidatorMemo:      runtimeStore,
		StalenessWarnings:  stalenessAcknowledgements,
		RetroStore:         runtimeStore,
		RoutineStore:       runtimeStore,
		AdmissionStore:     runtimeStore,
		ScheduleRuns:       runtimeStore,
		Activity:           activityBroker,
		GitHubToken:        runtimeGitHubToken.get(),
		RefreshGitHubToken: refreshGitHubToken,
		ScheduleOwner:      cfg.Global.InstanceName,
		ConnectorFactory:   cfg.ConnectorFactory,
		Runner:             cfg.Runner,
	}, runtimeStore, nil, runtimeGitHubToken.get)
	managerDependencies := deps.managerDependencies
	managerDependencies.ProjectFactory = projectFactory
	managerDependencies.Events = events
	managerDependencies.Logger = logger
	manager, err := project.NewManager(managerConfig, managerDependencies)
	if err != nil {
		return err
	}
	defer func() {
		stop()
		manager.Wait()
		for _, runtimeProject := range manager.Registry().List() {
			if err := runtimeProject.Close(); err != nil {
				logger.Warn("close runtime project failed", "project_id", runtimeProject.ID(), "error", err)
			}
		}
	}()
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

	snapshotSeq := &atomic.Uint64{}
	providerStatus := deps.providerStatusManager
	if providerStatus == nil {
		providerStatus = statuspage.NewManager(statuspage.ManagerConfig{}, statuspage.ManagerDependencies{Logger: logger})
	}
	var windowStatus *tmuxstatus.Status
	tmuxPane := os.Getenv("TMUX_PANE")
	if tmuxstatus.Enabled(os.Getenv("TMUX"), tmuxPane, cfg.Global.Ops.TmuxWindowStatus) {
		windowStatus, err = tmuxstatus.New(runCtx, tmuxPane, logger)
		if err != nil {
			logger.Warn("initialize tmux window status failed", "error", err)
		}
	}
	if windowStatus != nil {
		defer closeTmuxWindowStatus(windowStatus, logger)
	}
	healthNotifications, err := newHealthNotificationManager(cfg.Global, runtimeStore, logger)
	if err != nil {
		return err
	}
	updateScheduler, err := newRuntimeUpdateScheduler(
		cfg,
		logger,
		func(ctx context.Context) (func(), bool) {
			return runtimeUpdateIdleReservation(ctx, manager.Registry(), globalDispatchGate)
		},
		func(ctx context.Context) (func(), error) {
			return runtimeUpdateDrainReservation(ctx, manager.Registry(), globalDispatchGate)
		},
	)
	if err != nil {
		return err
	}
	kanbanWorkflow, err := bootKanbanWorkflow(runCtx, cfg)
	if err != nil {
		return err
	}
	boardSnapshotStore := deps.boardSnapshotStore
	if boardSnapshotStore == nil {
		boardSnapshotStore, err = boardsnapshot.New(boardsnapshot.Config{
			Path:   runtimeBoardSnapshotPath(cfg),
			MaxAge: time.Duration(kanbanWorkflow.Server.BoardSnapshotStaleAfterSeconds) * time.Second,
		})
		if err != nil {
			return err
		}
	}
	cachedSnapshot, cached, loadErr := boardSnapshotStore.Load(runCtx)
	if loadErr != nil {
		logger.Warn("load board snapshot failed", "error", loadErr)
	}
	if cached {
		if err := stalenessAcknowledgements.Publish(cachedSnapshot); err != nil {
			return fmt.Errorf("publish cached board snapshot: %w", err)
		}
	} else if err := publishStartupSnapshotOnce(runCtx, cfg.Global, stalenessAcknowledgements, runtimeStore, displayURL, time.Now(), updateScheduler); err != nil {
		return err
	}
	chatProvider := buildChatProvider(manager.Registry(), logger)
	var resourceWorkers sync.WaitGroup
	defer func() {
		stop()
		if deps.backgroundWaitStarted != nil {
			deps.backgroundWaitStarted()
		}
		resourceWorkers.Wait()
	}()
	readInstalledBuild := deps.readInstalledBuild
	if readInstalledBuild == nil {
		readInstalledBuild = newInstalledExecutableBuildReader(os.Executable, defaultCommandRunner)
	}
	resourceWorkers.Go(func() {
		runRuntimeBuildDriftMonitor(runCtx, cfg.Build, deps.buildDriftInterval, readInstalledBuild, logger)
	})
	if windowStatus != nil {
		resourceWorkers.Go(func() {
			runTmuxWindowStatus(runCtx, snapshotHub, windowStatus, defaultSnapshotInterval, logger)
		})
	}
	resourceWorkers.Go(func() {
		providerStatus.Run(runCtx, func() []statuspage.Source {
			return providerStatusSources(manager.Registry())
		}, func() []telemetry.TrackerCondition {
			snapshot, ok := snapshotHub.Latest()
			if !ok {
				return nil
			}
			return append([]telemetry.TrackerCondition(nil), snapshot.TrackerUnavailable...)
		}, time.Now)
	})
	resourceWorkers.Go(func() {
		publishSnapshots(runCtx, manager.Registry(), globalDispatchGate, stalenessAcknowledgements, snapshotSeq, cfg.Shutdown, runtimeStore, displayURL, providerStatus, defaultSnapshotInterval, time.Now, updateScheduler)
	})
	if healthNotifications.Enabled() {
		resourceWorkers.Go(func() {
			healthNotifications.Run(runCtx, snapshotHub, manager.Registry().Health)
		})
	}
	go republishSnapshotsOnProjectEvents(runCtx, events, snapshotHub, logger) // #nosec G118 -- runCtx is the service-lifetime context canceled during shutdown.
	resourceWorkers.Go(func() {
		persistBoardSnapshots(runCtx, snapshotHub, boardSnapshotStore, deps.boardSnapshotInterval, logger)
	})
	startupLifecycle := web.NewStartupLifecycle()
	//nolint:contextcheck // Echo middleware receives request contexts at serve time.
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
		Kanban:             kanbanWorkflow.Server.Kanban,
		KanbanWorkflow:     kanbanWorkflow,
		Demo: web.DemoConfig{
			Mode:  isolatedDemo(cfg),
			Clock: isolatedDemoClock(cfg),
		},
	}, web.Dependencies{
		Hub:                 snapshotHub,
		Store:               runtimeStore,
		Registry:            manager.Registry(),
		Connector:           firstConnector(manager),
		StartupLifecycle:    startupLifecycle,
		Refresher:           refresherForRegistry(manager.Registry()),
		OperatorMoves:       registryRefresher{registry: manager.Registry()},
		Recovery:            recoveryForRegistry(manager.Registry()),
		RunStopper:          registryRefresher{registry: manager.Registry()},
		UpdateApplier:       updateScheduler,
		Activity:            activityBroker,
		Chat:                chatProvider,
		IssueExplainer:      newIssueExplainer(snapshotHub, runtimeStore),
		HealthNotifications: healthNotifications,
		WorkerProcesses:     runtimeStore,
		StalenessWarnings:   stalenessAcknowledgements,
	})
	if err != nil {
		return err
	}

	onGlobalReload := func(reloaded globalconfig.Config) {
		syncGlobalDispatchProjects(globalDispatchGate, reloaded.Projects, manager.Registry())
		globalConfigState.set(reloaded)
		republishLatestSnapshot(snapshotHub, logger)
	}
	reloadLogLevel := runtimeLogLevelForReload(cfg)
	applyRuntimeConfig := func(reloaded globalconfig.Config) error {
		return applyGlobalRuntimeConfig(globalDispatchGate, runtimeStore, reloadLogLevel, reloaded)
	}
	startProjects := func(ctx context.Context) error {
		if err := manager.Start(ctx); err != nil {
			return err
		}
		go updateScheduler.Run(ctx)
		globalWatcherDone := startGlobalConfigWatcher(ctx, cfg.Global, manager, logger, runtimeGitHubToken, applyRuntimeConfig, onGlobalReload)
		credentialWatcherDone := startBackendCredentialWatchers(ctx, manager.Registry(), events, logger)
		resourceWorkers.Go(func() {
			<-credentialWatcherDone
		})
		select {
		case globalWatcherStarted <- globalWatcherDone:
		default:
		}
		resourceWorkers.Go(func() {
			registry := manager.Registry()
			runPauseMonitor(ctx, pauseMonitorDeps{
				read: func() (globalconfig.Config, error) {
					return globalconfig.Read(cfg.Global.Path, globalconfig.WithProjectPathLiterals())
				},
				write: func(updated globalconfig.Config) error {
					return globalconfig.Write(cfg.Global.Path, updated, globalconfig.WithProjectPathLiterals())
				},
				unpause: func(ctx context.Context, projectID string) error {
					return manager.Unpause(ctx, project.ID(projectID))
				},
				pause: func(ctx context.Context, projectID string) error {
					return manager.Pause(ctx, project.ID(projectID))
				},
				connectorFor: func(projectID string) connector.Connector {
					trackedProject, ok := registry.Get(project.ID(projectID))
					if !ok || trackedProject == nil {
						return nil
					}
					return trackedProject.Connector()
				},
				repositoryFor: func(projectID string) string {
					trackedProject, ok := registry.Get(project.ID(projectID))
					if !ok || trackedProject == nil {
						return ""
					}
					return trackedProject.Workflow().Config.Tracker.Repository
				},
				trackerKindFor: func(projectID string) string {
					trackedProject, ok := registry.Get(project.ID(projectID))
					if !ok || trackedProject == nil {
						return ""
					}
					return trackedProject.Workflow().Config.Tracker.Kind
				},
				pauseStatus:      registry.PauseExitStatus,
				setPauseStatus:   registry.SetPauseExitStatus,
				clearPauseStatus: registry.ClearPauseExitStatus,
				logger:           logger,
			})
		})
		return nil
	}
	readiness := startupReadiness{}
	if cfg.StartupRecovery != nil {
		readiness.AwaitServe = func(ctx context.Context) error {
			return awaitStartupServer(ctx, startupServerURL(listener.Addr()))
		}
		readiness.MarkHealthy = cfg.StartupRecovery.MarkHealthy
	}

	if useDashboard {
		if err := printBootBanner(cfg, displayURL); err != nil {
			return err
		}
		listenerOwned = false
		if cfg.Shutdown == nil {
			return runStartupAndServe(runCtx, startupLifecycle, startProjects, readiness, func(ctx context.Context) error {
				return serveWithTerminalDashboard(ctx, server, listener, snapshotHub, cfg.Build, runtimeLogPath(cfg), cfg.Output, nil, nil, nil)
			})
		}
		return runStartupAndServe(runCtx, startupLifecycle, startProjects, readiness, func(ctx context.Context) error {
			return runWithShutdown(ctx, runningShutdownConfig{
				Controller:        cfg.Shutdown,
				Registry:          manager.Registry(),
				SnapshotHub:       snapshotHub,
				SnapshotSeq:       snapshotSeq,
				Output:            cfg.Output,
				Logger:            logger,
				TerminalDashboard: true,
				DrainTimeoutSource: func() time.Duration {
					return shutdownDrainTimeout(manager.Registry())
				},
				ProgressInterval: defaultShutdownProgressInterval,
				HardTimeout:      defaultShutdownHardTimeout,
				WorkerProcesses:  runtimeStore,
				WorkerReapGrace:  procgroup.DefaultTerminationGrace,
			}, func(ctx context.Context) error {
				return serveWithTerminalDashboard(ctx, server, listener, snapshotHub, cfg.Build, runtimeLogPath(cfg), cfg.Output, func() time.Duration {
					return shutdownDrainTimeout(manager.Registry())
				}, func() {
					requestTerminalShutdownInterrupt(cfg.Shutdown)
				}, nil)
			})
		})
	}
	if err := printBootBanner(cfg, displayURL); err != nil {
		return err
	}
	listenerOwned = false
	if cfg.Shutdown == nil {
		return runStartupAndServe(runCtx, startupLifecycle, startProjects, readiness, func(ctx context.Context) error {
			return serve(ctx, server, listener)
		})
	}
	return runStartupAndServe(runCtx, startupLifecycle, startProjects, readiness, func(ctx context.Context) error {
		return runWithShutdown(ctx, runningShutdownConfig{
			Controller:  cfg.Shutdown,
			Registry:    manager.Registry(),
			SnapshotHub: snapshotHub,
			SnapshotSeq: snapshotSeq,
			Output:      cfg.Output,
			Logger:      logger,
			DrainTimeoutSource: func() time.Duration {
				return shutdownDrainTimeout(manager.Registry())
			},
			ProgressInterval: defaultShutdownProgressInterval,
			HardTimeout:      defaultShutdownHardTimeout,
			WorkerProcesses:  runtimeStore,
			WorkerReapGrace:  procgroup.DefaultTerminationGrace,
		}, func(ctx context.Context) error {
			return serve(ctx, server, listener)
		})
	})
}

func newHealthNotificationManager(cfg globalconfig.Config, stateStore store.HealthNotificationStateStore, logger *slog.Logger) (*healthnotify.Manager, error) {
	health := cfg.Notifications.Health
	if strings.TrimSpace(health.Webhook.URL) == "" {
		return healthnotify.NewManager(healthnotify.Config{}, healthnotify.Dependencies{})
	}
	hostname, err := os.Hostname()
	if err != nil {
		if logger != nil {
			logger.Warn("resolve health notification hostname failed", "error", err)
		}
		hostname = ""
	}
	instance := strings.TrimSpace(cfg.InstanceName)
	if instance == "" {
		instance = strings.TrimSpace(cfg.Global.Identity.Name)
	}
	if instance == "" {
		instance = strings.TrimSpace(strings.SplitN(hostname, ".", 2)[0])
	}
	manager, err := healthnotify.NewManager(healthnotify.Config{
		Webhook: notify.WebhookConfig{
			URL:     health.Webhook.URL,
			Headers: health.Webhook.Headers,
			Timeout: health.Webhook.Timeout(),
		},
		Instance: instance,
		Host:     hostname,
		Debounce: health.Debounce(),
	}, healthnotify.Dependencies{Store: stateStore, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("configure health notifications: %w", err)
	}
	return manager, nil
}

type sessionProjectBackfiller interface {
	BackfillSessionProjectIDs(context.Context, []store.SessionProjectAttribution) (int64, error)
}

func backfillRuntimeSessionProjects(
	ctx context.Context,
	projects []globalconfig.Project,
	backfiller sessionProjectBackfiller,
	loadWorkflow func(globalconfig.Project) (workflowconfig.Workflow, error),
) error {
	attributions := make(map[string]store.SessionProjectAttribution, len(projects))
	ambiguous := make(map[string]struct{})
	for _, configuredProject := range projects {
		workflow, err := loadWorkflow(configuredProject)
		if err != nil {
			return fmt.Errorf("load project workflow %s for session attribution: %w", configuredProject.ID, err)
		}
		repository := strings.TrimSpace(workflow.Config.Tracker.Repository)
		projectID := strings.TrimSpace(configuredProject.ID)
		if repository == "" || projectID == "" {
			continue
		}
		key := strings.ToLower(repository)
		if existing, ok := attributions[key]; ok && existing.ProjectID != projectID {
			delete(attributions, key)
			ambiguous[key] = struct{}{}
			continue
		}
		if _, ok := ambiguous[key]; !ok {
			attributions[key] = store.SessionProjectAttribution{ProjectID: projectID, Repository: repository}
		}
	}

	keys := make([]string, 0, len(attributions))
	for key := range attributions {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	ordered := make([]store.SessionProjectAttribution, 0, len(keys))
	for _, key := range keys {
		ordered = append(ordered, attributions[key])
	}
	if _, err := backfiller.BackfillSessionProjectIDs(ctx, ordered); err != nil {
		return fmt.Errorf("backfill session project attribution: %w", err)
	}
	return nil
}

func globalProjectCandidates(projects []globalconfig.Project) []scheduler.ProjectCandidate {
	return globalProjectCandidatesWithDefault(projects, nil)
}

func globalProjectCandidatesWithDefault(projects []globalconfig.Project, defaultActiveHours *activehours.Config) []scheduler.ProjectCandidate {
	candidates := make([]scheduler.ProjectCandidate, 0, len(projects))
	for _, projectConfig := range projects {
		activeHours := activehours.Config{}
		if projectConfig.ActiveHours != nil {
			activeHours = projectConfig.ActiveHours.Normalize()
		} else if defaultActiveHours != nil {
			activeHours = defaultActiveHours.Normalize()
		}
		overrideUntil := activehours.ParsePersistedOverride(projectConfig.ActiveHoursOverrideUntil)
		candidates = append(candidates, scheduler.ProjectCandidate{
			ID:                       projectConfig.ID,
			Pool:                     projectConfig.Pool,
			Weight:                   projectConfig.Weight,
			Priority:                 projectConfig.Priority,
			Paused:                   projectConfig.Paused,
			ActiveHours:              activeHours,
			ActiveHoursOverrideUntil: overrideUntil,
		})
	}
	return candidates
}

func syncGlobalDispatchProjects(
	gate *scheduler.PoolRegistry,
	projects []globalconfig.Project,
	registry *project.Registry,
) {
	candidates := globalProjectCandidates(projects)
	for index, candidate := range candidates {
		if runtimeProject, ok := registry.Get(project.ID(candidate.ID)); ok {
			candidates[index] = runtimeProject.DispatchCandidate()
		}
	}
	gate.SetProjects(candidates)
	for _, candidate := range candidates {
		if candidate.Paused {
			continue
		}
		runtimeProject, ok := registry.Get(project.ID(candidate.ID))
		if ok && runtimeProject.Running() {
			continue
		}
		gate.MarkIdle(candidate)
	}
}

func startOnboarding(ctx context.Context, cfg BootConfig) error {
	logger := slog.Default()
	listener, displayURL, err := listenForBoot(ctx, cfg)
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

	//nolint:contextcheck // Echo middleware receives request contexts at serve time.
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

type startupReadiness struct {
	AwaitServe  func(context.Context) error
	MarkHealthy func(context.Context) error
}

func runStartupAndServe(
	ctx context.Context,
	lifecycle *web.StartupLifecycle,
	startup func(context.Context) error,
	readiness startupReadiness,
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
	serveReady := readiness.AwaitServe == nil
	if readiness.AwaitServe != nil {
		go func() {
			results <- startupServeResult{name: "readiness", err: readiness.AwaitServe(runCtx)}
		}()
	}

	startupDone := false
	healthyMarked := false
	for {
		var result startupServeResult
		if startupDone && serveReady && !healthyMarked {
			select {
			case result = <-results:
			default:
				completeStartupLifecycle(lifecycle, nil)
				if readiness.MarkHealthy != nil {
					if err := readiness.MarkHealthy(runCtx); err != nil {
						slog.Default().Warn("record healthy startup failed", "error", err)
					}
				}
				healthyMarked = true
				result = <-results
			}
		} else {
			result = <-results
		}
		switch result.name {
		case "startup":
			if result.err != nil {
				completeStartupLifecycle(lifecycle, result.err)
				cancel()
				serveResult := awaitStartupServeResult(results, "serve")
				if primaryShutdownError(serveResult.err) {
					logSecondaryShutdownError("startup", serveResult.err, result.err)
					return serveResult.err
				}
				if unexpected := unexpectedBootServeError(serveResult.err); unexpected != nil {
					return errors.Join(result.err, unexpected)
				}
				return result.err
			}
			startupDone = true
		case "readiness":
			if result.err == nil {
				serveReady = true
			}
		case "serve":
			cancel()
			if !startupDone {
				startupResult := awaitStartupServeResult(results, "startup")
				completeStartupLifecycle(lifecycle, startupResult.err)
				if primaryShutdownError(result.err) {
					logSecondaryShutdownError("startup", result.err, startupResult.err)
					return result.err
				}
				if startupResult.err != nil {
					if unexpected := unexpectedBootServeError(result.err); unexpected != nil {
						return errors.Join(unexpected, startupResult.err)
					}
					return startupResult.err
				}
			} else if !healthyMarked {
				completeStartupLifecycle(lifecycle, errors.New("serving stopped before startup readiness"))
			}
			return result.err
		}
	}
}

func awaitStartupServeResult(results <-chan startupServeResult, name string) startupServeResult {
	for result := range results {
		if result.name == name {
			return result
		}
	}
	return startupServeResult{name: name, err: context.Canceled}
}

func awaitStartupServer(ctx context.Context, baseURL string) error {
	endpoint := strings.TrimRight(baseURL, "/") + "/.detent-startup-readiness"
	client := http.Client{Timeout: time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			return nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func startupServerURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	ip := net.ParseIP(strings.SplitN(host, "%", 2)[0])
	if ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

func completeStartupLifecycle(lifecycle *web.StartupLifecycle, err error) {
	if lifecycle == nil {
		return
	}
	if err != nil {
		lifecycle.MarkFailed()
		return
	}
	lifecycle.MarkReady()
}

func primaryShutdownError(err error) bool {
	return errors.Is(err, ErrShutdownForced) || errors.Is(err, ErrShutdownTimeout)
}

func logSecondaryShutdownError(component string, primary error, secondary error) {
	if secondary == nil || errors.Is(secondary, context.Canceled) {
		return
	}
	slog.Default().Warn("shutdown cleanup error ignored in favor of primary result", "component", component, "primary", primary, "error", secondary)
}

func unexpectedBootServeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func serveWithTerminalDashboard(
	ctx context.Context,
	server *web.Server,
	listener net.Listener,
	snapshots *hub.Hub[telemetry.Snapshot],
	build buildinfo.Info,
	logPath string,
	output io.Writer,
	shutdownTimeoutSource func() time.Duration,
	interrupt func(),
	afterExit func(),
) error {
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

	model, err := tui.NewModel(
		runCtx,
		snapshots,
		tui.WithBuild(build),
		tui.WithLogPath(logPath),
		tui.WithShutdownTimeoutSource(shutdownTimeoutSource),
		tui.WithInterruptFunc(interrupt),
	)
	if err != nil {
		return err
	}
	defer model.Close()

	type result struct {
		err error
	}
	results := make(chan result, 2)
	listenerOwned = false
	go func() {
		results <- result{err: serve(runCtx, server, listener)}
	}()
	go func() {
		finalModel, runErr := runTerminalDashboardProgram(runCtx, tea.NewProgram(model, terminalDashboardProgramOptions()...))
		summaryErr := writeTerminalDashboardSummary(output, finalModel, model)
		if afterExit != nil {
			afterExit()
		}
		results <- result{err: errors.Join(runErr, summaryErr)}
	}()

	first := <-results
	cancel()
	second := <-results
	return terminalDashboardError(first.err, second.err)
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

var errNilTerminalDashboardProgram = errors.New("nil terminal dashboard program")

func terminalDashboardProgramOptions() []tea.ProgramOption {
	return []tea.ProgramOption{
		tea.WithFilter(terminalDashboardMessageFilter),
		tea.WithoutSignalHandler(),
	}
}

func terminalDashboardMessageFilter(_ tea.Model, msg tea.Msg) tea.Msg {
	if _, ok := msg.(tea.InterruptMsg); ok {
		return tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl})
	}
	return msg
}

func runTerminalDashboardProgram(ctx context.Context, program terminalDashboardProgram) (tea.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if program == nil {
		return nil, errNilTerminalDashboardProgram
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			program.Kill()
		case <-done:
		}
	}()

	finalModel, err := program.Run()
	close(done)
	return finalModel, err
}

func writeTerminalDashboardSummary(output io.Writer, finalModel tea.Model, fallback tui.Model) error {
	if output == nil {
		output = os.Stdout
	}
	model := fallback
	if final, ok := finalModel.(tui.Model); ok {
		model = final
	}
	if _, err := fmt.Fprintln(output, model.ExitSummary()); err != nil {
		return fmt.Errorf("write terminal dashboard exit summary: %w", err)
	}
	return nil
}

func shouldLaunchTerminalDashboard(cfg BootConfig) bool {
	return cfg.Mode == BootModeRunning && cfg.StdoutTTY && !cfg.Headless
}

func requestTerminalShutdownInterrupt(controller *ShutdownController) bool {
	request, handled := controller.RequestInterruptKind()
	slog.Default().Debug("shutdown interrupt request", "operation", "shutdown_interrupt_request", "source", "terminal_dashboard", "request", request.String(), "handled", handled)
	return handled
}

func redirectDefaultLogger(path string, level string) (func(), error) {
	return redirectDefaultLoggerWithRotation(path, level, defaultRuntimeLogRotation())
}

func redirectDefaultLoggerWithRotation(path string, level string, rotation logRotation, levels ...*slog.LevelVar) (func(), error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	file, err := newRotatingLogWriter(path, rotation)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	previous := slog.Default()
	parsedLevel := parseSlogLevel(level)
	logLevel := resolveRuntimeLogLevel(parsedLevel, levels)
	slog.SetDefault(slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{
		Level:     logLevel,
		AddSource: runtimeLogAddSource(parsedLevel),
	})))

	return func() {
		slog.SetDefault(previous)
		if err := file.Close(); err != nil {
			previous.Warn("close log file failed", "path", path, "error", err)
		}
	}, nil
}

func resolveRuntimeLogLevel(initial slog.Level, levels []*slog.LevelVar) *slog.LevelVar {
	if len(levels) > 0 && levels[0] != nil {
		levels[0].Set(initial)
		return levels[0]
	}
	level := &slog.LevelVar{}
	level.Set(initial)
	return level
}

type logRotation struct {
	MaxSizeBytes int64
	MaxBackups   int
}

type rotatingLogWriter struct {
	mu   sync.Mutex
	path string
	cfg  logRotation
	file *os.File
	size int64
}

func defaultRuntimeLogRotation() logRotation {
	return logRotation{
		MaxSizeBytes: int64(defaultRuntimeLogMaxSizeBytes),
		MaxBackups:   defaultRuntimeLogMaxBackups,
	}
}

func runtimeLogRotation(settings RuntimeSettings) logRotation {
	return logRotation{
		MaxSizeBytes: int64(settings.LogMaxSizeBytes.Value),
		MaxBackups:   settings.LogMaxBackups.Value,
	}
}

func newRotatingLogWriter(path string, cfg logRotation) (*rotatingLogWriter, error) {
	if cfg.MaxSizeBytes < 0 {
		cfg.MaxSizeBytes = 0
	}
	if cfg.MaxBackups < 0 {
		cfg.MaxBackups = 0
	}
	file, size, err := openRotatingLogFile(path)
	if err != nil {
		return nil, err
	}
	return &rotatingLogWriter{
		path: path,
		cfg:  cfg,
		file: file,
		size: size,
	}, nil
}

func openRotatingLogFile(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, 0, errors.Join(err, closeErr)
		}
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.cfg.MaxSizeBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.cfg.MaxSizeBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingLogWriter) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if err := rotateLogFiles(w.path, w.cfg.MaxBackups); err != nil {
		return err
	}
	file, size, err := openRotatingLogFile(w.path)
	if err != nil {
		return err
	}
	w.file = file
	w.size = size
	return nil
}

func rotateLogFiles(path string, maxBackups int) error {
	if maxBackups <= 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	if err := os.Remove(rotatedLogPath(path, maxBackups)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for index := maxBackups - 1; index >= 1; index-- {
		from := rotatedLogPath(path, index)
		to := rotatedLogPath(path, index+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(path, rotatedLogPath(path, 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func rotatedLogPath(path string, index int) string {
	return fmt.Sprintf("%s.%d", path, index)
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

func bootKanbanWorkflow(ctx context.Context, cfg BootConfig) (workflowconfig.Config, error) {
	workflow, ok, err := bootWorkflow(ctx, cfg)
	if err != nil || !ok {
		return workflowconfig.Default(), err
	}
	workflow.Config.Server.Kanban.Normalize()
	return workflow.Config, nil
}

func bootWorkflow(ctx context.Context, cfg BootConfig) (workflowconfig.Workflow, bool, error) {
	firstProject := firstGlobalProject(cfg.Global)
	if strings.TrimSpace(firstProject.Workflow) != "" {
		workflow, err := loadRuntimeProjectWorkflow(ctx, firstProject, runtimeDeps{}.withDefaults())
		if err != nil {
			return workflowconfig.Workflow{}, false, fmt.Errorf("load boot workflow: %w", err)
		}
		return workflow, true, nil
	}

	path := strings.TrimSpace(cfg.WorkflowPath)
	if path == "" {
		return workflowconfig.Workflow{}, false, nil
	}
	workflow, err := workflowconfig.LoadWorkflow(path)
	if err != nil {
		return workflowconfig.Workflow{}, false, fmt.Errorf("load boot workflow: %w", err)
	}
	return workflow, true, nil
}

func buildGlobalScheduler(settings globalconfig.Settings, fairShareStore scheduler.FairShareStore) (scheduler.GlobalScheduler, error) {
	schedulerConfig, err := globalSchedulerConfig(settings, fairShareStore)
	if err != nil {
		return nil, err
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

func buildGlobalDispatchPools(
	cfg globalconfig.Config,
	fairShareStore scheduler.FairShareStore,
) (*scheduler.PoolRegistry, error) {
	pools, err := globalPoolConfigs(cfg.Global, fairShareStore)
	if err != nil {
		return nil, err
	}
	registry, err := scheduler.NewPoolRegistry(pools, globalProjectCandidatesWithDefault(cfg.Projects, cfg.Global.ActiveHours))
	if err != nil {
		return nil, fmt.Errorf("create agent pools: %w", err)
	}
	return registry, nil
}

func startupDispatchRampStarts(registry *scheduler.PoolRegistry) int {
	if registry == nil {
		return 1
	}
	total := 0
	for _, pool := range registry.PoolSnapshots() {
		capacity := pool.BurstTo
		if capacity <= 0 {
			capacity = pool.Capacity
		}
		total += capacity
	}
	return max(total, 1)
}

func globalPoolConfigs(
	settings globalconfig.Settings,
	fairShareStore scheduler.FairShareStore,
) ([]scheduler.PoolConfig, error) {
	defaultConfig, err := globalSchedulerConfig(settings, fairShareStore)
	if err != nil {
		return nil, err
	}
	pools := make([]scheduler.PoolConfig, 0, len(settings.AgentPools)+1)
	pools = append(pools, scheduler.PoolConfig{
		Name:      scheduler.DefaultPoolName,
		Scheduler: defaultConfig,
	})
	for _, pool := range settings.AgentPools {
		poolSettings := settings
		poolSettings.MaxConcurrentAgents = pool.MaxConcurrentAgents
		poolSettings.Scheduling = pool.Scheduling
		if strings.TrimSpace(poolSettings.Scheduling) == "" {
			poolSettings.Scheduling = settings.Scheduling
		}
		poolConfig, err := globalSchedulerConfig(poolSettings, fairShareStore)
		if err != nil {
			return nil, err
		}
		pools = append(pools, scheduler.PoolConfig{
			Name:      pool.Name,
			BurstTo:   pool.BurstTo,
			Scheduler: poolConfig,
		})
	}
	return pools, nil
}

func globalSchedulerConfig(settings globalconfig.Settings, fairShareStore scheduler.FairShareStore) (scheduler.Config, error) {
	halfLife, err := globalFairShareHalfLife(settings.FairShare)
	if err != nil {
		return scheduler.Config{}, err
	}

	schedulerConfig := scheduler.Config{
		Kind:          settings.Scheduling,
		Capacity:      settings.MaxConcurrentAgents,
		DecayHalfLife: halfLife,
	}
	if settings.Scheduling == globalconfig.SchedulingFairShare {
		schedulerConfig.FairShareStore = fairShareStore
	}

	return schedulerConfig, nil
}

func applyGlobalRuntimeConfig(
	gate *scheduler.PoolRegistry,
	fairShareStore scheduler.FairShareStore,
	logLevel *slog.LevelVar,
	cfg globalconfig.Config,
) error {
	pools, err := globalPoolConfigs(cfg.Global, fairShareStore)
	if err != nil {
		return err
	}
	if err := gate.Reconfigure(pools, globalProjectCandidatesWithDefault(cfg.Projects, cfg.Global.ActiveHours)); err != nil {
		return fmt.Errorf("reconfigure agent pools: %w", err)
	}
	if logLevel != nil {
		logLevel.Set(parseSlogLevel(cfg.LogLevel))
	}
	return nil
}

func runtimeLogLevelForReload(cfg BootConfig) *slog.LevelVar {
	switch cfg.Runtime.LogLevel.Source {
	case runtimeSourceConfig, runtimeSourceDefault:
		return cfg.LogLevel
	default:
		return nil
	}
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
		return 0, errors.New("global.fair_share.half_life: must be a duration string")
	}
}

func openRuntimeStore(ctx context.Context, cfg BootConfig) (store.Store, error) {
	return store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    runtimeStorePath(cfg),
	})
}

func acquireRuntimeInstanceLock(lockPath string) (*instancelock.Lock, error) {
	lock, err := instancelock.Acquire(lockPath)
	if errors.Is(err, instancelock.ErrHeld) {
		var heldErr *instancelock.HeldError
		if errors.As(err, &heldErr) && heldErr.Owner.PID > 0 && !heldErr.Owner.StartedAt.IsZero() {
			return nil, fmt.Errorf("another detent (pid %d, started %s) holds %s", heldErr.Owner.PID, heldErr.Owner.StartedAt.Format(time.RFC3339), lockPath)
		}
		return nil, fmt.Errorf("another detent holds %s, but its owner metadata is unreadable; stop the other instance or remove the stale lock", lockPath)
	}
	if err != nil {
		return nil, fmt.Errorf("protect runtime database with %q: %w", lockPath, err)
	}
	return lock, nil
}

func runtimeStoreIsMemory(path string) bool {
	if path == ":memory:" {
		return true
	}
	uri, ok := runtimeStoreURI(path)
	if !ok {
		return false
	}
	if uri.Query().Get("mode") == "memory" {
		return true
	}
	uriPath, ok := runtimeStoreURIPath(uri)
	return ok && uriPath == ":memory:"
}

func runtimeStoreLockPath(path string) string {
	uri, ok := runtimeStoreURI(path)
	if ok {
		if uriPath, pathOK := runtimeStoreURIPath(uri); pathOK {
			path = uriPath
		}
	}
	return path + ".lock"
}

func runtimeStoreURI(path string) (*url.URL, bool) {
	if !strings.HasPrefix(path, "file:") {
		return nil, false
	}
	uri, err := url.Parse(path)
	if err != nil || uri.Scheme != "file" {
		return nil, false
	}
	return uri, true
}

func runtimeStoreURIPath(uri *url.URL) (string, bool) {
	if uri == nil {
		return "", false
	}
	path := uri.Path
	if uri.Opaque != "" {
		decoded, err := url.PathUnescape(uri.Opaque)
		if err != nil {
			return "", false
		}
		path = decoded
	}
	if path == "" {
		return "", false
	}
	if uri.Host != "" && !strings.EqualFold(uri.Host, "localhost") {
		path = "//" + uri.Host + "/" + strings.TrimPrefix(path, "/")
	}
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path), true
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

func runtimeBoardSnapshotPath(cfg BootConfig) string {
	databasePath := runtimeStorePath(cfg)
	if runtimeStoreIsMemory(databasePath) {
		root := filepath.Dir(strings.TrimSpace(cfg.Global.Path))
		if root == "." || root == "" {
			root = filepath.Join(mustGetwd(), ".detent")
		}
		return filepath.Join(root, "detent-board-snapshot.json")
	}
	if uri, ok := runtimeStoreURI(databasePath); ok {
		if uriPath, pathOK := runtimeStoreURIPath(uri); pathOK {
			databasePath = uriPath
		}
	}
	extension := filepath.Ext(databasePath)
	base := strings.TrimSuffix(databasePath, extension)
	return base + "-board-snapshot.json"
}

func runtimeUpdateStatePath(cfg BootConfig) string {
	databasePath := runtimeStorePath(cfg)
	if runtimeStoreIsMemory(databasePath) {
		root := filepath.Dir(strings.TrimSpace(cfg.Global.Path))
		if root == "." || root == "" {
			root = filepath.Join(mustGetwd(), ".detent")
		}
		return filepath.Join(root, "detent-update-state.json")
	}
	if uri, ok := runtimeStoreURI(databasePath); ok {
		if uriPath, pathOK := runtimeStoreURIPath(uri); pathOK {
			databasePath = uriPath
		}
	}
	extension := filepath.Ext(databasePath)
	base := strings.TrimSuffix(databasePath, extension)
	return base + "-update-state.json"
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

func runtimeLogAddSource(level slog.Level) bool {
	value, ok := lookupLogSourceEnv("LOG_ADD_SOURCE", "DETENT_LOG_ADD_SOURCE")
	if ok {
		return parseRuntimeLogBool(value)
	}
	return level == slog.LevelDebug
}

func lookupLogSourceEnv(keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
			return value, true
		}
	}
	return "", false
}

func parseRuntimeLogBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
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

func recoveryForRegistry(registry *project.Registry) web.WorkAttemptRecovery {
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

func (r registryRefresher) RequestTargetedRefresh(ctx context.Context, target web.RefreshTarget) (web.RefreshResponse, error) {
	repository := strings.TrimSpace(target.Repository)
	if repository == "" {
		return r.RequestRefresh(ctx)
	}
	projectIDs := make(map[string]struct{}, len(target.ProjectIDs))
	for _, projectID := range target.ProjectIDs {
		if projectID = strings.TrimSpace(projectID); projectID != "" {
			projectIDs[projectID] = struct{}{}
		}
	}

	var response web.RefreshResponse
	refreshed := false
	for _, trackedProject := range r.registry.List() {
		if !trackedProject.Running() {
			continue
		}
		if len(projectIDs) > 0 {
			if _, ok := projectIDs[string(trackedProject.ID())]; !ok {
				continue
			}
		}
		workflow := trackedProject.Workflow().Config
		configuredRepository := strings.TrimSpace(workflow.Tracker.Repository)
		if len(projectIDs) == 0 && configuredRepository != "" && !strings.EqualFold(configuredRepository, repository) {
			continue
		}
		orch := trackedProject.Orchestrator()
		if orch == nil {
			continue
		}

		var next web.RefreshResponse
		var err error
		if target.IssueNumber > 0 || target.PullRequestNumber > 0 || strings.TrimSpace(target.Branch) != "" {
			next, err = orch.RequestTargetedRefresh(ctx, connector.ReconcileTarget{
				Scope:          repository,
				WorkItemNumber: target.IssueNumber,
				ChangeNumber:   target.PullRequestNumber,
				Revision:       strings.TrimSpace(target.SHA),
				Branch:         strings.TrimSpace(target.Branch),
				Event:          strings.TrimSpace(target.Event),
				DeliveryID:     strings.TrimSpace(target.DeliveryID),
			})
		} else {
			next, err = orch.RequestRefresh(ctx)
		}
		if err != nil {
			return web.RefreshResponse{}, err
		}
		next.Operations = appendOperations([]string{"target:" + repository}, next.Operations)
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

func (r registryRefresher) WorkAttemptReceipt(ctx context.Context, projectID string, attemptID int64) (orchestrator.WorkAttemptRecoveryResponse, error) {
	orch, err := r.projectOrchestrator(projectID)
	if err != nil {
		return orchestrator.WorkAttemptRecoveryResponse{}, err
	}
	return orch.WorkAttemptReceipt(ctx, projectID, attemptID)
}

func (r registryRefresher) ReconcileOperatorMove(ctx context.Context, request orchestrator.OperatorMoveRequest) (orchestrator.OperatorMoveResult, error) {
	orch, err := r.projectOrchestrator(request.ProjectID)
	if err != nil {
		return orchestrator.OperatorMoveResult{}, err
	}
	return orch.ReconcileOperatorMove(ctx, request)
}

func (r registryRefresher) RecoverWorkAttempt(ctx context.Context, request orchestrator.WorkAttemptRecoveryRequest) (orchestrator.WorkAttemptRecoveryResponse, error) {
	orch, err := r.projectOrchestrator(request.ProjectID)
	if err != nil {
		return orchestrator.WorkAttemptRecoveryResponse{}, err
	}
	return orch.RecoverWorkAttempt(ctx, request)
}

func (r registryRefresher) StopRun(ctx context.Context, request orchestrator.StopRunRequest) (orchestrator.StopRunResult, error) {
	orch, err := r.projectOrchestrator(request.ProjectID)
	if err != nil {
		return orchestrator.StopRunResult{}, err
	}
	return orch.StopRun(ctx, request)
}

func (r registryRefresher) projectOrchestrator(projectID string) (*orchestrator.Orchestrator, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, project.ErrMissingProjectID
	}
	trackedProject, ok := r.registry.Get(project.ID(projectID))
	if !ok {
		return nil, project.ErrProjectStopped
	}
	if !trackedProject.Running() {
		return nil, project.ErrNotRunning
	}
	orch := trackedProject.Orchestrator()
	if orch == nil {
		return nil, project.ErrMissingOrchestrator
	}
	return orch, nil
}

func mergeRefreshResponse(current web.RefreshResponse, next web.RefreshResponse) web.RefreshResponse {
	current.Queued = current.Queued || next.Queued
	current.Coalesced = current.Coalesced || next.Coalesced
	current.Refused = current.Refused || next.Refused
	if current.RequestedAt.IsZero() || (!next.RequestedAt.IsZero() && next.RequestedAt.Before(current.RequestedAt)) {
		current.RequestedAt = next.RequestedAt
		current.RequestID = next.RequestID
	}
	if current.RequestID == "" {
		current.RequestID = next.RequestID
	}
	if strings.TrimSpace(next.LastError) != "" {
		if strings.TrimSpace(current.LastError) == "" ||
			current.LastErrorAt == nil ||
			next.LastErrorAt == nil ||
			current.LastErrorAt.Before(*next.LastErrorAt) {
			current.LastError = next.LastError
		}
	}
	current.LastErrorAt = latestTime(current.LastErrorAt, next.LastErrorAt)
	current.RetryAt = earliestTime(current.RetryAt, next.RetryAt)
	current.Operations = appendOperations(current.Operations, next.Operations)
	current.Status = mergeRefreshResponseStatus(current, next)
	return current
}

func mergeRefreshResponseStatus(current web.RefreshResponse, next web.RefreshResponse) telemetry.RefreshAttemptStatus {
	if current.Coalesced || next.Coalesced {
		return telemetry.RefreshAttemptStatusCoalesced
	}
	if current.Queued || next.Queued {
		return telemetry.RefreshAttemptStatusInProgress
	}
	if current.Refused || next.Refused {
		return telemetry.RefreshAttemptStatusRefused
	}
	if current.Status != "" {
		return current.Status
	}
	if next.Status != "" {
		return next.Status
	}
	return ""
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

func listenForBoot(ctx context.Context, cfg BootConfig) (net.Listener, string, error) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", serverAddr(cfg))
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
