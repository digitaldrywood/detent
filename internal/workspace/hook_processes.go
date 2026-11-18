package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
)

type hookLifetime struct {
	mu        sync.Mutex
	processes map[*hookOwner]struct{}
	finished  atomic.Bool
}

type hookLifetimeKey struct{}

type hookOwner struct {
	mu         sync.Mutex
	running    bool
	closed     bool
	descriptor *hookproc.Lifetime
	finish     func(context.Context) error
	uses       *workspaceUses
	path       string
	token      string
	lifetime   *hookLifetime
}

func WithHookProcesses(ctx context.Context) (context.Context, func() error) {
	lifetime := &hookLifetime{}
	ctx = context.WithValue(ctx, hookLifetimeKey{}, lifetime)
	return ctx, func() error {
		lifetime.finished.Store(true)
		lifetime.mu.Lock()
		processes := slices.Collect(maps.Keys(lifetime.processes))
		lifetime.mu.Unlock()
		var result error
		for _, owner := range processes {
			_, err := owner.stop(ctx)
			result = errors.Join(result, err)
		}
		return result
	}
}

func (u *workspaceUses) hookOwner(ctx context.Context, path string) (*hookOwner, error) {
	if u == nil || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return nil, nil
	}
	lifetime, _ := ctx.Value(hookLifetimeKey{}).(*hookLifetime)
	descriptor, err := hookproc.NewLifetime()
	if err != nil {
		return nil, fmt.Errorf("open hook lifetime: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	owner := &hookOwner{uses: u, path: path, token: rand.Text(), lifetime: lifetime, descriptor: descriptor, running: true}
	if u.hooks == nil {
		u.hooks = make(map[string]map[*hookOwner]struct{})
	}
	if u.hooks[path] == nil {
		u.hooks[path] = make(map[*hookOwner]struct{})
	}
	u.hooks[path][owner] = struct{}{}
	if lifetime != nil {
		lifetime.mu.Lock()
		if lifetime.finished.Load() {
			lifetime.mu.Unlock()
			delete(u.hooks[path], owner)
			return nil, errors.Join(ErrWorkspacePreserved, descriptor.Close())
		}
		if lifetime.processes == nil {
			lifetime.processes = make(map[*hookOwner]struct{})
		}
		lifetime.processes[owner] = struct{}{}
		lifetime.mu.Unlock()
	}
	return owner, nil
}

func (u *workspaceUses) removableHooks(ctx context.Context, path string) ([]*hookOwner, error) {
	if u == nil {
		return nil, nil
	}
	lifetime, _ := ctx.Value(hookLifetimeKey{}).(*hookLifetime)
	u.mu.Lock()
	defer u.mu.Unlock()
	for owner := range u.hooks[path] {
		if owner.lifetime != nil && owner.lifetime != lifetime && !owner.lifetime.finished.Load() {
			return nil, fmt.Errorf("%w: another attempt owns workspace hooks", ErrWorkspacePreserved)
		}
	}
	return slices.Collect(maps.Keys(u.hooks[path])), nil
}

func (u *workspaceUses) reapHooks(ctx context.Context, path string) (int, error) {
	owners, err := u.removableHooks(ctx, path)
	if err != nil {
		return 0, err
	}
	var total int
	var result error
	for _, owner := range owners {
		n, err := owner.stop(ctx)
		total += n
		result = errors.Join(result, err)
	}
	return total, result
}

func (u *workspaceUses) stopHooks(ctx context.Context, path string) error {
	_, err := u.reapHooks(ctx, path)
	return err
}

func (o *hookOwner) commandFinished(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.running = false
	if err := o.descriptor.CloseWriter(); err != nil {
		return err
	}
	if o.finish != nil {
		return nil
	}
	active, err := o.descriptor.Active()
	if err != nil || active {
		return err
	}
	return o.release(ctx)
}

func (o *hookOwner) stop(ctx context.Context) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, nil
	}
	if o.running {
		return 0, ErrWorkspacePreserved
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	active, err := o.descriptor.Active()
	if err != nil {
		return 0, err
	}
	var count int
	if active {
		count, err = o.descriptor.Reap(cleanup, o.path, o.token)
		if err != nil {
			return count, fmt.Errorf("stop workspace hook processes: %w", err)
		}
		if err := o.descriptor.WaitClosed(cleanup); err != nil {
			return count, err
		}
	}
	return count, o.release(cleanup)
}

func (o *hookOwner) release(ctx context.Context) error {
	if o.finish != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := o.finish(cleanup); err != nil {
			return err
		}
		o.finish = nil
	}
	if err := o.descriptor.Close(); err != nil {
		return err
	}
	o.closed = true
	o.uses.mu.Lock()
	defer o.uses.mu.Unlock()
	delete(o.uses.hooks[o.path], o)
	if len(o.uses.hooks[o.path]) == 0 {
		delete(o.uses.hooks, o.path)
	}
	if o.lifetime != nil {
		o.lifetime.mu.Lock()
		delete(o.lifetime.processes, o)
		o.lifetime.mu.Unlock()
	}
	return nil
}
