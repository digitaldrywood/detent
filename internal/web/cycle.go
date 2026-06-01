package web

import (
	"context"
	"log/slog"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (s *Server) snapshotCycleTime(ctx context.Context) (telemetry.CycleTimeReport, bool) {
	if s.store == nil {
		return telemetry.CycleTimeReport{}, false
	}

	report, err := s.store.CycleTimeReport(ctx)
	if err != nil {
		s.logger.Warn("cycle time report failed", slog.Any("error", err))
		return telemetry.CycleTimeReport{DegradedReason: "cycle-time query failed"}, true
	}
	return cycleTimeReportFromStore(report), true
}

func cycleTimeReportFromStore(report store.CycleTimeReport) telemetry.CycleTimeReport {
	issues := make([]telemetry.CycleTimeIssue, 0, len(report.Issues))
	for _, issue := range report.Issues {
		issues = append(issues, telemetry.CycleTimeIssue{
			Key:             issue.Key,
			StartedAt:       issue.StartedAt,
			CompletedAt:     issue.CompletedAt,
			DurationSeconds: issue.DurationSeconds,
			Sessions:        issue.Sessions,
		})
	}

	buckets := make([]telemetry.CycleTimeBucket, 0, len(report.Buckets))
	for _, bucket := range report.Buckets {
		buckets = append(buckets, telemetry.CycleTimeBucket{
			Label:      bucket.Label,
			MinSeconds: bucket.MinSeconds,
			MaxSeconds: bucket.MaxSeconds,
			Count:      bucket.Count,
		})
	}

	return telemetry.CycleTimeReport{
		Available:      true,
		Issues:         issues,
		Buckets:        buckets,
		AverageSeconds: report.AverageSeconds,
	}
}
