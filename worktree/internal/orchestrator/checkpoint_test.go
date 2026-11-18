package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

type checkpointAttemptStore struct {
	recordingWorkAttemptStore
	attempt store.WorkAttempt
	err     error
}

func (s *checkpointAttemptStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	return s.attempt, s.err
}

func (s *checkpointAttemptStore) RecordWorkAttemptHeartbeat(ctx context.Context, heartbeat store.WorkAttemptHeartbeat) error {
	if s.attempt.ID != heartbeat.AttemptID {
		return store.ErrNotFound
	}
	if err := s.recordingWorkAttemptStore.RecordWorkAttemptHeartbeat(ctx, heartbeat); err != nil {
		return err
	}
	s.attempt.HeartbeatAt = heartbeat.HeartbeatAt
	s.attempt.LeaseExpiresAt = heartbeat.LeaseExpiresAt
	return nil
}

type checkpointLaneConnector struct {
	backendCapacityTestConnector
	read func() ([]connector.Issue, error)
}

func (c checkpointLaneConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return c.read()
}

func TestCheckpointValidator(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"owned", "cancelled", "no runtime", "no running", "wrong attempt", "wrong generation", "wrong mode", "no store", "no tracker", "store unavailable", "wrong issue", "terminal attempt", "expired lease", "tracker unavailable", "lane changed", "ownership lost during lookup", "runtime lost during lookup", "lease expires during lookup"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
			issue := connector.Issue{ID: "checkpoint", State: "In Progress"}
			attempts := &checkpointAttemptStore{attempt: store.WorkAttempt{ID: 23, IssueID: issue.ID, Status: store.WorkAttemptStatusActive, LeaseExpiresAt: now.Add(time.Minute)}}
			orch := &Orchestrator{workAttempts: attempts, now: func() time.Time { return now }}
			running := Running{Issue: issue, WorkAttemptID: 23, Generation: 5, Mode: runner.RunModeImplement}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			orch.connector = &checkpointLaneConnector{read: func() ([]connector.Issue, error) {
				switch scenario {
				case "tracker unavailable":
					return nil, errors.New("network unavailable")
				case "lane changed":
					issue.State = "Backlog"
				case "ownership lost during lookup":
					orch.latestRuntimeState.Store(&runtimeState{})
				case "runtime lost during lookup":
					orch.latestRuntimeState.Store(nil)
				case "lease expires during lookup":
					now = now.Add(time.Minute)
				}
				return []connector.Issue{issue}, nil
			}}
			switch scenario {
			case "cancelled":
				cancel()
			case "wrong attempt":
				running.WorkAttemptID++
			case "wrong generation":
				running.Generation++
			case "wrong mode":
				running.Mode = runner.RunModeRoutine
			case "no store":
				orch.workAttempts = nil
			case "no tracker":
				orch.connector = nil
			case "store unavailable":
				attempts.err = errors.New("store unavailable")
			case "wrong issue":
				attempts.attempt.IssueID = "other"
			case "terminal attempt":
				attempts.attempt.Status = store.WorkAttemptStatusTerminal
			case "expired lease":
				attempts.attempt.LeaseExpiresAt = now
			}
			state := &runtimeState{Running: map[string]Running{issue.ID: running}}
			if scenario == "no running" {
				state.Running = nil
			}
			if scenario != "no runtime" {
				orch.latestRuntimeState.Store(state)
			}
			err := orch.checkpointValidator(issue.ID, 23, 5)(ctx)
			if (err == nil) != (scenario == "owned" || scenario == "expired lease" || scenario == "lease expires during lookup") {
				t.Fatalf("checkpoint validation = %v", err)
			}
		})
	}
}

func TestCheckpointRenewsExpiredLease(t *testing.T) {
	for _, scenario := range []string{"owned", "with progress", "wrong generation", "lane changed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
			issue := connector.Issue{ID: "checkpoint", State: "In Progress"}
			expired := now.Add(-3 * time.Minute)
			const id int64 = 1
			db := &checkpointAttemptStore{attempt: store.WorkAttempt{
				ID: id, ProjectID: "test", IssueID: issue.ID, WorkerType: "implement",
				Status: store.WorkAttemptStatusActive, StartedAt: now.Add(-13 * time.Minute), LeaseExpiresAt: expired,
			}}
			o := &Orchestrator{workAttempts: db, now: func() time.Time { return now }}
			running := Running{Issue: issue, WorkAttemptID: id, Generation: 5, Mode: runner.RunModeImplement}
			if scenario == "with progress" {
				running.progress = newWorkerProgress(running, store.WorkAttemptHeartbeat{AttemptID: id, HeartbeatAt: now.Add(-13 * time.Minute), LeaseExpiresAt: expired}, db, 4096)
			}
			if scenario == "wrong generation" {
				running.Generation++
			}
			o.latestRuntimeState.Store(&runtimeState{Running: map[string]Running{issue.ID: running}})
			o.connector = &checkpointLaneConnector{read: func() ([]connector.Issue, error) {
				if scenario == "lane changed" {
					issue.State = "Backlog"
				}
				return []connector.Issue{issue}, nil
			}}
			err := o.checkpointValidator(issue.ID, id, 5)(t.Context())
			wantOK := scenario == "owned" || scenario == "with progress"
			if (err == nil) != wantOK {
				t.Fatalf("checkpoint: %v", err)
			}
			attempt, err := db.WorkAttempt(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if wantOK {
				if !attempt.LeaseExpiresAt.Equal(o.workAttemptLeaseExpiresAt(now)) || !attempt.HeartbeatAt.Equal(now) {
					t.Fatalf("lease not renewed: %+v", attempt)
				}
			} else if !attempt.LeaseExpiresAt.Equal(expired) {
				t.Fatalf("unowned lease renewed: %v", attempt.LeaseExpiresAt)
			}
		})
	}
}
