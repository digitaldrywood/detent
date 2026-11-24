package remoterunner

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

type Heartbeater interface {
	Heartbeat(context.Context) error
	HeartbeatInterval() time.Duration
}

type Ticker func(time.Duration) (<-chan time.Time, func())

func RunHeartbeats(ctx context.Context, source Heartbeater, newTicker Ticker, logger *slog.Logger) {
	if newTicker == nil {
		newTicker = systemTicker
	}
	ticks, stop := newTicker(source.HeartbeatInterval())
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := source.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("Hub runner heartbeat failed", "error", err)
			}
		}
	}
}

func systemTicker(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval - rand.N(interval/10+1))
	return ticker.C, ticker.Stop
}
