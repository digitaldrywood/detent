package cli

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"time"

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
	applyRuntime       func(globalconfig.Config) error
	onReload           func(globalconfig.Config)
}

type globalConfigChange struct {
	Field           string
	RequiresRestart bool
	Old             any
	New             any
}

func startGlobalConfigWatcher(
	ctx context.Context,
	current globalconfig.Config,
	manager globalConfigManager,
	logger *slog.Logger,
	githubToken *runtimeGitHubTokenState,
	applyRuntime func(globalconfig.Config) error,
	onReload func(globalconfig.Config),
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
		current:      current,
		manager:      manager,
		logger:       logger,
		githubToken:  githubToken,
		applyRuntime: applyRuntime,
		onReload:     onReload,
	}
	syncLatestGlobalConfig(ctx, path, reloader)
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

func syncLatestGlobalConfig(ctx context.Context, path string, reloader *globalConfigReloader) {
	if reloader == nil {
		return
	}
	latest, err := readGlobalConfig(path)
	update := configwatcher.FileUpdate[globalconfig.Config]{
		Path:  path,
		Value: latest,
		Err:   err,
		At:    time.Now(),
	}
	if err == nil && reflect.DeepEqual(reloader.current, latest) {
		return
	}
	reloader.handle(ctx, update)
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

	logGlobalConfigChanges(logger, previous, r.current)
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
	if r.applyRuntime != nil {
		if err := r.applyRuntime(update.Value); err != nil {
			if r.githubToken != nil {
				r.githubToken.set(previousGitHubToken)
			}
			return project.ReconcileResult{}, err
		}
	}
	result, err := r.manager.Reconcile(ctx, managerConfig)
	if err != nil {
		if r.applyRuntime != nil {
			err = errors.Join(err, r.applyRuntime(r.current))
		}
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
	return resolveGlobalRuntimeGitHubTokenWithCommandContext(ctx, cfg, context.WithTimeout)
}

func resolveGlobalRuntimeGitHubTokenWithCommandContext(
	ctx context.Context,
	cfg globalconfig.Config,
	commandContext runtimeCommandContextFactory,
) (string, error) {
	deps := runtimeDeps{}.withDefaults()
	if strings.TrimSpace(cfg.GitHubToken) != "" {
		deps.lookupEnv = withoutRuntimeGitHubTokenEnv(deps.lookupEnv)
		if githubTokenSentinel(cfg.GitHubToken) {
			deps.ghAuthToken = func(ctx context.Context) (string, error) {
				return runGHAuthTokenWithCommandContext(
					ctx,
					environmentWithoutKeys([]string{"GITHUB_TOKEN"}),
					commandContext,
				)
			}
		}
	}
	token, _, err := resolveRuntimeGitHubToken(ctx, &cfg, deps)
	if err != nil {
		return "", err
	}
	return runtimeGlobalGitHubToken(token), nil
}

func withoutRuntimeGitHubTokenEnv(lookupEnv func(string) string) func(string) string {
	return func(key string) string {
		if key == "GITHUB_TOKEN" {
			return ""
		}
		return lookupEnv(key)
	}
}

func managerConfigWithRuntimeGitHubToken(cfg globalconfig.Config, token string) project.ManagerConfig {
	managerConfig := project.ManagerConfigFromGlobal(cfg)
	managerConfig.RuntimeCredentialVersion = runtimeGitHubTokenVersion(token)
	return managerConfig
}

func logGlobalConfigChanges(logger *slog.Logger, previous globalconfig.Config, next globalconfig.Config) {
	for _, change := range changedGlobalConfigFields(previous, next) {
		if change.RequiresRestart {
			logger.Warn("global config setting change requires restart", "field", change.Field)
			continue
		}
		logger.Info("global config setting reloaded", "field", change.Field, "old", change.Old, "new", change.New)
	}
}

func changedGlobalConfigFields(previous globalconfig.Config, next globalconfig.Config) []globalConfigChange {
	fields := []globalConfigChange{}
	if previous.Env != next.Env {
		fields = append(fields, globalConfigChange{Field: "env", RequiresRestart: true, Old: previous.Env, New: next.Env})
	}
	if previous.LogLevel != next.LogLevel {
		fields = append(fields, globalConfigChange{Field: "log_level", Old: previous.LogLevel, New: next.LogLevel})
	}
	if !sameOptionalInt(previous.LogMaxSizeBytes, next.LogMaxSizeBytes) {
		fields = append(fields, globalConfigChange{Field: "log_max_size_bytes", RequiresRestart: true})
	}
	if !sameOptionalInt(previous.LogMaxBackups, next.LogMaxBackups) {
		fields = append(fields, globalConfigChange{Field: "log_max_backups", RequiresRestart: true})
	}
	if previous.GitHubToken != next.GitHubToken {
		fields = append(fields, globalConfigChange{Field: "github_token", Old: "<redacted>", New: "<redacted>"})
	}
	if previous.TrustLoopbackPeerRead != next.TrustLoopbackPeerRead {
		fields = append(fields, globalConfigChange{Field: "trust_loopback_peer_read", Old: previous.TrustLoopbackPeerRead, New: next.TrustLoopbackPeerRead})
	}
	if previous.DashboardAccess.Mode != next.DashboardAccess.Mode {
		fields = append(fields, globalConfigChange{Field: "dashboard_access.mode", Old: previous.DashboardAccess.Mode, New: next.DashboardAccess.Mode})
	}
	if previous.DashboardAccess.Token != next.DashboardAccess.Token {
		fields = append(fields, globalConfigChange{Field: "dashboard_access.token", Old: "<redacted>", New: "<redacted>"})
	}
	if previous.DashboardAccess.AllowWrite != next.DashboardAccess.AllowWrite {
		fields = append(fields, globalConfigChange{Field: "dashboard_access.allow_write", Old: previous.DashboardAccess.AllowWrite, New: next.DashboardAccess.AllowWrite})
	}
	if !sameOptionalInt(previous.Port, next.Port) {
		fields = append(fields, globalConfigChange{Field: "port", RequiresRestart: true})
	}
	if previous.InstanceName != next.InstanceName {
		fields = append(fields, globalConfigChange{Field: "instance_name", RequiresRestart: true})
	}
	if !reflect.DeepEqual(previous.Notifications, next.Notifications) {
		fields = append(fields, globalConfigChange{Field: "notifications", RequiresRestart: true})
	}
	if !reflect.DeepEqual(previous.Auth, next.Auth) {
		fields = append(fields, globalConfigChange{Field: "auth", RequiresRestart: true})
	}
	if !reflect.DeepEqual(previous.Ops, next.Ops) {
		fields = append(fields, globalConfigChange{Field: "ops.tmux_window_status", RequiresRestart: true})
	}
	if !reflect.DeepEqual(previous.Projects, next.Projects) {
		fields = append(fields, globalConfigChange{Field: "projects", Old: globalProjectIDs(previous.Projects), New: globalProjectIDs(next.Projects)})
	}
	fields = append(fields, changedGlobalSettings(previous.Global, next.Global)...)
	return fields
}

func changedGlobalSettings(previous globalconfig.Settings, next globalconfig.Settings) []globalConfigChange {
	fields := []globalConfigChange{}
	if previous.MaxConcurrentAgents != next.MaxConcurrentAgents {
		fields = append(fields, globalConfigChange{Field: "global.max_concurrent_agents", Old: previous.MaxConcurrentAgents, New: next.MaxConcurrentAgents})
	}
	if previous.Scheduling != next.Scheduling {
		fields = append(fields, globalConfigChange{Field: "global.scheduling", Old: previous.Scheduling, New: next.Scheduling})
	}
	if previous.RateWindowPacing != next.RateWindowPacing {
		fields = append(fields, globalConfigChange{Field: "global.rate_window_pacing", Old: previous.RateWindowPacing, New: next.RateWindowPacing})
	}
	if !reflect.DeepEqual(previous.AgentPools, next.AgentPools) {
		fields = append(fields, globalConfigChange{Field: "global.agent_pools", Old: previous.AgentPools, New: next.AgentPools})
	}
	if !reflect.DeepEqual(previous.ActiveHours, next.ActiveHours) {
		fields = append(fields, globalConfigChange{Field: "global.active_hours", Old: previous.ActiveHours, New: next.ActiveHours})
	}
	if !reflect.DeepEqual(previous.Identity, next.Identity) {
		fields = append(fields, globalConfigChange{Field: "global.identity", RequiresRestart: true})
	}
	if !reflect.DeepEqual(previous.FairShare, next.FairShare) {
		fields = append(fields, globalConfigChange{Field: "global.fair_share", Old: previous.FairShare, New: next.FairShare})
	}
	if !reflect.DeepEqual(previous.Startup, next.Startup) {
		fields = append(fields, globalConfigChange{Field: "global.startup", Old: previous.Startup, New: next.Startup})
	}
	return fields
}

func globalProjectIDs(projects []globalconfig.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, cfg := range projects {
		ids = append(ids, cfg.ID)
	}
	return ids
}

func sameOptionalInt(left *int, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func projectIDs(ids []project.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}
