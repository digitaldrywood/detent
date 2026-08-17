package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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
			name:       "tracker unavailable",
			waitReason: "tracker_unavailable",
			want:       CapacityConstraintTrackerUnavailable,
			wantOK:     true,
		},
		{
			name:       "CI unavailable",
			waitReason: "ci_unavailable",
			want:       CapacityConstraintCIUnavailable,
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

func TestQueryCapacityConstraintWaitsNormalizesFiveMinuteSamples(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		reason     string
		lane       string
		decisionAt time.Time
		snapshot   string
	}{
		{
			reason:     poolCapacityWaitReason,
			decisionAt: now,
			snapshot:   `{"pool":"default","global_available":0}`,
		},
		{
			reason:     poolHigherPriorityProjectWaitReason,
			decisionAt: now.Add(4 * time.Minute),
			snapshot:   `{"pool":"default","global_available":0}`,
		},
		{
			reason:     poolCapacityWaitReason,
			decisionAt: now.Add(5 * time.Minute),
			snapshot:   `{"pool":"default","global_available":0}`,
		},
		{
			reason:     string(CapacityConstraintLane),
			lane:       "In Progress",
			decisionAt: now.Add(time.Minute),
			snapshot:   `{"pool":"default"}`,
		},
		{
			reason:     string(CapacityConstraintLane),
			lane:       "In Progress",
			decisionAt: now.Add(3 * time.Minute),
			snapshot:   `{"pool":"default"}`,
		},
		{
			reason:     string(CapacityConstraintLane),
			lane:       "In Progress",
			decisionAt: now.Add(6 * time.Minute),
			snapshot:   `{"pool":"default"}`,
		},
	}
	for _, row := range rows {
		if _, err := backend.RecordSchedulerDecision(ctx, SchedulerDecision{
			ProjectID:            "video",
			Lane:                 row.lane,
			Result:               SchedulerDecisionResultSkipped,
			Reason:               row.reason,
			DecisionAt:           row.decisionAt,
			WaitReason:           row.reason,
			CapacitySnapshotJSON: row.snapshot,
		}); err != nil {
			t.Fatalf("RecordSchedulerDecision() error = %v", err)
		}
	}

	waits, err := QueryCapacityConstraintWaits(ctx, backend.(*sqliteStore).db, CapacityConstraintQuery{
		Since:          now.Add(-time.Hour),
		ProjectClasses: map[string]string{"video": "cloud-only"},
	})
	if err != nil {
		t.Fatalf("QueryCapacityConstraintWaits() error = %v", err)
	}
	counts := map[CapacityConstraintReason]int{}
	for _, wait := range waits {
		counts[wait.Reason] += wait.WaitCount
	}
	if counts[CapacityConstraintPool] != 2 || counts[CapacityConstraintLane] != 2 {
		t.Fatalf("normalized counts = %#v, want two five-minute samples for pool and lane", counts)
	}
}
