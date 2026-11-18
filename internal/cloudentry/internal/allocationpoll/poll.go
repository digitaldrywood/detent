package allocationpoll

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
)

func Run(ctx context.Context, wake <-chan struct{}, now func() time.Time, step func(*platformstore.AllocationCandidate) (bool, time.Time)) {
	const base = 5 * time.Second
	interval := base
	timer := time.NewTimer(interval)
	defer timer.Stop()
	var after platformstore.AllocationCandidate
	for ctx.Err() == nil {
		worked, next := step(&after)
		if worked {
			interval = base
		} else {
			interval = min(2*interval, time.Minute)
		}
		delay := interval/2 + rand.N(interval/2)
		if !next.IsZero() {
			delay = min(delay, max(0, next.Sub(now())))
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
			interval = base
		}
	}
}
