package billingstore

import (
	"context"
	"time"
)

type PopulationReader interface {
	BillingPopulation(context.Context, string, time.Time, time.Duration) (int64, bool, error)
}
