package hookproc

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const hookCleanupTimeout = 5 * time.Second

type HookExitUnverifiedError struct {
	Err error
}

func (e *HookExitUnverifiedError) Error() string {
	return "workspace hook process exit unverified: " + e.Err.Error()
}

func (e *HookExitUnverifiedError) Unwrap() error { return e.Err }

var ErrWorkspacePreserved = errors.New("workspace retained for recovery")

type Registry struct {
	hooks  map[string]map[*Owner]struct{}
	failed map[string]struct{}
	mu     sync.Mutex
	paths  map[string]chan struct{}
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

func hookLifetimeFromContext(ctx context.Context) *hookLifetime {
	if lifetime, ok := ctx.Value(hookLifetimeKey{}).(*hookLifetime); ok {
		return lifetime
	}
	return nil
}

type Owner struct {
	lock       chan struct{}
	running    bool
	closed     bool
	descriptor *Lifetime
	finish     func(context.Context) error
	uses       *Registry
	path       string
	token      string
	lifetime   *hookLifetime
	setup      *workspaceSetup
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
			_, err := owner.stop(context.WithoutCancel(ctx))
			result = errors.Join(result, err)
		}
		return result
	}
}

func (u *Registry) Start(ctx context.Context, path string) (*Owner, error) {
	if u == nil || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		return nil, nil
	}
	lifetime := hookLifetimeFromContext(ctx)
	setup, _ := ctx.Value(workspaceSetupKey{}).(*workspaceSetup)
	parent := ""
	if setup != nil {
		parent = filepath.Join(setup.root, pendingWorkspaceSetupPath(path))
	}
	descriptor, err := newLifetime(parent)
	if err != nil {
		return nil, fmt.Errorf("open hook lifetime: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	owner := &Owner{lock: make(chan struct{}, 1), uses: u, path: path, token: rand.Text(), lifetime: lifetime, descriptor: descriptor, running: true, setup: setup}
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
	lifetime := hookLifetimeFromContext(ctx)
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
	return u.reap(context.WithoutCancel(ctx), path)
}

func (u *Registry) reap(ctx context.Context, path string) (int, error) {
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

func (u *Registry) StopBounded(ctx context.Context, path string) error {
	_, err := u.reap(ctx, path)
	return err
}

func (o *Owner) CommandFinished(ctx context.Context, commandErr error) error {
	if o == nil {
		return nil
	}
	o.lock <- struct{}{}
	defer func() { <-o.lock }()
	o.running = false
	if commandErr != nil {
		_, err := o.stopLocked(context.WithoutCancel(ctx))
		return err
	}
	cleanup := context.WithoutCancel(ctx)
	discovery, cancel := context.WithTimeout(cleanup, hookCleanupTimeout)
	active, err := o.descriptor.Capture(discovery, o.path, o.token)
	cancel()
	if err != nil {
		return &HookExitUnverifiedError{Err: err}
	}
	if active {
		if o.setup != nil {
			if err := recordWorkspaceSetupReady(o.setup.root, o.path, o.descriptor); err != nil {
				return err
			}
			o.setup.retained.Store(true)
		}
		return nil
	}
	return o.release(cleanup)
}

func (o *Owner) stop(ctx context.Context) (int, error) {
	select {
	case o.lock <- struct{}{}:
		defer func() { <-o.lock }()
	case <-ctx.Done():
		return 0, &HookExitUnverifiedError{Err: ctx.Err()}
	}
	if err := ctx.Err(); err != nil {
		return 0, &HookExitUnverifiedError{Err: err}
	}
	return o.stopLocked(ctx)
}

func (o *Owner) stopLocked(ctx context.Context) (int, error) {
	if o.closed {
		return 0, nil
	}
	if o.running {
		return 0, &HookExitUnverifiedError{Err: ErrWorkspacePreserved}
	}
	count, err := o.descriptor.Reap(ctx, o.path, o.token)
	if err != nil {
		return count, &HookExitUnverifiedError{Err: err}
	}
	if err := o.descriptor.WaitClosed(ctx); err != nil {
		return count, &HookExitUnverifiedError{Err: err}
	}
	return count, o.release(ctx)
}

func (o *Owner) release(ctx context.Context) error {
	if o.finish != nil {
		cleanup, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := o.finish(cleanup); err != nil {
			return err
		}
		o.finish = nil
	}
	if o.setup != nil && o.setup.retained.Load() {
		if err := ClearWorkspaceSetupPending(o.setup.root, o.path); err != nil {
			return err
		}
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

func (o *Owner) ConfigureCommand(cmd *exec.Cmd) {
	if o == nil {
		return
	}
	const script = `exec 4>"$1" && /bin/chmod 400 "$1" || exit 1; shift; exec "$@"`
	cmd.Args = append([]string{"sh", "-c", script, "sh", o.descriptor.path(), cmd.Path}, cmd.Args[1:]...)
	cmd.Path = "/bin/sh"
}

func (o *Owner) OnExit(finish func(context.Context) error) { o.finish = finish }

func (u *Registry) SetSetupFailed(path string, failed bool) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if failed {
		if u.failed == nil {
			u.failed = make(map[string]struct{})
		}
		u.failed[path] = struct{}{}
	} else {
		delete(u.failed, path)
	}
}

func (u *Registry) setupFailed(path string) bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	_, failed := u.failed[path]
	return failed
}

func (u *Registry) ownsRetainedSetup(path string) bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for owner := range u.hooks[path] {
		if owner.setup != nil && owner.setup.retained.Load() {
			return true
		}
	}
	return false
}
