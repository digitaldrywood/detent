package cli

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/project"
)

var errMissingGlobalConfigManager = errors.New("global config reload manager is required")

type globalConfigManager interface {
	Reconcile(context.Context, project.ManagerConfig) (project.ReconcileResult, error)
}

type globalConfigReloader struct {
	current globalconfig.Config
	manager globalConfigManager
	logger  *slog.Logger
}

func startGlobalConfigWatcher(
	ctx context.Context,
	current globalconfig.Config,
	manager globalConfigManager,
	logger *slog.Logger,
) <-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}

	path := strings.TrimSpace(current.Path)
	if path == "" {
		logger.Warn("global config watcher skipped", "error", "global config path is empty")
		return nil
	}

	watcher, err := configwatcher.NewFile(path, readGlobalConfig, configwatcher.WithFileLogger(logger))
	if err != nil {
		logger.Warn("create global config watcher failed", "path", path, "error", err)
		return nil
	}
	updates, err := watcher.Watch(ctx)
	if err != nil {
		logger.Warn("watch global config failed", "path", path, "error", err)
		return nil
	}

	done := make(chan struct{})
	reloader := &globalConfigReloader{
		current: current,
		manager: manager,
		logger:  logger,
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				reloader.handle(ctx, update)
			}
		}
	}()
	return done
}

func readGlobalConfig(path string) (globalconfig.Config, error) {
	return globalconfig.Read(path)
}

func waitGlobalConfigWatcher(done <-chan struct{}) {
	if done != nil {
		<-done
	}
}

func (r *globalConfigReloader) handle(ctx context.Context, update configwatcher.FileUpdate[globalconfig.Config]) {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}

	previous := r.current
	result, err := r.apply(ctx, update)
	if err != nil {
		logger.Warn("global config reload failed", "path", update.Path, "error", err)
		return
	}

	logGlobalSettingChanges(logger, previous.Global, r.current.Global)
	logger.Info("global config reloaded",
		"path", update.Path,
		"added_projects", projectIDs(result.Added),
		"removed_projects", projectIDs(result.Removed),
		"changed_projects", projectIDs(result.Changed),
		"unchanged_projects", projectIDs(result.Unchanged),
	)
}

func (r *globalConfigReloader) apply(
	ctx context.Context,
	update configwatcher.FileUpdate[globalconfig.Config],
) (project.ReconcileResult, error) {
	if update.Err != nil {
		return project.ReconcileResult{}, update.Err
	}
	if r.manager == nil {
		return project.ReconcileResult{}, errMissingGlobalConfigManager
	}

	result, err := r.manager.Reconcile(ctx, project.ManagerConfigFromGlobal(update.Value))
	if err != nil {
		return result, err
	}

	r.current = update.Value
	return result, nil
}

func logGlobalSettingChanges(logger *slog.Logger, previous globalconfig.Settings, next globalconfig.Settings) {
	for _, field := range changedGlobalSettings(previous, next) {
		switch field {
		case "global.startup":
			logger.Info("global config setting reloaded", "field", field)
		default:
			logger.Warn("global config setting change requires restart", "field", field)
		}
	}
}

func changedGlobalSettings(previous globalconfig.Settings, next globalconfig.Settings) []string {
	fields := []string{}
	if previous.MaxConcurrentAgents != next.MaxConcurrentAgents {
		fields = append(fields, "global.max_concurrent_agents")
	}
	if previous.Scheduling != next.Scheduling {
		fields = append(fields, "global.scheduling")
	}
	if !reflect.DeepEqual(previous.FairShare, next.FairShare) {
		fields = append(fields, "global.fair_share")
	}
	if !reflect.DeepEqual(previous.Startup, next.Startup) {
		fields = append(fields, "global.startup")
	}
	return fields
}

func projectIDs(ids []project.ProjectID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}
