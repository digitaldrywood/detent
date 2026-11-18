package allocationpoll

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
)

const BaseInterval = 5 * time.Second

func Run(ctx context.Context, wake <-chan struct{}, ticker *time.Ticker, now func() time.Time, step func(*platformstore.AllocationCandidate) (bool, time.Time)) {
	interval := BaseInterval
	var after platformstore.AllocationCandidate
	for ctx.Err() == nil {
		worked, next := step(&after)
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
