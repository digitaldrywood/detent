package leasestore

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrUnaccountedUsage = errors.New("historical runner usage has not been accounted from its Hub receipts")

type RunnerSpendReader interface {
	RunnerTokenSpend(context.Context, tracker.ProjectID, string, *time.Time) (store.TokenSpend, error)
	RunnerModelTokenQuantiles(context.Context, tracker.ProjectID, store.ModelTokenQuantileQuery) (store.ModelTokenQuantiles, error)
}

type RunnerUsageWriter interface {
	AccountRunnerTokenUsage(context.Context, Attempt, []tracker.NativeUsage) error
}
