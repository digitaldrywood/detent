package orchestrator

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestSchedulerDecisionWaitReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "planner project capacity wait",
			reason: dispatchSkipGlobalCapacityFull,
			want:   "project_capacity_full",
		},
		{
			name:   "slot acquisition pool capacity wait",
			reason: dispatchIssueFailureGlobalSlotUnavailable,
			want:   scheduler.DispatchGateReasonGlobalCapacityFull,
		},
		{
			name:   "lane capacity wait",
			reason: dispatchSkipLocalSlotUnavailable,
			want:   "lane_capacity_full",
		},
		{
			name:   "other reason",
			reason: "provider_backoff",
			want:   "provider_backoff",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := schedulerDecisionWaitReason(tt.reason); got != tt.want {
				t.Fatalf("schedulerDecisionWaitReason(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}
