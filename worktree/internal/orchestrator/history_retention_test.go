package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

type retentionWriteContext struct {
	deadline func() (time.Time, bool)
	done     func() <-chan struct{}
	err      func() error
	value    func(any) any
	entered  chan struct{}
	once     sync.Once
}

func (c *retentionWriteContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.done()
}

func (c *retentionWriteContext) Deadline() (time.Time, bool) { return c.deadline() }
func (c *retentionWriteContext) Err() error                  { return c.err() }
func (c *retentionWriteContext) Value(key any) any           { return c.value(key) }

func newRetentionWriteContext(ctx context.Context, entered chan struct{}) context.Context {
	return &retentionWriteContext{deadline: ctx.Deadline, done: ctx.Done, err: ctx.Err, value: ctx.Value, entered: entered}
}

type retentionRecoveryStore struct {
	store.WorkAttemptStore
	store.RuntimeHistoryMaintainer
	timeoutEntered chan struct{}
	timeoutErrors  chan error
}

func (s *retentionRecoveryStore) TimeoutExpiredWorkAttempts(ctx context.Context, attrs store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	attempts, err := s.WorkAttemptStore.TimeoutExpiredWorkAttempts(newRetentionWriteContext(ctx, s.timeoutEntered), attrs)
	s.timeoutErrors <- err
	return attempts, err
}

func TestHistoryRetentionDoesNotStarveHeartbeatOrOrphanRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "attempts.db")
	inDelete := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	probe := fmt.Sprintf("retention_overlap_%x", sha256.Sum256([]byte(path)))
	if err := sqlite.RegisterScalarFunction(probe, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		close(inDelete)
		select {
		case <-release:
			return int64(1), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	ids := map[string]int64{}
	for _, issue := range []string{"old", "heartbeat", "orphan", "running", "recoverable"} {
		id, err := db.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "project-a", IssueID: issue, Identifier: "example-project/" + issue, WorkerType: "implement", StartedAt: old, LeaseExpiresAt: old.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		ids[issue] = id
		if issue == "old" || issue == "recoverable" {
			terminal, class := store.WorkAttemptTerminalSuccess, ""
			if issue == "recoverable" {
				terminal, class = store.WorkAttemptTerminalAbandoned, "service_restart"
			}
			if err := db.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: id, CompletedAt: old, TerminalState: terminal, ErrorClass: class}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO workflow_phase_events(project_id,issue_id,phase_type,phase_name,started_at,finished_at,event_day) VALUES ('project-a',?,'lane','implement',?,?,'2026-09-20')`, issue, old.Format(time.RFC3339Nano), old.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER retention_overlap BEFORE DELETE ON workflow_phase_events WHEN OLD.issue_id='old' BEGIN SELECT `+probe+`(); END`); err != nil {
		t.Fatal(err)
	}
	attempts := &retentionRecoveryStore{WorkAttemptStore: db, RuntimeHistoryMaintainer: db.(store.RuntimeHistoryMaintainer), timeoutEntered: make(chan struct{}), timeoutErrors: make(chan error, 1)}
	var logs bytes.Buffer
	cfg := normalizeConfig(Config{PollInterval: 30 * time.Second})
	cfg.Project.ID = "project-a"
	o := &Orchestrator{cfg: cfg, workAttempts: attempts, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	state := newState(cfg)
	for _, issue := range []string{"heartbeat", "running"} {
		state.Running[issue] = Running{Issue: connector.Issue{ID: issue}, WorkAttemptID: ids[issue]}
	}
	o.startWorkspaceCleanup(ctx, &state, now)
	defer o.stopWorkspaceCleanup()
	defer unblock()
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	wait(inDelete)
	heartbeatEntered := make(chan struct{})
	heartbeatDone := make(chan struct{})
	var heartbeatErr error
	go func() {
		defer close(heartbeatDone)
		heartbeatErr = db.RecordWorkAttemptHeartbeat(newRetentionWriteContext(ctx, heartbeatEntered), store.WorkAttemptHeartbeat{AttemptID: ids["heartbeat"], HeartbeatAt: now, LeaseExpiresAt: now.Add(time.Hour)})
	}()
	recoveryDone := make(chan struct{})
	go func() { defer close(recoveryDone); o.expireOrphanedWorkAttempts(ctx, &state, now) }()
	defer func() { unblock(); <-heartbeatDone; <-recoveryDone }()
	wait(heartbeatEntered)
	wait(attempts.timeoutEntered)
	unblock()
	wait(heartbeatDone)
	wait(recoveryDone)
	if heartbeatErr != nil {
		t.Fatalf("heartbeat during retention: %v", heartbeatErr)
	}
	if err := <-attempts.timeoutErrors; err != nil {
		t.Fatalf("orphan recovery during retention: %v", err)
	}
	finishTestWorkspaceCleanup(t, o, &state)
	o.workspaceCleanupWG.Wait()
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if logs.Len() != 0 {
		t.Fatalf("retention/recovery logged errors: %s", logs.String())
	}
	for _, issue := range []string{"heartbeat", "orphan", "running", "recoverable"} {
		attempt, err := db.WorkAttempt(ctx, ids[issue])
		if err != nil {
			t.Fatal(err)
		}
		switch issue {
		case "heartbeat":
			if !attempt.HeartbeatAt.Equal(now) || !attempt.LeaseExpiresAt.Equal(now.Add(time.Hour)) || attempt.Status != store.WorkAttemptStatusActive {
				t.Errorf("heartbeat did not persist: %+v", attempt)
			}
		case "orphan":
			if attempt.Status != store.WorkAttemptStatusTerminal || attempt.TerminalState != store.WorkAttemptTerminalTimedOut || attempt.ErrorClass != "lease_expired" {
				t.Errorf("orphan was not terminalized: %+v", attempt)
			}
		case "running":
			if attempt.Status != store.WorkAttemptStatusActive {
				t.Errorf("running attempt expired: %+v", attempt)
			}
		case "recoverable":
			if attempt.TerminalState != store.WorkAttemptTerminalAbandoned || attempt.ErrorClass != "service_restart" {
				t.Errorf("recoverable attempt changed: %+v", attempt)
			}
		}
	}
	if len(state.WorkAttempts) != 1 || state.WorkAttempts[0].AttemptID != ids["orphan"] || !state.WorkAttempts[0].Stale {
		t.Fatalf("recovered state missing or includes running attempts: %+v", state.WorkAttempts)
	}
	for _, issue := range []string{"old", "running", "recoverable"} {
		var count int
		if err := raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_phase_events WHERE issue_id=?", issue).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 1
		if issue == "old" {
			want = 0
		}
		if count != want {
			t.Errorf("%s events=%d want %d", issue, count, want)
		}
	}
}
