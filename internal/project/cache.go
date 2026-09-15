package project

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace"
)

// SweepPausedSharedCache lets the existing fleet refresh perform the workspace
// sweep for projects whose orchestrator is stopped. Serialize with unpause so
// module cache removal finishes before workers can start.
func (p *Project) SweepPausedSharedCache(ctx context.Context, now time.Time) workspace.CacheUsage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cfg.Paused || p.done != nil {
		return p.pausedCache
	}
	cfg := p.workflow.Config.Workspace
	interval := time.Duration(cfg.CleanupSweepIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if !p.pausedCache.ObservedAt.IsZero() && now.Before(p.pausedCache.ObservedAt.Add(interval)) {
		return p.pausedCache
	}
	p.pausedCache = workspace.TrimSharedCache(ctx, cfg.Root, p.cfg.ID, time.Duration(cfg.CleanupIdleTTLMS)*time.Millisecond, false)
	return p.pausedCache
}
