package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type remoteRunnerConfig struct {
	mu       sync.Mutex
	state    *globalConfigState
	manager  globalConfigManager
	registry *project.Registry
	gate     *scheduler.PoolRegistry
	logLevel *slog.LevelVar
}

func (r *remoteRunnerConfig) applyProjects(ctx context.Context, routed globalconfig.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.state.get()
	next.Projects = slices.Clone(routed.Projects)
	next.Client.NativeProjects = maps.Clone(routed.Client.NativeProjects)
	_, err := r.apply(ctx, next)
	return err
}

func (r *remoteRunnerConfig) applyFile(ctx context.Context, next globalconfig.Config) (project.ReconcileResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.state.get()
	next.Projects = current.Projects
	next.Client.NativeProjects = current.Client.NativeProjects
	return r.apply(ctx, next)
}

func (r *remoteRunnerConfig) apply(ctx context.Context, next globalconfig.Config) (project.ReconcileResult, error) {
	if r.manager == nil {
		return project.ReconcileResult{}, errors.New("runner project manager is unavailable")
	}
	previous := r.state.get()
	if err := applyGlobalRuntimeConfig(r.gate, r.logLevel, next); err != nil {
		return project.ReconcileResult{}, err
	}
	result, err := r.manager.Reconcile(ctx, project.ManagerConfigFromGlobal(next))
	if err != nil {
		return result, errors.Join(fmt.Errorf("reconcile runner projects: %w", err), applyGlobalRuntimeConfig(r.gate, r.logLevel, previous))
	}
	syncGlobalDispatchProjects(r.gate, next.Projects, r.registry)
	r.state.set(next)
	return result, nil
}
