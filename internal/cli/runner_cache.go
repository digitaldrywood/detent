package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

type runnerBuildCache struct {
	cache     *toolcache.Managed
	root      string
	gate      dispatchPauser
	processes workerProcessStore
	logger    *slog.Logger
	mu        sync.Mutex
	report    atomic.Pointer[toolcache.Report]
}

var errNativeBuildCache = errors.New("runner uses the native Go build cache")

func newRunnerBuildCache(ctx context.Context, cfg BootConfig, gate dispatchPauser, processes workerProcessStore, logger *slog.Logger) (*runnerBuildCache, error) {
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
	return &runnerBuildCache{cache: cache, root: root, gate: gate, processes: processes, logger: logger}, nil
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
	if c == nil || !c.mu.TryLock() {
		return nil
	}
	defer c.mu.Unlock()
	report, err := toolcache.TrimWithReport(ctx, c.root, policy, now)
	c.report.Store(&report)
	if err != nil {
		return err
	}
	capacity, err := toolcache.CapacityBytes(c.root)
	if err != nil {
		return err
	}
	if report.RetainedBytes <= policy.NormalizedForCapacity(capacity).MaxBytes {
		return nil
	}
	if c.gate == nil {
		return errors.New("managed Go cache has no dispatch owner")
	}
	resume := c.gate.PauseDispatch()
	defer resume()
	report, err = c.cache.Maintain(ctx, policy, now, c.confirmQuiescence)
	if err != nil {
		previous := c.observe()
		if previous != nil {
			report = *previous
		}
		report.Error = err.Error()
	}
	c.report.Store(&report)
	return err
}

func (c *runnerBuildCache) confirmQuiescence(ctx context.Context) error {
	if c.processes == nil {
		return errors.New("managed Go cache has no process recovery owner")
	}
	if err := reapWorkerProcesses(ctx, c.processes, c.logger, "cache_maintenance", procgroup.DefaultTerminationGrace, time.Now, nil); err != nil {
		return fmt.Errorf("confirm managed Go cache consumers stopped: %w", err)
	}
	active, err := c.processes.ListActiveWorkerProcesses(ctx)
	if err != nil {
		return err
	}
	if len(active) != 0 {
		return errors.New("managed Go cache still has recorded worker processes")
	}
	return nil
}
