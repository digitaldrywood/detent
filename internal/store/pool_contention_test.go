package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestQueryCrossClassPoolContention(t *testing.T) {
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
	sqliteBackend := backend.(*sqliteStore)

	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	rows := []struct {
		name       string
		projectID  string
		startedAt  time.Time
		waitReason string
		snapshot   string
	}{
		{
			name:       "cross class cloud wait",
			projectID:  "video",
			startedAt:  now.Add(-24 * time.Hour),
			waitReason: poolCapacityWaitReason,
			snapshot:   `{"pool":"default","holders":["detent"]}`,
		},
		{
			name:       "cross class wait counts once per holder class",
			projectID:  "video",
			startedAt:  now.Add(-48 * time.Hour),
			waitReason: poolCapacityWaitReason,
			snapshot:   `{"pool":"default","holders":["detent","gopher-ai"]}`,
		},
		{
			name:       "same class wait",
			projectID:  "video",
			startedAt:  now.Add(-72 * time.Hour),
			waitReason: poolCapacityWaitReason,
			snapshot:   `{"pool":"default","holders":["podcast"]}`,
		},
		{
			name:       "outside seven day window",
			projectID:  "video",
			startedAt:  now.Add(-8 * 24 * time.Hour),
			waitReason: poolCapacityWaitReason,
			snapshot:   `{"pool":"default","holders":["detent"]}`,
		},
		{
			name:       "different wait reason",
			projectID:  "video",
			startedAt:  now.Add(-time.Hour),
			waitReason: "provider_capacity",
			snapshot:   `{"pool":"default","holders":["detent"]}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
				ProjectID:              row.projectID,
				WorkerType:             "implement",
				StartedAt:              row.startedAt,
				WaitReason:             row.waitReason,
				CapacitySnapshotJSON:   row.snapshot,
				WorkerMetadataJSON:     "{}",
				GitHubRateSnapshotJSON: "{}",
				MetricsJSON:            "{}",
			}); err != nil {
				t.Fatalf("StartWorkAttempt() error = %v", err)
			}
		})
	}
	if _, err := backend.RecordSchedulerDecision(ctx, SchedulerDecision{
		ProjectID:            "video",
		Result:               SchedulerDecisionResultSkipped,
		DecisionAt:           now.Add(-30 * time.Minute),
		WaitReason:           poolCapacityWaitReason,
		CapacitySnapshotJSON: `{"pool":"default","holders":["detent"]}`,
	}); err != nil {
		t.Fatalf("RecordSchedulerDecision() error = %v", err)
	}

	got, err := QueryCrossClassPoolContention(ctx, sqliteBackend.db, PoolContentionQuery{
		Since: now.Add(-7 * 24 * time.Hour),
		ProjectClasses: map[string]string{
			"detent":    "local-heavy",
			"gopher-ai": "local-heavy",
			"video":     "cloud-only",
			"podcast":   "cloud-only",
		},
	})
	if err != nil {
		t.Fatalf("QueryCrossClassPoolContention() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("contention = %#v, want one cross-class aggregate", got)
	}
	if got[0].Pool != "default" || got[0].WaitingClass != "cloud-only" ||
		got[0].HoldingClass != "local-heavy" || got[0].WaitCount != 3 {
		t.Fatalf("contention[0] = %#v, want three cloud-only waits held by local-heavy", got[0])
	}
}
