package observability

import (
	"testing"
	"time"
)

func TestConditionClassification(t *testing.T) {
	t.Parallel()
	zero := 0
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		got  Class
		want Class
	}{
		{name: "lane aging is diagnostic", got: Staleness(false), want: ClassDiagnostic},
		{name: "review wait stays in review queue", got: Staleness(true), want: ClassReviewQueue},
		{name: "authorization decline across stalled project is fault", got: Dispatch(true, "authorization_selector_declined"), want: ClassFault},
		{name: "pacing stall is diagnostic", got: Dispatch(true, "provider_rate_window_backpressure"), want: ClassDiagnostic},
		{name: "human gate stall is review queue", got: Dispatch(true, "artifact_gate_wait_status"), want: ClassReviewQueue},
		{name: "unstalled dispatch has no condition", got: Dispatch(false, "authorization_selector_declined"), want: ""},
		{name: "provider usage outage is fault", got: BackendOutage("usage_limit"), want: ClassFault},
		{name: "github rest pause is diagnostic", got: BackendOutage("github_rest_rate_limit"), want: ClassDiagnostic},
		{name: "parked breaker without eligible work is diagnostic", got: FailureBreaker(&zero, 2, 2), want: ClassDiagnostic},
		{name: "breaker with unparked work is fault", got: FailureBreaker(&zero, 2, 1), want: ClassFault},
		{name: "scheduled recovery is diagnostic", got: DispatchRecovery("waiting", now.Add(time.Minute), now), want: ClassDiagnostic},
		{name: "unscheduled recovery is diagnostic", got: DispatchRecovery("waiting", time.Time{}, now), want: ClassDiagnostic},
		{name: "ramping recovery is diagnostic", got: DispatchRecovery("ramping", now, now), want: ClassDiagnostic},
		{name: "overdue recovery is fault", got: DispatchRecovery("waiting", now.Add(-time.Minute), now), want: ClassFault},
		{name: "fault wins merged class", got: Merge(ClassReviewQueue, ClassDiagnostic, ClassFault), want: ClassFault},
		{name: "diagnostic wins review queue", got: Merge(ClassReviewQueue, ClassDiagnostic), want: ClassDiagnostic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Fatalf("classification = %q, want %q", tt.got, tt.want)
			}
		})
	}
}
