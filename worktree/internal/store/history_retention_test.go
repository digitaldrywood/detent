package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func seedRetentionAttempt(t *testing.T, s *sqliteStore, project, issue string, at time.Time, terminal, recoverable bool) int64 {
	t.Helper()
	id, err := s.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: project, IssueID: issue, WorkerType: "implement", StartedAt: at, LeaseExpiresAt: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if terminal {
		state := WorkAttemptTerminalSuccess
		errorClass := ""
		if recoverable {
			state = WorkAttemptTerminalAbandoned
			errorClass = "service_restart"
		}
		if err := s.CompleteWorkAttempt(t.Context(), WorkAttemptCompletion{AttemptID: id, CompletedAt: at, TerminalState: state, ErrorClass: errorClass}); err != nil {
			t.Fatal(err)
		}
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	for _, query := range []string{
		"INSERT INTO workflow_phase_events(project_id,issue_id,phase_type,phase_name,started_at,finished_at,event_day) VALUES (?,?,'lane','implement',?,?, '2026-01-01')",
		"INSERT INTO scheduler_decisions(project_id,issue_id,result,decision_at,reason) VALUES (?,?,'selected',?,?)",
	} {
		if _, err := s.db.ExecContext(t.Context(), query, project, issue, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func retentionCount(t *testing.T, s *sqliteStore, table, project, issue string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE project_id=? AND issue_id=?", project, issue).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestRuntimeHistoryRetentionProtectsCurrentWork(t *testing.T) {
	s := openTestStore(t, t.Context()).(*sqliteStore)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, project := range []string{"project-a", "project-b"} {
		for _, tc := range []struct {
			name                  string
			age                   time.Duration
			terminal, recoverable bool
		}{
			{"old", 31 * 24 * time.Hour, true, false},
			{"recent", time.Hour, true, false},
			{"boundary", 30 * 24 * time.Hour, true, false},
			{"active", 40 * 24 * time.Hour, false, false},
			{"recoverable", 40 * 24 * time.Hour, true, true},
			{"cleanup", 40 * 24 * time.Hour, true, false},
		} {
			id := seedRetentionAttempt(t, s, project, tc.name, now.Add(-tc.age), tc.terminal, tc.recoverable)
			if tc.name == "cleanup" {
				session, err := s.StartSession(t.Context(), SessionStart{ProjectID: project, IssueID: tc.name, WorkAttemptID: id, StartedAt: now.Add(-tc.age)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.ExecContext(t.Context(), "UPDATE codex_sessions SET completed_at=?, final_state='completed', worker_pid=123, worker_cleanup_path='scratch' WHERE id=?", now.Add(-tc.age).Format(time.RFC3339Nano), session); err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.ExecContext(t.Context(), "UPDATE workflow_phase_events SET session_id=? WHERE project_id=? AND issue_id=?", session, project, tc.name); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := s.PruneRuntimeHistory(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"project-a", "project-b"} {
		for _, issue := range []string{"old", "recent", "boundary", "active", "recoverable", "cleanup"} {
			for _, table := range []string{"work_attempts", "workflow_phase_events", "scheduler_decisions"} {
				want := 1
				if issue == "old" || (issue == "cleanup" && table == "scheduler_decisions") {
					want = 0
				}
				if got := retentionCount(t, s, table, project, issue); got != want {
					t.Errorf("%s/%s/%s = %d want %d", project, issue, table, got, want)
				}
			}
		}
	}
}

func TestRuntimeHistoryRetentionBatchesAndConcurrentWrites(t *testing.T) {
	s := openTestStore(t, t.Context()).(*sqliteStore)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	const total = runtimeHistoryBatch + 17
	var maxID int64
	for range total {
		maxID = seedRetentionAttempt(t, s, "project-a", "old", now.Add(-40*24*time.Hour), true, false)
	}
	heartbeat := seedRetentionAttempt(t, s, "project-b", "heartbeat", now, false, false)
	expired := seedRetentionAttempt(t, s, "project-b", "expired", now.Add(-time.Hour*2), false, false)
	start := make(chan struct{})
	results := make(chan error, 3)
	go func() { <-start; results <- s.PruneRuntimeHistory(t.Context(), now) }()
	go func() {
		<-start
		results <- s.RecordWorkAttemptHeartbeat(t.Context(), WorkAttemptHeartbeat{AttemptID: heartbeat, HeartbeatAt: now, LeaseExpiresAt: now.Add(time.Hour)})
	}()
	go func() {
		<-start
		_, err := s.TimeoutExpiredWorkAttempts(t.Context(), WorkAttemptTimeout{ProjectID: "project-b", Now: now})
		results <- err
	}()
	close(start)
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"work_attempts", "workflow_phase_events", "scheduler_decisions"} {
		if got := retentionCount(t, s, table, "project-a", "old"); got != 17 {
			t.Errorf("%s remaining=%d want 17", table, got)
		}
	}
	if err := s.PruneRuntimeHistory(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"work_attempts", "workflow_phase_events", "scheduler_decisions"} {
		if got := retentionCount(t, s, table, "project-a", "old"); got != 0 {
			t.Errorf("%s remaining=%d", table, got)
		}
	}
	attempt, err := s.WorkAttempt(t.Context(), expired)
	if err != nil || attempt.Status != WorkAttemptStatusTerminal {
		t.Fatalf("expired attempt: %+v %v", attempt, err)
	}
	fresh := seedRetentionAttempt(t, s, "project-a", "fresh", now, true, false)
	if fresh <= maxID {
		t.Fatalf("reused ID %d <= %d", fresh, maxID)
	}
}

func TestRuntimeHistoryRetentionReclaimsPagesAndPreservesIDs(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			s := openTestStore(t, t.Context()).(*sqliteStore)
			if legacy {
				if _, err := s.db.ExecContext(t.Context(), "PRAGMA auto_vacuum=NONE"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.ExecContext(t.Context(), "VACUUM"); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			pageCount := func() int {
				var n int
				if err := s.db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			seed := func() int64 {
				id := seedRetentionAttempt(t, s, "project-a", "old", now.Add(-40*24*time.Hour), true, false)
				if _, err := s.db.ExecContext(t.Context(), "UPDATE scheduler_decisions SET reason=? WHERE project_id='project-a' AND issue_id='old'", strings.Repeat("x", 100000)); err != nil {
					t.Fatal(err)
				}
				return id
			}
			active := seedRetentionAttempt(t, s, "project-a", "active", now, false, false)
			recoverable := seedRetentionAttempt(t, s, "project-b", "recoverable", now.Add(-40*24*time.Hour), true, true)
			first := seed()
			if legacy {
				path := s.path
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := Open(t.Context(), Config{Backend: BackendSQLite, Path: path})
				if err != nil {
					t.Fatal(err)
				}
				s = reopened.(*sqliteStore)
				t.Cleanup(func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				})
				attempt, err := s.WorkAttempt(t.Context(), first)
				if err != nil || attempt.ID != first {
					t.Fatalf("reopened attempt: %+v %v", attempt, err)
				}
			}
			fileSize := func() int64 {
				if _, err := s.db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(s.path)
				if err != nil {
					t.Fatal(err)
				}
				return info.Size()
			}
			peak := pageCount()
			peakBytes := fileSize()
			for range 100 {
				if err := s.PruneRuntimeHistory(context.Background(), now); err != nil {
					t.Fatal(err)
				}
			}
			if pageCount() >= peak {
				t.Fatalf("incremental vacuum did not reclaim pages: %d >= %d", pageCount(), peak)
			}
			if size := fileSize(); size >= peakBytes {
				t.Fatalf("retention did not shrink database: %d >= %d", size, peakBytes)
			}
			for _, id := range []int64{active, recoverable} {
				attempt, err := s.WorkAttempt(t.Context(), id)
				if err != nil || attempt.ID != id {
					t.Fatalf("protected attempt after reopen and retention: %+v %v", attempt, err)
				}
			}
			if err := s.RecordWorkAttemptHeartbeat(t.Context(), WorkAttemptHeartbeat{AttemptID: active, HeartbeatAt: now, LeaseExpiresAt: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			second := seed()
			if second <= first {
				t.Fatalf("attempt ID reused after emptying store: %d <= %d", second, first)
			}
			if pageCount() > peak+2 {
				t.Fatalf("steady-state pages grew: %d > %d", pageCount(), peak+2)
			}
		})
	}
}
