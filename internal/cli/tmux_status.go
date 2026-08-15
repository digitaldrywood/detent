package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const tmuxRestoreTimeout = 2 * time.Second

type tmuxWindowStatus interface {
	Update(context.Context, telemetry.Snapshot) error
	Close(context.Context) error
}

func runTmuxWindowStatus(
	ctx context.Context,
	snapshots *hub.Hub[telemetry.Snapshot],
	status tmuxWindowStatus,
	interval time.Duration,
	logger *slog.Logger,
) {
	if snapshots == nil || status == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = defaultSnapshotInterval
	}
	if logger == nil {
		logger = slog.Default()
	}

	subscription, err := snapshots.Subscribe(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("subscribe tmux window status failed", "error", err)
		}
		return
	}
	defer subscription.Close()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runTmuxWindowStatusUpdates(ctx, subscription.C(), ticker.C, status, logger)
}

func runTmuxWindowStatusUpdates(
	ctx context.Context,
	updates <-chan telemetry.Snapshot,
	ticks <-chan time.Time,
	status tmuxWindowStatus,
	logger *slog.Logger,
) {
	var latest telemetry.Snapshot
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			latest = snapshot
			pending = true
		case <-ticks:
			if !pending {
				continue
			}
			pending = false
			if err := status.Update(ctx, latest); err != nil {
				logger.Warn("update tmux window status failed", "error", err)
			}
		}
	}
}

func closeTmuxWindowStatus(status tmuxWindowStatus, logger *slog.Logger) {
	if status == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxRestoreTimeout)
	defer cancel()
	if err := status.Close(ctx); err != nil {
		logger.Warn("restore tmux window name failed", "error", err)
	}
}
