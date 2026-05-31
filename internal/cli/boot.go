package cli

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony-go/internal/config"
	globalconfig "github.com/digitaldrywood/symphony-go/internal/config/global"
	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/connector/memory"
	"github.com/digitaldrywood/symphony-go/internal/hub"
	"github.com/digitaldrywood/symphony-go/internal/project"
	"github.com/digitaldrywood/symphony-go/internal/store"
	"github.com/digitaldrywood/symphony-go/internal/telemetry"
	"github.com/digitaldrywood/symphony-go/internal/web"
)

const (
	defaultWorkflowFile = "WORKFLOW.md"
	defaultProjectID    = "default"
	defaultWebHost      = "127.0.0.1"
	defaultWebPort      = 4000
)

func resolveBootConfig(configPath string, host string, port int, opts options) (BootConfig, error) {
	if port < -1 {
		return BootConfig{}, errors.New("port must be greater than or equal to 0")
	}

	path, err := resolveConfigPath(configPath, opts)
	if err != nil {
		return BootConfig{}, err
	}

	cfg, err := opts.read(path)
	if err == nil {
		return BootConfig{
			Mode:   BootModeRunning,
			Global: cfg,
			Host:   strings.TrimSpace(host),
			Port:   bootPort(port),
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
		return BootConfig{
			Mode:         BootModeRunning,
			Global:       cfg,
			WorkflowPath: workflowPath,
			Host:         strings.TrimSpace(host),
			Port:         bootPort(port),
		}, nil
	}

	cfg, err = globalconfig.DefaultAt(path)
	if err != nil {
		return BootConfig{}, err
	}
	return BootConfig{
		Mode:         BootModeOnboarding,
		Global:       cfg,
		WorkflowPath: workflowPath,
		Host:         strings.TrimSpace(host),
		Port:         bootPort(port),
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
	logger := slog.Default()
	runtimeStore, err := openRuntimeStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := runtimeStore.Close(); err != nil {
			logger.Warn("close runtime store failed", "error", err)
		}
	}()

	events := hub.New[project.Event]()
	manager, err := project.NewManager(project.ManagerConfigFromGlobal(cfg.Global), project.ManagerDependencies{
		Events: events,
		Logger: logger,
	})
	if err != nil {
		return err
	}
	if err := manager.Start(ctx); err != nil {
		return err
	}

	snapshotHub := hub.New[telemetry.Snapshot]()
	server, err := web.NewServer(web.Config{
		Mode:         web.ModeRunning,
		WorkflowPath: firstWorkflowPath(cfg),
	}, web.Dependencies{
		Hub:       snapshotHub,
		Store:     runtimeStore,
		Registry:  manager.Registry(),
		Connector: firstConnector(manager),
	})
	if err != nil {
		return err
	}

	return serve(ctx, server, serverAddr(cfg))
}

func startOnboarding(ctx context.Context, cfg BootConfig) error {
	server, err := web.NewServer(web.Config{
		Mode:         web.ModeOnboarding,
		WorkflowPath: firstWorkflowPath(cfg),
	}, web.Dependencies{})
	if err != nil {
		return err
	}

	return serve(ctx, server, serverAddr(cfg))
}

func serve(ctx context.Context, server *web.Server, addr string) error {
	errs := make(chan error, 1)
	go func() {
		errs <- server.Start(addr)
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
	path := filepath.Join(filepath.Dir(cfg.Global.Path), "symphony.db")
	if strings.TrimSpace(cfg.Global.Path) == "" {
		path = filepath.Join(mustGetwd(), ".symphony", "symphony.db")
	}
	return store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    path,
	})
}

func firstConnector(manager *project.Manager) connector.Connector {
	for _, project := range manager.Registry().List() {
		if projectConnector := project.Connector(); projectConnector != nil {
			return projectConnector
		}
	}
	return memory.New(memory.Config{})
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

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
