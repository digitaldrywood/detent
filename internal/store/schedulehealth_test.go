package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/schedulehealth"
)

func TestSQLiteScheduledRunLedger(t *testing.T) {
	t.Parallel()

	backend, err := Open(context.Background(), Config{Backend: BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	at := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	record := schedulehealth.Run{
		ProjectID: "detent", ScheduleID: schedulehealth.AdmissionID,
		ScheduledFor: at, StartedAt: at.Add(time.Second), CompletedAt: at.Add(2 * time.Second), Error: "example failure",
	}
	if err := backend.RecordScheduledRun(t.Context(), record); err != nil {
		t.Fatalf("RecordScheduledRun() error = %v", err)
	}
	got, found, err := backend.LatestScheduledRun(t.Context(), "detent", schedulehealth.AdmissionID)
	if err != nil {
		t.Fatalf("LatestScheduledRun() error = %v", err)
	}
	if !found || got.ScheduleID != record.ScheduleID || !got.CompletedAt.Equal(record.CompletedAt) || got.Error != record.Error {
		t.Fatalf("LatestScheduledRun() = %#v, %t, want %#v", got, found, record)
	}
}
