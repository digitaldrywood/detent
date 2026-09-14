package store

import (
	"testing"
	"time"
)

func TestProtectedCodexSessions(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name               string
		started, completed *time.Time
		attempt            bool
		want               bool
	}{
		{name: "old completed", started: new(now.Add(-40 * 24 * time.Hour)), completed: new(now.Add(-39 * 24 * time.Hour))},
		{name: "recent session", started: new(now.Add(-24 * time.Hour)), completed: new(now), want: true},
		{name: "recent completion", started: new(now.Add(-40 * 24 * time.Hour)), completed: new(now), want: true},
		{name: "active session", started: new(now.Add(-40 * 24 * time.Hour)), want: true},
		{name: "unknown age", completed: new(now.Add(-40 * 24 * time.Hour)), want: true},
		{name: "active attempt", attempt: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := openTestStore(t, t.Context()).(*sqliteStore)
			if tt.attempt {
				_, err := backend.db.ExecContext(t.Context(), `INSERT INTO work_attempts(project_id,worker_type,status,started_at,provider_session_id) VALUES ('other-project','implement','running',?,'thread')`, now.Add(-40*24*time.Hour).Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var started, completed *string
				if tt.started != nil {
					started = new(tt.started.Format(time.RFC3339Nano))
				}
				if tt.completed != nil {
					completed = new(tt.completed.Format(time.RFC3339Nano))
				}
				_, err := backend.db.ExecContext(t.Context(), `INSERT INTO codex_sessions(started_at,completed_at,provider_session_id) VALUES (?,?,'thread')`, started, completed)
				if err != nil {
					t.Fatal(err)
				}
			}
			protected, err := backend.ProtectedCodexSessions(t.Context(), now.Add(-30*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if protected["thread"] != tt.want {
				t.Fatalf("protected=%v want %v", protected, tt.want)
			}
		})
	}
}
