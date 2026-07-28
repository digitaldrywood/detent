package store

import "testing"

func TestCapacityConstraintReason(t *testing.T) {
	t.Parallel()

	zero := 0
	one := 1
	tests := []struct {
		name            string
		waitReason      string
		globalAvailable *int
		want            CapacityConstraintReason
		wantOK          bool
	}{
		{
			name:            "exhausted pool",
			waitReason:      poolHigherPriorityProjectWaitReason,
			globalAvailable: &zero,
			want:            CapacityConstraintPool,
			wantOK:          true,
		},
		{
			name:            "pool arbitration with capacity",
			waitReason:      poolSelectedProjectWaitReason,
			globalAvailable: &one,
		},
		{
			name:       "project",
			waitReason: "project_capacity_full",
			want:       CapacityConstraintProject,
			wantOK:     true,
		},
		{
			name:       "lane",
			waitReason: "lane_capacity_full",
			want:       CapacityConstraintLane,
			wantOK:     true,
		},
		{
			name:       "legacy lane",
			waitReason: "local_slot_unavailable",
			want:       CapacityConstraintLane,
			wantOK:     true,
		},
		{
			name:       "worker host",
			waitReason: "worker_host_capacity_full",
			want:       CapacityConstraintWorkerHost,
			wantOK:     true,
		},
		{
			name:       "legacy worker host",
			waitReason: "worker_host_unavailable",
			want:       CapacityConstraintWorkerHost,
			wantOK:     true,
		},
		{
			name:       "rate window",
			waitReason: "provider_rate_window_backpressure",
			want:       CapacityConstraintRateWindow,
			wantOK:     true,
		},
		{
			name:       "unrelated scheduler reason",
			waitReason: "blocked_by_dependency",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := capacityConstraintReason(tt.waitReason, tt.globalAvailable)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf(
					"capacityConstraintReason(%q) = %q, %v, want %q, %v",
					tt.waitReason,
					got,
					ok,
					tt.want,
					tt.wantOK,
				)
			}
		})
	}
}
