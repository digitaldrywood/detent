package store

import (
	"context"
	"testing"
	"time"

	routinemodel "github.com/digitaldrywood/detent/internal/routine/model"
)

func TestRoutineRunRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	record := routinemodel.RunRecord{
		ProjectID: "detent", RoutineName: "dependencies", ScheduledFor: base,
		StartedAt: base.Add(time.Minute), CompletedAt: base.Add(2 * time.Minute), Filed: 1, Deduplicated: 2,
		Issues: []routinemodel.IssueRecord{{ID: "I_1", Identifier: "#1400", URL: "https://example.test/issues/1400"}},
		Error:  "one proposal failed",
	}
	if err := backend.RecordRoutineRun(ctx, record); err != nil {
		t.Fatalf("RecordRoutineRun() error = %v", err)
	}
	got, found, err := backend.LatestRoutineRun(ctx, "detent", "dependencies")
	if err != nil {
		t.Fatalf("LatestRoutineRun() error = %v", err)
	}
	if !found || got.ProjectID != record.ProjectID || got.RoutineName != record.RoutineName || got.Filed != 1 || got.Deduplicated != 2 || got.Error != record.Error {
		t.Fatalf("LatestRoutineRun() = %#v, %t", got, found)
	}
	if !got.ScheduledFor.Equal(record.ScheduledFor) || !got.StartedAt.Equal(record.StartedAt) || !got.CompletedAt.Equal(record.CompletedAt) {
		t.Fatalf("timestamps = %#v, want %#v", got, record)
	}
	if len(got.Issues) != 1 || got.Issues[0] != record.Issues[0] {
		t.Fatalf("issues = %#v, want %#v", got.Issues, record.Issues)
	}
	_, found, err = backend.LatestRoutineRun(ctx, "detent", "missing")
	if err != nil || found {
		t.Fatalf("missing LatestRoutineRun() found = %t, error = %v", found, err)
	}
}
