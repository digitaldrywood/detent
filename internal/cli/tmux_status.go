package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type tmuxWindowStatus interface {
	Update(context.Context, telemetry.Snapshot) error
}

func runTmuxWindowStatusUpdates(
	ctx context.Context,
	updates <-chan telemetry.Snapshot,
	ticks <-chan time.Time,
	status tmuxWindowStatus,
	logger *slog.Logger,
) {
	var latest telemetry.Snapshot
	initialized := false
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			latest = snapshot
			initialized = true
		case <-ticks:
			if !initialized {
				continue
			}
			if err := status.Update(ctx, latest); err != nil {
				logger.Warn("update tmux window status failed", "error", err)
			}
		}
	}
}
