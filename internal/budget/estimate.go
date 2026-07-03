package budget

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/digitaldrywood/detent/internal/store"
)

const (
	DefaultDispatchEstimateMinSessions = 5
	DefaultDispatchEstimateLimit       = 50
)

type DispatchEstimateStore interface {
	RecentModelTokenQuantiles(context.Context, store.ModelTokenQuantileQuery) (store.ModelTokenQuantiles, error)
}

type DispatchEstimator struct {
	store       DispatchEstimateStore
	minSessions int64
	limit       int64
}

type DispatchEstimatorOption func(*DispatchEstimator)

func NewDispatchEstimator(store DispatchEstimateStore, opts ...DispatchEstimatorOption) *DispatchEstimator {
	estimator := &DispatchEstimator{
		store:       store,
		minSessions: DefaultDispatchEstimateMinSessions,
		limit:       DefaultDispatchEstimateLimit,
	}
	for _, opt := range opts {
		opt(estimator)
	}
	if estimator.minSessions <= 0 {
		estimator.minSessions = DefaultDispatchEstimateMinSessions
	}
	if estimator.limit <= 0 {
		estimator.limit = DefaultDispatchEstimateLimit
	}
	return estimator
}

func WithDispatchEstimateMinSessions(minSessions int64) DispatchEstimatorOption {
	return func(estimator *DispatchEstimator) {
		estimator.minSessions = minSessions
	}
}

func WithDispatchEstimateLimit(limit int64) DispatchEstimatorOption {
	return func(estimator *DispatchEstimator) {
		estimator.limit = limit
	}
}

func (e *DispatchEstimator) EstimateDispatch(ctx context.Context, model string) (TokenEstimate, error) {
	if e == nil || missingEstimateStore(e.store) || strings.TrimSpace(model) == "" {
		return defaultTokenEstimate, nil
	}

	quantiles, err := e.store.RecentModelTokenQuantiles(ctx, store.ModelTokenQuantileQuery{
		Model: model,
		Limit: e.limit,
	})
	if err != nil {
		return TokenEstimate{}, fmt.Errorf("recent model token quantiles: %w", err)
	}
	if quantiles.Sessions < e.minSessions {
		return defaultTokenEstimate, nil
	}

	return TokenEstimate{
		InputTokens:       quantiles.P90InputTokens,
		CachedInputTokens: quantiles.P90CachedInputTokens,
		OutputTokens:      quantiles.P90OutputTokens,
		TotalTokens:       quantiles.P90TotalTokens,
		Sessions:          quantiles.Sessions,
	}.normalized(), nil
}

func missingEstimateStore(store DispatchEstimateStore) bool {
	if store == nil {
		return true
	}

	value := reflect.ValueOf(store)
	return value.Kind() == reflect.Pointer && value.IsNil()
}
