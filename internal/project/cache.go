package project

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace"
)

// SweepSharedCache trims every project on the existing fleet refresh cadence.
// Serialize with start/unpause so inactive module cleanup finishes before workers start.
func (p *Project) SweepSharedCache(ctx context.Context, now time.Time) workspace.CacheUsage {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.mu.Lock()
	cfg := p.workflow.Config.Workspace
	projectID := p.cfg.ID
	active := !p.cfg.Paused || p.done != nil
	if active {
		p.mu.Unlock() // Active scans must not hold up project control operations.
	} else {
		defer p.mu.Unlock()
	}
	interval := time.Duration(cfg.CleanupSweepIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if !p.sharedCache.ObservedAt.IsZero() && now.Before(p.sharedCache.ObservedAt.Add(interval)) {
		return p.sharedCache
	}
	// Cache scans have their own deadline, independent of tracker/workspace cleanup.
	sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	p.sharedCache = workspace.TrimSharedCache(sweepCtx, cfg.Root, projectID, time.Duration(cfg.CleanupIdleTTLMS)*time.Millisecond, active)
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("shared cache sweep", "project_id", projectID,
		"measured_bytes", p.sharedCache.TotalBytes+p.sharedCache.RemovedBytes,
		"retained_bytes", p.sharedCache.TotalBytes, "build_bytes", p.sharedCache.BuildBytes,
		"budget_bytes", p.sharedCache.BudgetBytes, "removed_bytes", p.sharedCache.RemovedBytes,
		"error", p.sharedCache.Error)
	return p.sharedCache
}
