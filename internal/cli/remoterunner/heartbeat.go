package remoterunner

import (
	"context"
	"crypto/rand"
	"log/slog"
	"math/big"
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
	offset, err := rand.Int(rand.Reader, big.NewInt(int64(interval/10+1)))
	if err == nil {
		interval -= time.Duration(offset.Int64())
	}
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}
