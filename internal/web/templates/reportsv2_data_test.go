package templates

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

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
