package orchestrator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	tickWatchdogIntervalCount = int64(2)
	maxTickWatchdogCheckEvery = 30 * time.Second
)

type tickWatchdog struct {
	mu            sync.RWMutex
	projectID     string
	status        telemetry.TickLivenessStatus
	lastTickAt    time.Time
	nextRefreshAt time.Time
	interval      time.Duration
	frozenAt      time.Time
	logger        *slog.Logger
}

func newTickWatchdog(projectID string, interval time.Duration, logger *slog.Logger) *tickWatchdog {
	if logger == nil {
		logger = slog.Default()
	}
	return &tickWatchdog{
		projectID: projectID,
		status:    telemetry.TickLivenessStatusInitializing,
		interval:  interval,
		logger:    logger,
	}
}

func (w *tickWatchdog) Advance(at time.Time, nextRefreshAt time.Time, interval time.Duration) {
	if w == nil || at.IsZero() {
		return
	}
	at = at.UTC()
	if nextRefreshAt.IsZero() && interval > 0 {
		nextRefreshAt = at.Add(interval)
	}
	if !nextRefreshAt.IsZero() {
		nextRefreshAt = nextRefreshAt.UTC()
	}

	w.mu.Lock()
	recovered := w.status == telemetry.TickLivenessStatusNeedsAttention
	w.lastTickAt = at
	w.nextRefreshAt = nextRefreshAt
	if interval > 0 {
		w.interval = interval
	}
	w.frozenAt = time.Time{}
	w.status = telemetry.TickLivenessStatusReady
	w.mu.Unlock()

	if recovered {
		w.logger.Info("orchestrator tick loop recovered", "project_id", w.projectID, "last_tick_at", at)
	}
}

func (w *tickWatchdog) Schedule(nextRefreshAt time.Time, interval time.Duration) {
	if w == nil {
		return
	}
	if !nextRefreshAt.IsZero() {
		nextRefreshAt = nextRefreshAt.UTC()
	}
	w.mu.Lock()
	w.nextRefreshAt = nextRefreshAt
	if interval > 0 {
		w.interval = interval
	}
	w.mu.Unlock()
}

func (w *tickWatchdog) Evaluate(now time.Time) telemetry.TickLiveness {
	if w == nil {
		return telemetry.TickLiveness{Status: telemetry.TickLivenessStatusInitializing}
	}
	now = now.UTC()
	w.mu.Lock()
	previous := w.status
	missed := missedTickIntervals(w.lastTickAt, now, w.interval)
	if !w.lastTickAt.IsZero() && missed >= tickWatchdogIntervalCount {
		w.status = telemetry.TickLivenessStatusNeedsAttention
		if w.frozenAt.IsZero() {
			w.frozenAt = w.lastTickAt
		}
	}
	liveness := w.livenessLocked(now)
	w.mu.Unlock()

	if previous != telemetry.TickLivenessStatusNeedsAttention && liveness.Status == telemetry.TickLivenessStatusNeedsAttention {
		w.logger.Warn(
			"orchestrator tick loop frozen",
			"project_id", liveness.ProjectID,
			"last_tick_at", liveness.LastTickAt,
			"next_refresh_at", liveness.NextRefreshAt,
			"missed_intervals", liveness.MissedIntervals,
		)
	}
	return liveness
}

func (w *tickWatchdog) Snapshot(now time.Time) telemetry.TickLiveness {
	if w == nil {
		return telemetry.TickLiveness{Status: telemetry.TickLivenessStatusInitializing}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.livenessLocked(now.UTC())
}

func (w *tickWatchdog) Run(ctx context.Context) {
	if w == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(w.checkEvery())
	defer ticker.Stop()
	w.run(ctx, ticker.C)
}

func (w *tickWatchdog) checkEvery() time.Duration {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return tickWatchdogCheckEvery(w.interval)
}

func (w *tickWatchdog) run(ctx context.Context, checks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-checks:
			w.Evaluate(now)
		}
	}
}

func (w *tickWatchdog) livenessLocked(now time.Time) telemetry.TickLiveness {
	return telemetry.TickLiveness{
		ProjectID:             w.projectID,
		Status:                w.status,
		LastTickAt:            watchdogTimePointer(w.lastTickAt),
		NextRefreshAt:         watchdogTimePointer(w.nextRefreshAt),
		NextRefreshOverdue:    !w.nextRefreshAt.IsZero() && !now.IsZero() && now.After(w.nextRefreshAt),
		FrozenAt:              watchdogTimePointer(w.frozenAt),
		MissedIntervals:       missedTickIntervals(w.lastTickAt, now, w.interval),
		WatchdogIntervalCount: tickWatchdogIntervalCount,
	}
}

func missedTickIntervals(lastTickAt time.Time, now time.Time, interval time.Duration) int64 {
	if lastTickAt.IsZero() || now.IsZero() || interval <= 0 || now.Before(lastTickAt) {
		return 0
	}
	return int64(now.Sub(lastTickAt) / interval)
}

func tickWatchdogCheckEvery(interval time.Duration) time.Duration {
	checkEvery := interval / 2
	if checkEvery <= 0 {
		checkEvery = time.Millisecond
	}
	if checkEvery > maxTickWatchdogCheckEvery {
		return maxTickWatchdogCheckEvery
	}
	return checkEvery
}

func watchdogTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}
