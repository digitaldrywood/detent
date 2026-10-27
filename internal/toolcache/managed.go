package toolcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Managed struct {
	root string
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
	lock, err := cache.lock(ctx, "admission", false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	marker := filepath.Join(root, ".detent-owner")
	existing, err := os.ReadFile(marker)
	if err == nil {
		if strings.TrimSpace(string(existing)) != filepath.Clean(owner) {
			return nil, errors.New("managed Go build cache belongs to another runtime store")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(marker, []byte(filepath.Clean(owner)+"\n"), 0o600); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	return cache, nil
}

func ManagedBuildPath() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "detent", "go-build"), nil
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
	queue, err := m.lock(ctx, "admission", false)
	if err != nil {
		return ctx, nil, err
	}
	usage, err := m.lock(ctx, "consumers", true)
	err = errors.Join(err, queue.Close())
	if err != nil {
		if usage != nil {
			err = errors.Join(err, usage.Close())
		}
		return ctx, nil, err
	}
	return context.WithValue(m.WithContext(ctx), consumerContextKey{}, m.root), sync.OnceValue(usage.Close), nil
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
