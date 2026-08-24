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
	if len(states) != 1 || states[0].RemindedAt == nil || states[0].AcknowledgedAt == nil || states[0].LastSeenAt == nil {
		t.Fatalf("states = %#v, want persisted reminder and acknowledgement", states)
	}
	if !states[0].RemindedAt.Equal(remindedAt) || !states[0].AcknowledgedAt.Equal(acknowledgedAt) {
		t.Fatalf("state timestamps = %#v, want %v and %v", states[0], remindedAt, acknowledgedAt)
	}
	if !states[0].LastSeenAt.Equal(acknowledgedAt) {
		t.Fatalf("last seen = %v, want acknowledgement time %v", states[0].LastSeenAt, acknowledgedAt)
	}
}

func TestReconcileStalenessWarningStatesRetainsOnlyLiveEpisodes(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	inactiveBefore := now.Add(-30 * 24 * time.Hour)
	tests := []struct {
		name           string
		acknowledgedAt time.Time
		active         []string
		recur          bool
		wantState      bool
		wantLastSeen   time.Time
	}{
		{name: "recently inactive survives transient absence", acknowledgedAt: now.Add(-time.Hour), wantState: true, wantLastSeen: now.Add(-time.Hour)},
		{name: "recently active refreshes last seen", acknowledgedAt: inactiveBefore.Add(time.Second), active: []string{"warning-1"}, wantState: true, wantLastSeen: now},
		{name: "expired active acknowledgement cannot be revived", acknowledgedAt: inactiveBefore.Add(-time.Second), active: []string{"warning-1"}, wantState: false},
		{name: "stale inactive is pruned", acknowledgedAt: inactiveBefore.Add(-time.Second)},
		{name: "stale recurring episode resurfaces", acknowledgedAt: inactiveBefore.Add(-time.Second), recur: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend, err := Open(t.Context(), Config{Path: filepath.Join(t.TempDir(), "detent.db")})
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			if err := backend.AcknowledgeStalenessWarning(t.Context(), "detent", "warning-1", tt.acknowledgedAt); err != nil {
				t.Fatalf("AcknowledgeStalenessWarning() error = %v", err)
			}
			states, err := backend.ReconcileStalenessWarningStates(t.Context(), "detent", tt.active, now, inactiveBefore)
			if err != nil {
				t.Fatalf("ReconcileStalenessWarningStates() error = %v", err)
			}
			if tt.recur {
				states, err = backend.ReconcileStalenessWarningStates(t.Context(), "detent", []string{"warning-1"}, now.Add(time.Minute), inactiveBefore.Add(time.Minute))
				if err != nil {
					t.Fatalf("ReconcileStalenessWarningStates(recurred) error = %v", err)
				}
			}
			if got := len(states) == 1; got != tt.wantState {
				t.Fatalf("state present = %t, want %t; states = %#v", got, tt.wantState, states)
			}
			if !tt.wantState {
				return
			}
			if states[0].LastSeenAt == nil || !states[0].LastSeenAt.Equal(tt.wantLastSeen) {
				t.Fatalf("LastSeenAt = %v, want %v", states[0].LastSeenAt, tt.wantLastSeen)
			}
		})
	}
}
