package store

import (
	"testing"
	"time"
)

func TestScratchRetentionState(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                         string
		registered, terminal, reaped bool
	}{
		{name: "absent"}, {name: "active", registered: true}, {name: "terminal", registered: true, terminal: true}, {name: "reaped terminal", registered: true, terminal: true, reaped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, ok := openTestStore(t, t.Context()).(*sqliteStore)
			if !ok {
				t.Fatal("expected SQLite store")
			}
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			path := "/workspace/.detent/worker-tmp/attempt-uuid"
			if test.registered {
				id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: "project", IssueID: "issue", WorkerType: "implement", StartedAt: now})
				if err != nil {
					t.Fatal(err)
				}
				session, err := backend.StartSession(t.Context(), SessionStart{IssueID: "issue", StartedAt: now, WorkAttemptID: id})
				if err != nil {
					t.Fatal(err)
				}
				if err := backend.UpdateSessionWorkerProcess(t.Context(), session, WorkerProcessRegistration{WorkerProcessIdentity: WorkerProcessIdentity{PID: 123, StartedAt: now}, CleanupPath: path}); err != nil {
					t.Fatal(err)
				}
				if test.terminal {
					if err := backend.CompleteWorkAttempt(t.Context(), WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(time.Minute), Status: WorkAttemptStatusTerminal, TerminalState: WorkAttemptTerminalSuccess}); err != nil {
						t.Fatal(err)
					}
				}
				if test.reaped {
					if _, err := backend.db.ExecContext(t.Context(), "UPDATE codex_sessions SET worker_reaped_at = ? WHERE id = ?", now.Format(time.RFC3339Nano), session); err != nil {
						t.Fatal(err)
					}
				}
			}
			registered, terminal, err := backend.ScratchRetentionState(t.Context(), path)
			if err != nil || registered != test.registered || terminal != test.terminal {
				t.Fatalf("got (%v,%v,%v)", registered, terminal, err)
			}
		})
	}
}
