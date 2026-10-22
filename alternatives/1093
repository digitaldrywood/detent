package connector

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

const LocalDeferralReasonRESTFanoutCap = "github_rest_fanout_cap"

type LocalDeferral struct {
	Reason     string
	Scope      string
	RetryAfter time.Duration
}

type LocalDeferralReporter interface {
	LocalDeferral() LocalDeferral
}

func ErrorLocalDeferral(err error) (LocalDeferral, bool) {
	var reporter LocalDeferralReporter
	if !errors.As(err, &reporter) || reporter == nil {
		return LocalDeferral{}, false
	}
	deferral := reporter.LocalDeferral()
	deferral.Reason = strings.TrimSpace(deferral.Reason)
	deferral.Scope = strings.TrimSpace(deferral.Scope)
	return deferral, deferral.Reason != ""
}

type restFanoutBudgetContextKey struct{}

type RESTFanoutBudget struct {
	scope string
	mu    sync.Mutex
	units int64
}

func WithRESTFanoutBudget(ctx context.Context, scope string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, restFanoutBudgetContextKey{}, &RESTFanoutBudget{scope: strings.TrimSpace(scope)})
}

func RESTFanoutBudgetFromContext(ctx context.Context) (*RESTFanoutBudget, bool) {
	if ctx == nil {
		return nil, false
	}
	budget, ok := ctx.Value(restFanoutBudgetContextKey{}).(*RESTFanoutBudget)
	return budget, ok && budget != nil
}

func (b *RESTFanoutBudget) Scope() string {
	if b == nil {
		return ""
	}
	return b.scope
}

func (b *RESTFanoutBudget) Reserve(maxUnits int64, requestUnits int64) (int64, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	used := b.units
	if maxUnits > 0 && used+requestUnits > maxUnits {
		return used, false
	}
	b.units += requestUnits
	return used, true
}

func (b *RESTFanoutBudget) Add(units int64) {
	if b == nil || units == 0 {
		return
	}
	b.mu.Lock()
	b.units += units
	b.mu.Unlock()
}
