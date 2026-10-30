package toolcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Managed struct {
	root    string
	program string
}

type managedContextKey struct{}
type consumerContextKey struct{}
type waitObserverContextKey struct{}

func (m *Managed) WithContext(ctx context.Context) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, managedContextKey{}, m)
}

func WithWaitObserver(ctx context.Context, observe func(string, bool)) context.Context {
	return context.WithValue(ctx, waitObserverContextKey{}, observe)
}

func UseFromContext(ctx context.Context) (context.Context, func() error, error) {
	cache, ok := ctx.Value(managedContextKey{}).(*Managed)
	if !ok {
		return ctx, func() error { return nil }, nil
	}
	return cache.Use(ctx)
}

func NewManaged(ctx context.Context, root, owner string) (cache *Managed, err error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(owner) {
		return nil, errors.New("managed Go build cache requires absolute cache and owner paths")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create managed Go build cache: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(errors.New("managed Go build cache must be an owned directory"), err)
	}
	cache = &Managed{root: filepath.Clean(root)}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve cache program: %w", err)
	}
	if strings.Contains(executable, `"`) {
		return nil, errors.New("cache program path contains a quote")
	}
	cache.program = `"` + executable + `" ` + ProgramArgument
	lock, err := cache.lock(ctx, "admission", false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	marker := filepath.Join(root, ".detent-owner")
	_, err = os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(marker, []byte(filepath.Clean(owner)+"\n"), 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return cache, nil
}

func ManagedBuildFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	cache, ok := ctx.Value(managedContextKey{}).(*Managed)
	if !ok || cache == nil {
		return ""
	}
	return cache.root
}

func (m *Managed) Use(ctx context.Context) (context.Context, func() error, error) {
	if m == nil || ctx.Value(consumerContextKey{}) == m.root {
		return ctx, func() error { return nil }, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if info, err := os.Stat(m.root); err != nil || !info.IsDir() {
		return ctx, nil, errors.Join(errors.New("managed cache unavailable"), err)
	}
	return context.WithValue(m.WithContext(ctx), consumerContextKey{}, m.root), func() error { return nil }, nil
}

func ManagedEnvironment(ctx context.Context) map[string]string {
	cache, ok := ctx.Value(managedContextKey{}).(*Managed)
	if !ok || cache == nil {
		return nil
	}
	return map[string]string{"GOCACHE": cache.root, "GOCACHEPROG": cache.program}
}

func (m *Managed) Maintain(ctx context.Context, policy Policy, now time.Time) (report Report, err error) {
	defer func() {
		if err != nil {
			report.Error = err.Error()
		}
	}()
	report.BuildPath = m.root
	busy := func() (Report, error) {
		report.Deferred = true
		report.BuildBytes, err = size(ctx, m.root)
		report.RetainedBytes = report.BuildBytes
		return report, err
	}
	queue, err := m.tryLock(ctx, "admission", false)
	if err != nil {
		return report, err
	}
	if queue == nil {
		return busy()
	}
	defer func() {
		if queue != nil {
			err = errors.Join(err, queue.Close())
		}
	}()
	usage, err := m.tryLock(ctx, "consumers", false)
	if err != nil {
		return report, err
	}
	if usage == nil {
		err = queue.Close()
		queue = nil
		if err != nil {
			return report, err
		}
		return busy()
	}
	defer func() { err = errors.Join(err, usage.Close()) }()

	_, err = trim(ctx, m.root, policy, now, &report, true)
	return report, err
}

func (m *Managed) lock(ctx context.Context, name string, shared bool) (*os.File, error) {
	waiting := false
	observe, observing := ctx.Value(waitObserverContextKey{}).(func(string, bool))
	defer func() {
		if waiting && observing && observe != nil {
			observe(name, false)
		}
	}()
	for {
		file, err := m.tryLock(ctx, name, shared)
		if err != nil || file != nil {
			return file, err
		}
		if !waiting && observing && observe != nil {
			waiting = true
			observe(name, true)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Managed) tryLock(ctx context.Context, name string, shared bool) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(m.root, ".detent-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryCacheLock(file, shared); errors.Is(err, errCacheInUse) {
		return nil, file.Close()
	} else if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

var errCacheInUse = errors.New("managed Go build cache is in use")
