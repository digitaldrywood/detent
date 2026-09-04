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
			name:   "worker host capacity wait",
			reason: dispatchSkipWorkerHostUnavailable,
			want:   "worker_host_capacity_full",
		},
		{
			name:   "memory pressure wait",
			reason: dispatchIssueFailureMemoryPressure,
			want:   "memory pressure is above the admission threshold",
		},
		{
			name:   "IO pressure wait",
			reason: dispatchIssueFailureIOPressure,
			want:   "IO pressure is above the admission threshold",
		},
		{
			name:   "CPU pressure wait",
			reason: dispatchIssueFailureCPUPressure,
			want:   "CPU pressure is above the admission threshold",
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
