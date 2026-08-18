package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestConcurrencyReportReadsPersistedWorkAttemptIntervals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	from := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	intervals := []struct {
		projectID string
		start     time.Time
		end       time.Time
	}{
		{projectID: "alpha", start: from, end: from.Add(time.Hour)},
		{projectID: "alpha", start: from.Add(30 * time.Minute), end: from.Add(90 * time.Minute)},
		{projectID: "beta", start: from.Add(15 * time.Minute), end: from.Add(45 * time.Minute)},
	}
	for index, interval := range intervals {
		attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
			ProjectID:      interval.projectID,
			IssueID:        interval.projectID + "-issue",
			WorkerType:     "agent",
			AttemptNumber:  index + 1,
			StartedAt:      interval.start,
			LeaseExpiresAt: interval.end,
		})
		if err != nil {
			t.Fatalf("StartWorkAttempt(%d) error = %v", index, err)
		}
		if err := backend.CompleteWorkAttempt(ctx, WorkAttemptCompletion{
			AttemptID:     attemptID,
			CompletedAt:   interval.end,
			Status:        WorkAttemptStatusTerminal,
			TerminalState: WorkAttemptTerminalSuccess,
		}); err != nil {
			t.Fatalf("CompleteWorkAttempt(%d) error = %v", index, err)
		}
	}

	reader, ok := backend.(ConcurrencyStore)
	if !ok {
		t.Fatalf("backend %T does not implement ConcurrencyStore", backend)
	}
	report, err := reader.ConcurrencyReport(ctx, ConcurrencyQuery{From: from, To: from.Add(2 * time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatalf("ConcurrencyReport() error = %v", err)
	}
	if report.AttemptCount != 3 || len(report.Series) != 3 {
		t.Fatalf("report = %#v, want fleet and two projects", report)
	}
	if got := report.Series[0].Buckets[0].Max; got != 3 {
		t.Fatalf("fleet first-hour max = %d, want 3", got)
	}
}

func TestConcurrencyReportBuildsFleetAndProjectHourlyQuantiles(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	query := ConcurrencyQuery{From: from, To: from.Add(2 * time.Hour), Bucket: time.Hour}
	intervals := []concurrencyInterval{
		{projectID: "alpha", start: from, end: from.Add(time.Hour)},
		{projectID: "alpha", start: from.Add(30 * time.Minute), end: from.Add(90 * time.Minute)},
		{projectID: "beta", start: from.Add(15 * time.Minute), end: from.Add(45 * time.Minute)},
	}

	report := concurrencyReport(intervals, query)
	if report.AttemptCount != 3 || len(report.Series) != 3 {
		t.Fatalf("report = %#v, want fleet and two project series", report)
	}
	tests := []struct {
		name       string
		series     int
		bucket     int
		wantP50    int
		wantP90    int
		wantMax    int
		wantActive int64
	}{
		{name: "fleet first hour", series: 0, bucket: 0, wantP50: 2, wantP90: 3, wantMax: 3, wantActive: 3600},
		{name: "fleet second hour", series: 0, bucket: 1, wantP50: 0, wantP90: 1, wantMax: 1, wantActive: 1800},
		{name: "alpha first hour", series: 1, bucket: 0, wantP50: 1, wantP90: 2, wantMax: 2, wantActive: 3600},
		{name: "beta first hour", series: 2, bucket: 0, wantP50: 0, wantP90: 1, wantMax: 1, wantActive: 1800},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bucket := report.Series[test.series].Buckets[test.bucket]
			if bucket.Median != test.wantP50 || bucket.P90 != test.wantP90 || bucket.Max != test.wantMax || bucket.ActiveSeconds != test.wantActive {
				t.Fatalf("bucket = %#v, want median=%d p90=%d max=%d active=%d", bucket, test.wantP50, test.wantP90, test.wantMax, test.wantActive)
			}
		})
	}
}

func TestConcurrencyReportRejectsInvalidWindows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		query ConcurrencyQuery
		want  string
	}{
		{name: "missing window", query: ConcurrencyQuery{Bucket: time.Hour}, want: "window is required"},
		{name: "reversed window", query: ConcurrencyQuery{From: now, To: now.Add(-time.Hour), Bucket: time.Hour}, want: "start must be before end"},
		{name: "missing bucket", query: ConcurrencyQuery{From: now, To: now.Add(time.Hour)}, want: "bucket must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := QueryConcurrencyReport(t.Context(), nil, test.query); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("QueryConcurrencyReport() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
