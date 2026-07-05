package templates

import "testing"

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
