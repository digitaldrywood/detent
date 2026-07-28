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
		decisionAt time.Time
		waitReason string
		snapshot   string
	}{
		{
			name:       "global capacity full",
			projectID:  "video",
			decisionAt: now.Add(-24 * time.Hour),
			waitReason: poolCapacityWaitReason,
			snapshot:   `{"pool":"default","global_available":0,"holders":["detent"]}`,
		},
		{
			name:       "higher priority project",
			projectID:  "video",
			decisionAt: now.Add(-48 * time.Hour),
			waitReason: poolHigherPriorityProjectWaitReason,
			snapshot:   `{"pool":"default","global_available":0,"holders":["detent","gopher-ai"]}`,
		},
		{
			name:       "higher priority state",
			projectID:  "video",
			decisionAt: now.Add(-72 * time.Hour),
			waitReason: poolHigherPriorityStateWaitReason,
			snapshot:   `{"pool":"default","global_available":0,"holders":["detent"]}`,
		},
		{
			name:       "selected project waiting",
			projectID:  "video",
			decisionAt: now.Add(-96 * time.Hour),
			waitReason: poolSelectedProjectWaitReason,
			snapshot:   `{"pool":"default","global_available":0,"holders":["detent"]}`,
		},
		{
			name:       "pure arbitration with capacity",
			projectID:  "video",
			decisionAt: now.Add(-time.Hour),
			waitReason: poolHigherPriorityProjectWaitReason,
			snapshot:   `{"pool":"default","global_available":1,"holders":["detent"]}`,
		},
		{
			name:       "same class wait",
			projectID:  "video",
			decisionAt: now.Add(-time.Hour),
			waitReason: poolSelectedProjectWaitReason,
			snapshot:   `{"pool":"default","global_available":0,"holders":["podcast"]}`,
		},
		{
			name:       "outside seven day window",
			projectID:  "video",
			decisionAt: now.Add(-8 * 24 * time.Hour),
			waitReason: poolHigherPriorityProjectWaitReason,
			snapshot:   `{"pool":"default","global_available":0,"holders":["detent"]}`,
		},
		{
			name:       "different wait reason",
			projectID:  "video",
			decisionAt: now.Add(-time.Hour),
			waitReason: "provider_capacity",
			snapshot:   `{"pool":"default","global_available":0,"holders":["detent"]}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := backend.RecordSchedulerDecision(ctx, SchedulerDecision{
				ProjectID:            row.projectID,
				Result:               SchedulerDecisionResultSkipped,
				Reason:               row.waitReason,
				DecisionAt:           row.decisionAt,
				WaitReason:           row.waitReason,
				CapacitySnapshotJSON: row.snapshot,
			}); err != nil {
				t.Fatalf("RecordSchedulerDecision() error = %v", err)
			}
		})
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
		got[0].HoldingClass != "local-heavy" || got[0].WaitCount != 4 {
		t.Fatalf("contention[0] = %#v, want four exhausted cloud-only waits held by local-heavy", got[0])
	}
}
