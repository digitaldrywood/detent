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
	current            globalconfig.Config
	manager            globalConfigManager
	logger             *slog.Logger
	githubToken        *runtimeGitHubTokenState
	resolveGitHubToken func(context.Context, globalconfig.Config) (string, error)
	onReload           func(globalconfig.Config)
}

func startGlobalConfigWatcher(
	ctx context.Context,
	current globalconfig.Config,
	manager globalConfigManager,
	logger *slog.Logger,
	githubToken *runtimeGitHubTokenState,
	onReload ...func(globalconfig.Config),
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
		current:     current,
		manager:     manager,
		logger:      logger,
		githubToken: githubToken,
	}
	if len(onReload) > 0 {
		reloader.onReload = onReload[0]
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

	previousGitHubToken := ""
	nextGitHubToken := ""
	if r.githubToken != nil {
		resolvedGitHubToken, err := r.runtimeGitHubToken(ctx, update.Value)
		if err != nil {
			return project.ReconcileResult{}, err
		}
		nextGitHubToken = resolvedGitHubToken
		previousGitHubToken = r.githubToken.get()
		r.githubToken.set(nextGitHubToken)
	}

	managerConfig := managerConfigWithRuntimeGitHubToken(update.Value, nextGitHubToken)
	result, err := r.manager.Reconcile(ctx, managerConfig)
	if err != nil {
		if r.githubToken != nil {
			r.githubToken.set(previousGitHubToken)
		}
		return result, err
	}

	r.current = update.Value
	if r.onReload != nil {
		r.onReload(update.Value)
	}
	return result, nil
}

func (r *globalConfigReloader) runtimeGitHubToken(ctx context.Context, cfg globalconfig.Config) (string, error) {
	if r.resolveGitHubToken != nil {
		return r.resolveGitHubToken(ctx, cfg)
	}
	return resolveGlobalRuntimeGitHubToken(ctx, cfg)
}

func resolveGlobalRuntimeGitHubToken(ctx context.Context, cfg globalconfig.Config) (string, error) {
	deps := runtimeDeps{}.withDefaults()
	token, _, err := resolveRuntimeGitHubToken(ctx, &cfg, deps)
	if err != nil {
		return "", err
	}
	return runtimeGlobalGitHubToken(token), nil
}

func managerConfigWithRuntimeGitHubToken(cfg globalconfig.Config, token string) project.ManagerConfig {
	managerConfig := project.ManagerConfigFromGlobal(cfg)
	managerConfig.RuntimeCredentialVersion = runtimeGitHubTokenVersion(token)
	return managerConfig
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
	if !reflect.DeepEqual(previous.Identity, next.Identity) {
		fields = append(fields, "global.identity")
	}
	if !reflect.DeepEqual(previous.FairShare, next.FairShare) {
		fields = append(fields, "global.fair_share")
	}
	if !reflect.DeepEqual(previous.Startup, next.Startup) {
		fields = append(fields, "global.startup")
	}
	return fields
}

func projectIDs(ids []project.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}
