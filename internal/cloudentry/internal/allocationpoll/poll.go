package allocationpoll

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
)

const BaseInterval = 5 * time.Second

func Run(ctx context.Context, wake <-chan struct{}, ticker *time.Ticker, now func() time.Time, step func(*platformstore.AllocationCandidate) (bool, time.Time), sweep func(context.Context, string) string) {
	interval := BaseInterval
	var after platformstore.AllocationCandidate
	var sweepAfter string
	var nextSweep time.Time
	var sweeping chan string
	defer func() {
		if sweeping != nil {
			<-sweeping
		}
	}()
	for ctx.Err() == nil {
		worked, next := step(&after)
		select {
		case sweepAfter = <-sweeping:
			sweeping = nil
		default:
		}
		if sweep != nil && sweeping == nil && ctx.Err() == nil && (sweepAfter != "" || !now().Before(nextSweep)) {
			if sweepAfter == "" {
				nextSweep = now().Add(time.Hour)
			}
			sweeping = make(chan string, 1)
			go func(after string, done chan<- string) { done <- sweep(ctx, after) }(sweepAfter, sweeping)
			worked = true
		}
		if worked {
			interval = BaseInterval
		} else {
			interval = min(2*interval, time.Minute)
		}
		delay := interval/2 + rand.N(interval/2)
		if !next.IsZero() {
			delay = min(delay, max(time.Nanosecond, next.Sub(now())))
		}
		ticker.Reset(delay)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
			interval = BaseInterval
		}
	}
}
