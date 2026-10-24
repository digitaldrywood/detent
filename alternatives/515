package budget

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestDispatchEstimatorEstimateDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		store        *fakeDispatchEstimateStore
		minSessions  int64
		wantEstimate TokenEstimate
		wantErr      string
	}{
		{
			name:         "nil store uses static fallback",
			wantEstimate: defaultTokenEstimate,
		},
		{
			name: "insufficient sessions uses static fallback",
			store: &fakeDispatchEstimateStore{
				quantiles: store.ModelTokenQuantiles{
					Sessions:       4,
					P90InputTokens: 900,
				},
			},
			minSessions:  5,
			wantEstimate: defaultTokenEstimate,
		},
		{
			name: "enough sessions uses p90 observed tokens",
			store: &fakeDispatchEstimateStore{
				quantiles: store.ModelTokenQuantiles{
					Sessions:             5,
					P50InputTokens:       300,
					P90InputTokens:       900,
					P50CachedInputTokens: 100,
					P90CachedInputTokens: 400,
					P50OutputTokens:      30,
					P90OutputTokens:      90,
					P50TotalTokens:       330,
					P90TotalTokens:       990,
				},
			},
			minSessions: 5,
			wantEstimate: TokenEstimate{
				InputTokens:       900,
				CachedInputTokens: 400,
				OutputTokens:      90,
				TotalTokens:       990,
				Sessions:          5,
			},
		},
		{
			name: "store error is wrapped",
			store: &fakeDispatchEstimateStore{
				err: errors.New("database unavailable"),
			},
			minSessions: 5,
			wantErr:     "recent model token quantiles",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			estimator := NewDispatchEstimator(
				tt.store,
				WithDispatchEstimateMinSessions(tt.minSessions),
				WithDispatchEstimateLimit(25),
			)
			got, err := estimator.EstimateDispatch(context.Background(), "gpt-test")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("EstimateDispatch() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EstimateDispatch() error = %v", err)
			}
			if got != tt.wantEstimate {
				t.Fatalf("EstimateDispatch() = %#v, want %#v", got, tt.wantEstimate)
			}
			if tt.store != nil {
				if tt.store.query.Model != "gpt-test" || tt.store.query.Limit != 25 {
					t.Fatalf("query = %#v, want model gpt-test limit 25", tt.store.query)
				}
			}
		})
	}
}

type fakeDispatchEstimateStore struct {
	query     store.ModelTokenQuantileQuery
	quantiles store.ModelTokenQuantiles
	err       error
}

func (s *fakeDispatchEstimateStore) RecentModelTokenQuantiles(_ context.Context, query store.ModelTokenQuantileQuery) (store.ModelTokenQuantiles, error) {
	s.query = query
	return s.quantiles, s.err
}
