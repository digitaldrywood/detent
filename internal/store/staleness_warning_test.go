package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStalenessWarningStatePersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "runtime.db")
	remindedAt := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	acknowledgedAt := remindedAt.Add(time.Hour)

	first, err := Open(t.Context(), Config{Path: path})
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if err := first.RecordStalenessWarningReminder(t.Context(), "detent", "warning-1", remindedAt); err != nil {
		t.Fatalf("RecordStalenessWarningReminder() error = %v", err)
	}
	if err := first.AcknowledgeStalenessWarning(t.Context(), "detent", "warning-1", acknowledgedAt); err != nil {
		t.Fatalf("AcknowledgeStalenessWarning() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	second, err := Open(t.Context(), Config{Path: path})
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	states, err := second.ListStalenessWarningStates(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ListStalenessWarningStates() error = %v", err)
	}
	if len(states) != 1 || states[0].RemindedAt == nil || states[0].AcknowledgedAt == nil {
		t.Fatalf("states = %#v, want persisted reminder and acknowledgement", states)
	}
	if !states[0].RemindedAt.Equal(remindedAt) || !states[0].AcknowledgedAt.Equal(acknowledgedAt) {
		t.Fatalf("state timestamps = %#v, want %v and %v", states[0], remindedAt, acknowledgedAt)
	}
}
