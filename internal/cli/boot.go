package cli

import (
	"context"
	"encoding/json"
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
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/digitaldrywood/detent/internal/activehours"
	"github.com/digitaldrywood/detent/internal/boardsnapshot"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/healthnotify"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/logging"
	"github.com/digitaldrywood/detent/internal/notify"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/statuspage"
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
	return resolveBootConfigWithRuntimeDeps(ctx, configPath, host, flags, opts, runtimeDepsFromOptions(opts))
}

func resolveBootConfigWithRuntimeDeps(ctx context.Context, configPath string, host string, flags runtimeFlags, opts options, deps runtimeDeps, deferProjectValidation ...bool) (BootConfig, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return BootConfig{}, err
	}
	path := resolution.Path

	cfg, err := opts.read(path)
	if err == nil {
		if opts.resolveProjects != nil {
			cfg, err = opts.resolveProjects(ctx, cfg)
		} else {
			cfg, err = resolveRunnerProjects(ctx, cfg)
		}
		if err != nil {
			return BootConfig{}, err
		}
		workflowPath := firstGlobalWorkflowPath(cfg)
		runtime, err := resolveRuntimeSettings(ctx, runtimeInput{
			Config:                 &cfg,
			ConfigPath:             resolution,
			Workflow:               workflowPath,
			Flags:                  flags,
			DeferProjectValidation: len(deferProjectValidation) > 0 && deferProjectValidation[0],
		}, deps)
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
		}, deps)
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
	}, deps)
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
	if cfg.Mode == BootModeRunning {
		return startRunning(ctx, cfg)
	}
	if supervisesControllers(cfg) {
		return superviseControllers(ctx, cfg)
	}
	if cfg.Restart != nil && cfg.Restart.control != nil {
		cfg.Restart.control.controller.Store(cfg.Shutdown)
		port := 0
		cfg.Host, cfg.Port, cfg.Headless, cfg.Output = defaultWebHost, &port, true, io.Discard
		controlCtx, cancel := context.WithCancel(ctx)
		ctx = controlCtx
		defer cancel()
		defer func() {
			if err := cfg.Restart.control.close(); err != nil {
				slog.Warn("close controller supervisor channel", "error", err)
			}
		}()
		if cfg.Restart.control.initErr != nil {
			return fmt.Errorf("initialize controller channel: %w", cfg.Restart.control.initErr)
		}
		defer func() {
			if cfg.Restart.artifactRoot != "" {
				if err := os.RemoveAll(cfg.Restart.artifactRoot); err != nil {
					slog.Warn("remove controller execution cache", "error", err)
				}
			}
		}()
		joined := make(chan struct{})
		defer func() { cancel(); <-joined }()
		go func() {
			defer close(joined)
			select {
			case <-cfg.Restart.control.peer.Context().Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	stdout, stderr := os.Stdout, os.Stderr
	if cfg.serviceStdout != nil {
		stdout, stderr = cfg.serviceStdout, cfg.serviceStderr
	}
	stopLogRetention := startServiceLogRetention(ctx, os.Getenv(service.ManagerEnvironment), cfg.Global.Path, runtimeLogRotation(cfg.Runtime), stdout, stderr)
	defer stopLogRetention()
	if cfg.Isolated == nil {
		if err := validateHubStartup(ctx, cfg.Global, runtimeDeps{}); err != nil {
			return err
		}
	}

	switch cfg.Mode {
	case BootModeOnboarding:
		return startOnboarding(ctx, cfg)
	default:
		return startRunning(ctx, cfg)
	}
}

type startRunningDependencies struct {
	snapshotNow           func() time.Time
	boardSnapshotStore    boardsnapshot.Store
	boardSnapshotInterval time.Duration
	backgroundWaitStarted func()
	buildDriftInterval    time.Duration
	readInstalledBuild    installedBuildReader
	managerDependencies   project.ManagerDependencies
	providerStatusManager *statuspage.Manager
	startupMaintenance    func(context.Context, globalconfig.Config, store.Store) func()
}

func startRunning(ctx context.Context, cfg BootConfig) error {
	if err := validateRunnerHubStartup(cfg); err != nil {
		return err
	}
	if cfg.Global.Client.Configured() {
		return startRemoteRunner(ctx, cfg)
	}
	return startRunningWithDependencies(ctx, cfg, startRunningDependencies{})
}

func startRunningWithDependencies(ctx context.Context, cfg BootConfig, deps startRunningDependencies) error {
	if err := validateRunnerHubStartup(cfg); err != nil {
		return err
	}
	return startRemoteRunner(ctx, cfg)
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
	invalidDefinition := false
	for _, configuredProject := range projects {
		workflow, err := loadWorkflow(configuredProject)
		if err != nil {
			if !errors.Is(err, project.ErrProjectDefinition) {
				return fmt.Errorf("load project workflow %s for session attribution: %w", configuredProject.ID, err)
			}
			// The project manager reports invalid workflows as unavailable. Defer
			// this optional migration: without every repository mapping we cannot
			// exclude ambiguous attribution of historical sessions. Continue loading
			// to ensure a later infrastructure failure still aborts startup.
			invalidDefinition = true
			continue
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

	if invalidDefinition {
		return nil
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
			Rank:                     projectConfig.Priority,
			Pool:                     projectConfig.Pool,
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
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()

	// Every worker can publish even when an error exit has stopped consuming.
	// Join before caller-owned stores, workspaces, and listeners are cleaned up.
	results := make(chan startupServeResult, 3)
	workers.Go(func() {
		results <- startupServeResult{name: "startup", err: startup(runCtx)}
	})
	workers.Go(func() {
		results <- startupServeResult{name: "serve", err: serveApp(runCtx)}
	})
	serveReady := readiness.AwaitServe == nil
	if readiness.AwaitServe != nil {
		workers.Go(func() {
			results <- startupServeResult{name: "readiness", err: readiness.AwaitServe(runCtx)}
		})
	}

	startupDone := false
	healthyMarked := false
	for {
		var result startupServeResult
		if startupDone && serveReady && !healthyMarked {
			select {
			case result = <-results:
			default:
				if readiness.MarkHealthy != nil {
					if err := readiness.MarkHealthy(runCtx); err != nil {
						completeStartupLifecycle(lifecycle, err)
						cancel()
						return fmt.Errorf("verify healthy restarted build: %w", err)
					}
				}
				completeStartupLifecycle(lifecycle, nil)
				slog.Info("runner startup ready")
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
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) && runCtx.Err() != nil {
					continue
				}
				completeStartupLifecycle(lifecycle, result.err)
				cancel()
				return fmt.Errorf("verify restarted listener: %w", result.err)
			}
			serveReady = true
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

func awaitStartupServer(ctx context.Context, baseURL string, expected buildinfo.Info) error {
	endpoint := strings.TrimRight(baseURL, "/") + "/health"
	client := http.Client{Timeout: time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			var identity struct {
				Version string `json:"version"`
				Commit  string `json:"commit"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&identity)
			closeErr := response.Body.Close()
			if decodeErr != nil {
				return fmt.Errorf("decode startup listener identity: %w", decodeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close startup listener response: %w", closeErr)
			}
			if strings.TrimSpace(identity.Version) != strings.TrimSpace(expected.Version) ||
				!strings.EqualFold(strings.TrimSpace(identity.Commit), strings.TrimSpace(expected.Commit)) {
				return fmt.Errorf("startup listener build %s/%s does not match restarted build %s/%s", identity.Version, identity.Commit, expected.Version, expected.Commit)
			}
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
	slog.SetDefault(slog.New(logging.NewHandler(file, logLevel, false, logging.SourceFromEnv(os.Getenv))))

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

	if err := rotateLogBackups(path, maxBackups); err != nil {
		return err
	}
	if err := os.Rename(path, rotatedLogPath(path, 1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func rotateLogBackups(path string, maxBackups int) error {
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
		if cfg.Global.Client.IdentityFile != "" && errors.Is(err, os.ErrNotExist) {
			return workflowconfig.Workflow{}, false, nil
		}
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

func buildGlobalScheduler(settings globalconfig.Settings) (scheduler.GlobalScheduler, error) {
	schedulerConfig, err := globalSchedulerConfig(settings)
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

func buildGlobalDispatchPools(cfg globalconfig.Config) (*scheduler.PoolRegistry, error) {
	pools, err := globalPoolConfigs(cfg.Global)
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

func globalPoolConfigs(settings globalconfig.Settings) ([]scheduler.PoolConfig, error) {
	defaultConfig, err := globalSchedulerConfig(settings)
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
		poolConfig, err := globalSchedulerConfig(poolSettings)
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

func globalSchedulerConfig(settings globalconfig.Settings) (scheduler.Config, error) {
	return scheduler.Config{Kind: "strict", Capacity: settings.MaxConcurrentAgents}, nil
}

func applyGlobalRuntimeConfig(
	gate *scheduler.PoolRegistry,
	logLevel *slog.LevelVar,
	cfg globalconfig.Config,
) error {
	pools, err := globalPoolConfigs(cfg.Global)
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
	registry  *project.Registry
	attempts  store.WorkAttemptStore
	processes workerProcessStore
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
		if r.attempts == nil || r.processes == nil {
			return orchestrator.StopRunResult{}, err
		}
		cfg := orchestrator.Config{}
		if tracked, ok := r.registry.Get(project.ID(request.ProjectID)); ok {
			cfg = orchestrator.ConfigFromWorkflow(tracked.Workflow().Config)
		} else if pending, ok := r.registry.Pending(project.ID(request.ProjectID)); ok && pending.Project.Workflow != "" {
			workflow, loadErr := project.LoadWorkflowContext(ctx, pending.Project)
			// Broken workflow settings must not prevent a processless record
			// from being stopped. Canonical routes need no workflow config.
			if loadErr == nil {
				cfg = orchestrator.ConfigFromWorkflow(workflow.Config)
			}
		}
		return orchestrator.StopRecordedRun(ctx, r.attempts, r.processes, request, cfg)
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

func dashboardServiceAddress(addr net.Addr) (string, error) {
	if addr == nil {
		return "", errors.New("listener address is nil")
	}
	host, portText, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", fmt.Errorf("parse listener address %q: %w", addr.String(), err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("invalid listener port %q", portText)
	}
	return dashboardServerAddr(BootConfig{Host: host, Port: &port}), nil
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
