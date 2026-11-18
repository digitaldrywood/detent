package remoterunner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

type Manager interface {
	Reconcile(context.Context, project.ManagerConfig) (project.ReconcileResult, error)
}

type ConfigOwner struct {
	apply        sync.Mutex
	mu           sync.RWMutex
	current      globalconfig.Config
	applyRuntime func(globalconfig.Config) error
	manager      Manager
	syncDispatch func([]globalconfig.Project)
}

func NewConfigOwner(current globalconfig.Config, applyRuntime func(globalconfig.Config) error) *ConfigOwner {
	return &ConfigOwner{current: current, applyRuntime: applyRuntime}
}

func (o *ConfigOwner) Attach(manager Manager, syncDispatch func([]globalconfig.Project)) {
	o.apply.Lock()
	defer o.apply.Unlock()
	o.manager, o.syncDispatch = manager, syncDispatch
}

func (o *ConfigOwner) Current() globalconfig.Config {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.current
}

func (o *ConfigOwner) ApplyProjects(ctx context.Context, routed globalconfig.Config) error {
	o.apply.Lock()
	defer o.apply.Unlock()
	next := o.Current()
	next.Projects = slices.Clone(routed.Projects)
	next.Client.NativeProjects = maps.Clone(routed.Client.NativeProjects)
	_, err := o.applyLocked(ctx, next)
	return err
}

func (o *ConfigOwner) ApplyFile(ctx context.Context, next globalconfig.Config) (project.ReconcileResult, error) {
	o.apply.Lock()
	defer o.apply.Unlock()
	current := o.Current()
	next.Projects = current.Projects
	next.Client.NativeProjects = current.Client.NativeProjects
	return o.applyLocked(ctx, next)
}

func (o *ConfigOwner) applyLocked(ctx context.Context, next globalconfig.Config) (project.ReconcileResult, error) {
	if o.manager == nil {
		return project.ReconcileResult{}, errors.New("runner project manager is unavailable")
	}
	previous := o.Current()
	if err := o.applyRuntime(next); err != nil {
		return project.ReconcileResult{}, err
	}
	result, err := o.manager.Reconcile(ctx, project.ManagerConfigFromGlobal(next))
	if err != nil {
		return result, errors.Join(fmt.Errorf("reconcile runner projects: %w", err), o.applyRuntime(previous))
	}
	if o.syncDispatch != nil {
		o.syncDispatch(next.Projects)
	}
	o.mu.Lock()
	o.current = next
	o.mu.Unlock()
	return result, nil
}
