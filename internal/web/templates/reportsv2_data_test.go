package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestReportsEfficiencyRendersRollupAndBaseline(t *testing.T) {
	t.Parallel()

	data := ReportsData{Efficiency: efficiency.Rollup{
		Current: efficiency.RollupWindow{
			Issues:                3,
			TokensPerIssue:        efficiency.Percentiles{P50: 1_000_000, P90: 3_500_000},
			CostPerIssueUSD:       efficiency.Percentiles{P50: 3.25, P90: 8.75},
			CacheShare:            0.97,
			SessionsPerIssue:      1.5,
			FirstAttemptMergeRate: 0.67,
			Dwell:                 efficiency.Dwell{WorkingSeconds: 3600, GateWaitSeconds: 900, MergeTrainSeconds: 300},
			Anomalies:             1,
		},
		Baseline: efficiency.RollupWindow{
			TokensPerIssue:        efficiency.Percentiles{P50: 800_000, P90: 2_000_000},
			CostPerIssueUSD:       efficiency.Percentiles{P50: 2.5, P90: 6},
			CacheShare:            0.95,
			SessionsPerIssue:      1.2,
			FirstAttemptMergeRate: 0.75,
		},
		CacheTrend: []efficiency.TrendPoint{{Day: "2026-07-09", CacheShare: 0.96}, {Day: "2026-07-10", CacheShare: 0.97}},
	}}
	var output bytes.Buffer
	if err := ReportsSnapshotV2(data).Render(context.Background(), &output); err != nil {
		t.Fatalf("ReportsSnapshotV2() render error = %v", err)
	}
	html := output.String()
	for _, want := range []string{"reports-efficiency", "Fleet efficiency", "Tokens / merged issue", "Baseline", "97%", "anomaly receipts", "Cache share trend"} {
		if !strings.Contains(html, want) {
			t.Fatalf("reports efficiency missing %q:\n%s", want, html)
		}
	}
}

func TestReportsP95IgnoresZeroBuckets(t *testing.T) {
	// A window dominated by $0 days plus one normal day and one spike must
	// still clamp at the positive P95, not collapse to 0 (no clamp).
	values := []float64{0, 0, 0, 0, 0, 0, 0, 0, 12, 480}
	if got := reportsP95(values); got <= 0 {
		t.Fatalf("reportsP95 with zero-dominated window = %v, want a positive clamp", got)
	}

	// Fewer than two positive values cannot form a percentile: no clamp.
	if got := reportsP95([]float64{0, 0, 25}); got != 0 {
		t.Fatalf("reportsP95 with a single positive value = %v, want 0", got)
	}

	// A flat positive series has no outlier to clamp.
	if got := reportsP95([]float64{10, 10, 10, 10}); got != 0 {
		t.Fatalf("reportsP95 on a flat series = %v, want 0", got)
	}
}

func TestReportsTopRowsResolveTrackerURLs(t *testing.T) {
	t.Parallel()

	issueURL := "https://github.com/digitaldrywood/detent/issues/117"
	prURL := "https://github.com/digitaldrywood/detent/pull/133"
	snapshot := telemetry.Snapshot{BoardIssues: []telemetry.Issue{{
		Identifier: "digitaldrywood/detent#117",
		URL:        issueURL,
		PullRequest: &telemetry.PullRequest{
			Number: 133,
			URL:    prURL,
		},
	}}}

	tests := []struct {
		name   string
		prefix string
		bucket string
		want   string
	}{
		{name: "issue", prefix: "issue", bucket: "digitaldrywood/detent#117", want: issueURL},
		{name: "pull request", prefix: "pr", bucket: "detent#133", want: prURL},
		{name: "unknown", prefix: "issue", bucket: "MT-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rows := reportsTopRows(tt.prefix, []UsageBucketData{{Bucket: tt.bucket}}, snapshot)
			if len(rows) != 1 || rows[0].URL != tt.want {
				t.Fatalf("reportsTopRows() = %#v, want URL %q", rows, tt.want)
			}
		})
	}
}

func TestReportsDigestViewShowsTrailingSevenDayDelta(t *testing.T) {
	t.Parallel()

	days := make([]DailyDigestDayData, 8)
	for index := range 7 {
		days[index] = DailyDigestDayData{Date: "2026-07-01", Sessions: 10, InputTokens: 100, CachedInputTokens: 80, TotalTokens: 1000, SpendUSD: 1, Efficiency: efficiency.RollupWindow{Issues: 1, TokensPerIssue: efficiency.Percentiles{P50: 1000, P90: 1500}, CostPerIssueUSD: efficiency.Percentiles{P50: 1, P90: 1.5}, SessionsPerIssue: 1, FirstAttemptMergeRate: 0.5, Dwell: efficiency.Dwell{WorkingSeconds: 60}}}
	}
	days[7] = DailyDigestDayData{Date: "2026-07-08", Sessions: 20, InputTokens: 200, CachedInputTokens: 100, TotalTokens: 1500, SpendUSD: 2, Efficiency: efficiency.RollupWindow{Issues: 2, TokensPerIssue: efficiency.Percentiles{P50: 2000, P90: 3000}, CostPerIssueUSD: efficiency.Percentiles{P50: 2, P90: 3}, SessionsPerIssue: 2, FirstAttemptMergeRate: 1, Dwell: efficiency.Dwell{WorkingSeconds: 120}, Anomalies: 1}}

	view := reportsDigestView(DailyDigestData{Timezone: "America/Chicago", Days: days})

	if view.Timezone != "America/Chicago" || len(view.Days) != 7 || !view.Days[0].Today {
		t.Fatalf("digest view = %#v, want seven visible days with today first", view)
	}
	metrics := map[string]reportsDigestMetric{}
	for _, metric := range view.Days[0].Metrics {
		metrics[metric.ID] = metric
	}
	if metrics["sessions"].Delta != "+100% vs 7d" || metrics["sessions"].Value != "20" {
		t.Fatalf("session metric = %#v, want 20 and +100%%", metrics["sessions"])
	}
	if metrics["cache"].Value != "50%" || metrics["cost"].Value != "$2.00" {
		t.Fatalf("cache/cost metrics = %#v / %#v, want exact current values", metrics["cache"], metrics["cost"])
	}
	if metrics["tokens-per-merged"].Value != "2.0K" || metrics["tokens-per-merged"].Detail != "p90 3.0K" || metrics["first-attempt"].Value != "100%" {
		t.Fatalf("efficiency digest metrics = %#v / %#v", metrics["tokens-per-merged"], metrics["first-attempt"])
	}
	if metrics["efficiency-anomalies"].Value != "1" || metrics["dwell-working"].Value != "2m 0s" {
		t.Fatalf("efficiency anomaly/dwell metrics = %#v / %#v", metrics["efficiency-anomalies"], metrics["dwell-working"])
	}
}
