package hookproc

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type HookExitUnverifiedError struct {
	Err error
}

func (e *HookExitUnverifiedError) Error() string {
	return "workspace hook process exit unverified: " + e.Err.Error()
}

func (e *HookExitUnverifiedError) Unwrap() error { return e.Err }

var ErrWorkspacePreserved = errors.New("workspace retained for recovery")

type Registry struct {
	hooks map[string]map[*Owner]struct{}
	mu    sync.Mutex
	paths map[string]chan struct{}
}

func (u *Registry) permit(key string) chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.paths == nil {
		u.paths = make(map[string]chan struct{})
	}
	permit := u.paths[key]
	if permit == nil {
		permit = make(chan struct{}, 1)
		u.paths[key] = permit
	}
	return permit
}

func (u *Registry) Use(ctx context.Context, key string) (func(), error) {
	permit := u.permit(key)
	select {
	case permit <- struct{}{}:
		return func() { <-permit }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (u *Registry) TryCleanup(key string) (func(), bool) {
	permit := u.permit(key)
	select {
	case permit <- struct{}{}:
		return func() { <-permit }, true
	default:
		return nil, false
	}
}

type hookLifetime struct {
	mu        sync.Mutex
	processes map[*Owner]struct{}
	finished  atomic.Bool
}

type hookLifetimeKey struct{}

type Owner struct {
	mu         sync.Mutex
	running    bool
	closed     bool
	descriptor *Lifetime
	finish     func(context.Context) error
	uses       *Registry
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

func (u *Registry) Start(ctx context.Context, path string) (*Owner, error) {
	if u == nil || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return nil, nil
	}
	lifetime, _ := ctx.Value(hookLifetimeKey{}).(*hookLifetime)
	descriptor, err := NewLifetime()
	if err != nil {
		return nil, fmt.Errorf("open hook lifetime: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	owner := &Owner{uses: u, path: path, token: rand.Text(), lifetime: lifetime, descriptor: descriptor, running: true}
	if u.hooks == nil {
		u.hooks = make(map[string]map[*Owner]struct{})
	}
	if u.hooks[path] == nil {
		u.hooks[path] = make(map[*Owner]struct{})
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
			lifetime.processes = make(map[*Owner]struct{})
		}
		lifetime.processes[owner] = struct{}{}
		lifetime.mu.Unlock()
	}
	return owner, nil
}

func (u *Registry) removable(ctx context.Context, path string) ([]*Owner, error) {
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

func (u *Registry) Reap(ctx context.Context, path string) (int, error) {
	owners, err := u.removable(ctx, path)
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

func (u *Registry) Stop(ctx context.Context, path string) error {
	_, err := u.Reap(ctx, path)
	return err
}

func (o *Owner) CommandFinished(ctx context.Context, commandErr error) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.running = false
	if err := o.descriptor.CloseWriter(); err != nil {
		return err
	}
	if commandErr != nil {
		_, err := o.stopLocked(ctx)
		return err
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	active, err := o.descriptor.Capture(cleanup, o.path, o.token)
	if err != nil {
		return &HookExitUnverifiedError{Err: err}
	}
	if active {
		return nil
	}
	return o.release(cleanup)
}

func (o *Owner) stop(ctx context.Context) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stopLocked(ctx)
}

func (o *Owner) stopLocked(ctx context.Context) (int, error) {
	if o.closed {
		return 0, nil
	}
	if o.running {
		return 0, &HookExitUnverifiedError{Err: ErrWorkspacePreserved}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	count, err := o.descriptor.Reap(cleanup, o.path, o.token)
	if err != nil {
		return count, &HookExitUnverifiedError{Err: err}
	}
	if err := o.descriptor.WaitClosed(cleanup); err != nil {
		return count, &HookExitUnverifiedError{Err: err}
	}
	return count, o.release(cleanup)
}

func (o *Owner) release(ctx context.Context) error {
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

func (u *Registry) CheckRemoval(ctx context.Context, path string) error {
	_, err := u.removable(ctx, path)
	return err
}

func (o *Owner) Token() string { return o.token }

func (o *Owner) Attach(cmd *exec.Cmd) {
	if o != nil {
		o.descriptor.Attach(cmd)
	}
}

func (o *Owner) OnExit(finish func(context.Context) error) { o.finish = finish }
