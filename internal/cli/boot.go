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
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/connector/memory"
	"github.com/digitaldrywood/symphony/internal/hub"
	"github.com/digitaldrywood/symphony/internal/project"
	"github.com/digitaldrywood/symphony/internal/store"
	"github.com/digitaldrywood/symphony/internal/telemetry"
	"github.com/digitaldrywood/symphony/internal/tui"
	"github.com/digitaldrywood/symphony/internal/web"
)

const (
	defaultWorkflowFile = "WORKFLOW.md"
	defaultProjectID    = "default"
	defaultWebHost      = "127.0.0.1"
	defaultWebPort      = 4000
	dashboardHost       = "localhost"
	projectURL          = "https://github.com/digitaldrywood/symphony"
)

func resolveBootConfig(configPath string, host string, port int, opts options) (BootConfig, error) {
	if port < -1 {
		return BootConfig{}, errors.New("port must be greater than or equal to 0")
	}

	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return BootConfig{}, err
	}
	path := resolution.Path

	cfg, err := opts.read(path)
	if err == nil {
		host, port := bootServer(host, port, firstGlobalWorkflowPath(cfg))
		return BootConfig{
			Mode:           BootModeRunning,
			Global:         cfg,
			ConfigPathRule: resolution.Rule,
			Host:           host,
			Port:           port,
			Version:        opts.version,
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
		host, port := bootServer(host, port, workflowPath)
		return BootConfig{
			Mode:           BootModeRunning,
			Global:         cfg,
			ConfigPathRule: resolution.Rule,
			WorkflowPath:   workflowPath,
			Host:           host,
			Port:           port,
			Version:        opts.version,
		}, nil
	}

	cfg, err = globalconfig.DefaultAt(path)
	if err != nil {
		return BootConfig{}, err
	}
	return BootConfig{
		Mode:           BootModeOnboarding,
		Global:         cfg,
		ConfigPathRule: resolution.Rule,
		WorkflowPath:   workflowPath,
		Host:           strings.TrimSpace(host),
		Port:           bootPort(port),
		Version:        opts.version,
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
		restoreLogger, err := redirectDefaultLogger(runtimeLogPath(cfg))
		if err != nil {
			return err
		}
		defer restoreLogger()
	}

	logger := slog.Default()
	runtimeStore, err := openRuntimeStore(runCtx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		stop()
		if err := runtimeStore.Close(); err != nil {
			logger.Warn("close runtime store failed", "error", err)
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
	projectFactory := withRunnerFactory(project.Dependencies{
		Events: events,
		Logger: logger,
	}, runtimeStore, nil)
	manager, err := project.NewManager(project.ManagerConfigFromGlobal(cfg.Global), project.ManagerDependencies{
		ProjectFactory: projectFactory,
		Events:         events,
		Logger:         logger,
	})
	if err != nil {
		return err
	}
	if err := manager.Start(runCtx); err != nil {
		return err
	}

	snapshotHub := hub.New[telemetry.Snapshot]()
	go publishSnapshots(runCtx, manager.Registry(), snapshotHub, runtimeStore, defaultSnapshotInterval, time.Now)
	server, err := web.NewServer(web.Config{
		Mode:         web.ModeRunning,
		WorkflowPath: firstWorkflowPath(cfg),
		Version:      cfg.Version,
		DashboardURL: displayURL,
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

	if useDashboard {
		if err := printBootBanner(cfg, displayURL); err != nil {
			return err
		}
		listenerOwned = false
		return serveWithTerminalDashboard(runCtx, server, listener, snapshotHub)
	}
	if err := printBootBanner(cfg, displayURL); err != nil {
		return err
	}
	listenerOwned = false
	return serve(runCtx, server, listener)
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
		DashboardURL: displayURL,
	}, web.Dependencies{})
	if err != nil {
		return err
	}

	if err := printBootBanner(cfg, displayURL); err != nil {
		return err
	}
	listenerOwned = false
	return serve(ctx, server, listener)
}

func serve(ctx context.Context, server *web.Server, listener net.Listener) error {
	errs := make(chan error, 1)
	go func() {
		errs <- server.StartListener(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errs
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return ctx.Err()
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func serveWithTerminalDashboard(ctx context.Context, server *web.Server, listener net.Listener, snapshots *hub.Hub[telemetry.Snapshot]) error {
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

	model, err := tui.NewModel(runCtx, snapshots)
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
		_, err := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(runCtx)).Run()
		errs <- err
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
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func shouldLaunchTerminalDashboard(cfg BootConfig) bool {
	return cfg.Mode == BootModeRunning && cfg.StdoutTTY && !cfg.Headless
}

func redirectDefaultLogger(path string) (func(), error) {
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
		Level: logLevelFromEnv(),
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

func openRuntimeStore(ctx context.Context, cfg BootConfig) (store.Store, error) {
	return store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    runtimeStorePath(cfg),
	})
}

func runtimeStorePath(cfg BootConfig) string {
	path := filepath.Join(filepath.Dir(cfg.Global.Path), "symphony.db")
	if strings.TrimSpace(cfg.Global.Path) == "" {
		path = filepath.Join(mustGetwd(), ".symphony", "symphony.db")
	}
	return path
}

func runtimeLogPath(cfg BootConfig) string {
	return filepath.Join(filepath.Dir(runtimeStorePath(cfg)), "symphony.log")
}

func logLevelFromEnv() slog.Level {
	for _, key := range []string{"SYMPHONY_LOG_LEVEL", "LOG_LEVEL"} {
		if level, ok := os.LookupEnv(key); ok {
			return parseSlogLevel(level)
		}
	}
	return slog.LevelInfo
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
	for _, existing := range operations {
		if existing == operation {
			return true
		}
	}
	return false
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
	if len(cfg.Projects) == 0 {
		return ""
	}
	return cfg.Projects[0].Workflow
}

func bootServer(host string, port int, workflowPath string) (string, *int) {
	resolvedHost := strings.TrimSpace(host)
	resolvedPort := bootPort(port)

	workflowPath = strings.TrimSpace(workflowPath)
	if workflowPath == "" || (resolvedHost != "" && resolvedPort != nil) {
		return resolvedHost, resolvedPort
	}

	workflow, err := workflowconfig.LoadWorkflow(workflowPath)
	if err != nil || workflow.Config.Validate() != nil {
		return resolvedHost, resolvedPort
	}
	if resolvedHost == "" {
		resolvedHost = strings.TrimSpace(workflow.Config.Server.Host)
	}
	if resolvedPort == nil {
		resolvedPort = workflow.Config.Server.Port
	}
	return resolvedHost, resolvedPort
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
	port := defaultWebPort
	if cfg.Port != nil {
		port = *cfg.Port
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
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
	_, err := io.WriteString(out, bootBanner(cfg.Version, displayURL))
	return err
}

func bootBanner(version string, displayURL string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	displayURL = strings.TrimSpace(displayURL)
	if displayURL == "" {
		displayURL = "http://" + net.JoinHostPort(dashboardHost, strconv.Itoa(defaultWebPort))
	}

	var out strings.Builder
	out.WriteString("Symphony ")
	out.WriteString(version)
	out.WriteByte('\n')
	out.WriteString("Project: ")
	out.WriteString(projectURL)
	out.WriteByte('\n')
	out.WriteString("Dashboard: ")
	out.WriteString(displayURL)
	out.WriteByte('\n')
	return out.String()
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
