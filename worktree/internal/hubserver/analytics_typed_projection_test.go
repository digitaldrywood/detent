package hubserver

import (
	"strings"
	"testing"
	"time"
)

func TestAnalyticsFailureTime(t *testing.T) {
	observed := time.Date(2026, 10, 25, 0, 0, 0, 7, time.UTC)
	updated := "2026-10-25T01:00:00Z"
	for _, test := range []struct {
		name, status string
		landing      *analyticsAttemptLanding
		want         string
	}{
		{"unknown landing", "succeeded", nil, ""},
		{"failed without landing", "failed", nil, updated},
		{"interrupted", "interrupted", nil, updated},
		{"conflict", "succeeded", &analyticsAttemptLanding{RefusalKind: "conflict", ObservedAt: observed}, formatHubTime(observed)},
		{"protection", "succeeded", &analyticsAttemptLanding{RefusalKind: "base_protected", ObservedAt: observed}, formatHubTime(observed)},
		{"refusal then landed", "succeeded", &analyticsAttemptLanding{RefusalKind: "conflict", Landed: true, ObservedAt: observed}, updated},
		{"gate failure", "succeeded", &analyticsAttemptLanding{GateFailed: true, Landed: true, ObservedAt: observed}, formatHubTime(observed)},
		{"missing observation", "succeeded", &analyticsAttemptLanding{GateFailed: true}, updated},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := analyticsFailureTime(test.landing, test.status, updated)
			if got.Valid != (test.want != "") || got.String != test.want || strings.Contains(got.String, "0001-") {
				t.Fatalf("failure time=%v want %q", got, test.want)
			}
		})
	}
}
