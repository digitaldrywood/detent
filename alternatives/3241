package web

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	dashboardConcurrencyWindow = 24 * time.Hour
	dashboardConcurrencyBucket = time.Hour
)

func (s *Server) snapshotConcurrency(ctx context.Context, snapshot telemetry.Snapshot) telemetry.ConcurrencyHistory {
	if s.store == nil {
		return telemetry.ConcurrencyHistory{DegradedReason: "runtime store is not configured"}
	}
	reader, ok := s.store.(store.ConcurrencyStore)
	if !ok {
		return telemetry.ConcurrencyHistory{DegradedReason: "runtime store does not provide concurrency history"}
	}
	now := snapshot.GeneratedAt
	if now.IsZero() {
		now = s.now()
	}
	now = now.UTC().Truncate(dashboardConcurrencyBucket)
	report, err := reader.ConcurrencyReport(ctx, store.ConcurrencyQuery{
		From:   now.Add(-dashboardConcurrencyWindow),
		To:     now,
		Bucket: dashboardConcurrencyBucket,
	})
	if err != nil {
		s.logger.WarnContext(ctx, "concurrency history query failed", slog.Any("error", err))
		return telemetry.ConcurrencyHistory{DegradedReason: "concurrency history query failed"}
	}
	return concurrencyHistoryFromStore(report)
}

func concurrencyHistoryFromStore(report store.ConcurrencyReport) telemetry.ConcurrencyHistory {
	history := telemetry.ConcurrencyHistory{
		Available:     true,
		From:          report.From,
		To:            report.To,
		BucketSeconds: int64(report.Bucket / time.Second),
		AttemptCount:  report.AttemptCount,
		Series:        make([]telemetry.ConcurrencySeries, 0, len(report.Series)),
	}
	for _, source := range report.Series {
		series := telemetry.ConcurrencySeries{ProjectID: source.ProjectID, Buckets: make([]telemetry.ConcurrencyBucket, 0, len(source.Buckets))}
		for _, bucket := range source.Buckets {
			series.Buckets = append(series.Buckets, telemetry.ConcurrencyBucket{
				Start:         bucket.Start,
				End:           bucket.End,
				Median:        bucket.Median,
				P90:           bucket.P90,
				Max:           bucket.Max,
				ActiveSeconds: bucket.ActiveSeconds,
			})
		}
		history.Series = append(history.Series, series)
	}
	return history
}
