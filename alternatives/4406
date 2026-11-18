package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

type blockedHeartbeatStore struct {
	store.WorkAttemptStore
	calls    int
	recorded int
}

func (s *blockedHeartbeatStore) RecordWorkAttemptHeartbeat(ctx context.Context, h store.WorkAttemptHeartbeat) error {
	s.calls++
	if s.calls == 1 {
		<-ctx.Done()
	}
	err := ctx.Err()
	if err == nil {
		err = s.WorkAttemptStore.RecordWorkAttemptHeartbeat(ctx, h)
	}
	if err == nil {
		s.recorded++
	}
	return err
}

func TestHeartbeatWriteDeadlineRecovery(t *testing.T) {
	for _, path := range []string{"dedicated", "tick", "progress"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC().Truncate(time.Second)
				const id int64 = 1
				db := &checkpointAttemptStore{attempt: store.WorkAttempt{
					ID: id, ProjectID: "test", IssueID: "worker", WorkerType: "implement",
					Status: store.WorkAttemptStatusActive, StartedAt: now.Add(-time.Minute),
					HeartbeatAt: now.Add(-time.Minute), LeaseExpiresAt: now.Add(time.Minute),
				}}
				backend := &blockedHeartbeatStore{WorkAttemptStore: db}
				var logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, nil))
				running := Running{Issue: connector.Issue{ID: "worker", State: "In Progress"}, WorkAttemptID: id, Generation: 1, Mode: runner.RunModeImplement}
				h := store.WorkAttemptHeartbeat{AttemptID: id, HeartbeatAt: now, LeaseExpiresAt: now.Add(time.Minute)}
				ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
				defer cancel()
				switch path {
				case "dedicated":
					m := newHeartbeatManager(Config{}, nil, backend, func() time.Time { return now }, logger)
					target := heartbeatTarget{issueID: "worker", sequence: 1, workAttemptHeartbeat: h}
					m.targets["worker"] = target
					m.execute(ctx, target)
				case "tick":
					o := &Orchestrator{workAttempts: backend, logger: logger}
					o.heartbeatRunningWorkAttempts(ctx, &State{Running: map[string]Running{"worker": running}}, now)
				case "progress":
					p := newWorkerProgress(running, h, backend, 4096)
					if err := p.observe(ctx, runner.UsageUpdate{CompletionSourceStart: &runner.CompletionSourceStartSnapshot{WorkspaceDiffAvailable: true}}); err != nil {
						t.Error(err)
					}
				}
				if backend.calls != 2 || backend.recorded != 1 {
					t.Errorf("calls=%d recorded=%d, want 2/1", backend.calls, backend.recorded)
				}
				if strings.Contains(logs.String(), "WARN") {
					t.Errorf("unexpected warning: %s", logs.String())
				}
				attempt, err := db.WorkAttempt(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if attempt.HeartbeatAt.Before(now) {
					t.Errorf("heartbeat did not advance: %v", attempt.HeartbeatAt)
				}
			})
		})
	}
}
