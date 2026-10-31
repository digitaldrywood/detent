package web

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestConcurrencyHistoryFromStorePreservesHourlyStatistics(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	history := concurrencyHistoryFromStore(store.ConcurrencyReport{
		From:         start,
		To:           start.Add(time.Hour),
		Bucket:       time.Hour,
		AttemptCount: 2,
		Series: []store.ConcurrencySeries{{
			ProjectID: "alpha",
			Buckets: []store.ConcurrencyBucket{{
				Start: start, End: start.Add(time.Hour), Median: 1, P90: 2, Max: 4, ActiveSeconds: 3600,
			}},
		}},
	})

	if !history.Available || history.BucketSeconds != 3600 || history.AttemptCount != 2 || len(history.Series) != 1 {
		t.Fatalf("history = %#v", history)
	}
	bucket := history.Series[0].Buckets[0]
	if bucket.Median != 1 || bucket.P90 != 2 || bucket.Max != 4 || bucket.ActiveSeconds != 3600 {
		t.Fatalf("bucket = %#v", bucket)
	}
}
