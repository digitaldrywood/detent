package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/toolcache"
)

type runnerBuildCache struct {
	cache  *toolcache.Managed
	root   string
	native string
	mu     sync.Mutex
	report atomic.Pointer[toolcache.Report]
}

var errNativeBuildCache = errors.New("runner uses the native Go build cache")

func newRunnerBuildCache(ctx context.Context, cfg BootConfig) (*runnerBuildCache, error) {
	if !cfg.Global.Client.Configured() || cfg.Global.Client.IdentityFile == "" || runtimeStoreIsMemory(runtimeStorePath(cfg)) || os.Getenv("GOCACHE") != "" {
		return nil, errNativeBuildCache
	}
	paths, err := toolcache.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	nativeRoot, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	if filepath.Clean(paths.Build) != filepath.Join(nativeRoot, "go-build") {
		return nil, errNativeBuildCache
	}
	root, err := toolcache.ManagedBuildPath()
	if err != nil {
		return nil, err
	}
	owner, err := filepath.EvalSymlinks(runtimeStorePath(cfg))
	if err != nil {
		return nil, err
	}
	owner, err = filepath.Abs(owner)
	if err != nil {
		return nil, err
	}
	cache, err := toolcache.NewManaged(ctx, root, owner)
	if err != nil {
		return nil, err
	}
	return &runnerBuildCache{cache: cache, root: root, native: paths.Build}, nil
}

func (c *runnerBuildCache) observe() *toolcache.Report {
	if c == nil {
		return nil
	}
	report := c.report.Load()
	if report == nil {
		return nil
	}
	copy := *report
	return &copy
}

func (c *runnerBuildCache) trim(ctx context.Context, policy toolcache.Policy, now time.Time) error {
	return c.trimForVolume(ctx, policy, now, runnerStorageVolume)
}

func (c *runnerBuildCache) trimForVolume(ctx context.Context, policy toolcache.Policy, now time.Time, volume func(string) (uint64, uint64, error)) error {
	if c == nil || !c.mu.TryLock() {
		return nil
	}
	defer c.mu.Unlock()
	ctx = toolcache.WithWaitObserver(ctx, func(lock string, waiting bool) {
		slog.Info("managed Go build cache maintenance wait", "path", c.root, "cache_lock", lock, "waiting", waiting)
	})
	freeBefore, capacity, freeBeforeErr := volume(c.root)
	var pressureReclaim int64
	if freeBeforeErr == nil {
		reserve := toolcache.Policy{}.NormalizedForCapacity(capacity).MaxBytes
		if freeBefore < uint64(reserve) {
			headroom := min(policy.NormalizedForCapacity(capacity).MaxBytes, reserve)
			pressureReclaim = reserve - int64(freeBefore) + headroom
			policy = policy.WithReclaimBytes(pressureReclaim)
		}
	}
	report, err := c.cache.Maintain(ctx, policy, now)
	c.report.Store(&report)
	var native toolcache.Report
	if c.native != "" {
		var nativeErr error
		native, nativeErr = toolcache.TrimWithReport(ctx, c.native, policy, now)
		err = errors.Join(err, nativeErr)
	}
	status := "complete"
	if report.Deferred {
		status = "busy"
	}
	if err != nil {
		status = "failed"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = "cancelled"
	}
	freeAfter, _, freeAfterErr := volume(c.root)
	slog.Info("managed Go build cache maintenance", "path", c.root, "status", status,
		"retained_bytes", report.RetainedBytes, "age_expired_bytes", report.AgeExpiredBytes,
		"size_evicted_bytes", report.SizeEvictedBytes, "native_path", c.native, "native_age_expired_bytes", native.AgeExpiredBytes,
		"pressure_reclaim_bytes", pressureReclaim, "free_bytes_before", freeBefore,
		"free_bytes_after", freeAfter, "disk_observation_error", errors.Join(freeBeforeErr, freeAfterErr))
	return err
}
