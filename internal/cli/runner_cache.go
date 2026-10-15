package cli

import (
	"context"
	"errors"
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
	return &runnerBuildCache{cache: cache, root: root}, nil
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
	return err
}
