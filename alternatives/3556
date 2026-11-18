package postgres

import (
	"context"
	"time"
)

func (f *runnerEventSession) BillingPopulation(ctx context.Context, metric string, now time.Time, connected time.Duration) (int64, bool, error) {
	return f.session.BillingPopulation(ctx, metric, now, connected)
}
