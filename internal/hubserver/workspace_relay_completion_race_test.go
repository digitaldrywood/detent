package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresWorkspaceRelayCompletionSerializesLeaseRelease(t *testing.T) {
	t.Parallel()
	for _, workerHTTP := range []bool{false, true} {
		name := "relay"
		if workerHTTP {
			name = "worker HTTP report"
		}
		t.Run(name, func(t *testing.T) {
			f := newRelayFixture(t, withExec)
			runner := f.dialRunner(t)
			person := f.dialPerson(t)
			action := f.action(t, "Example completion", "printf example")
			controlAction := f.action(t, "Example control", "printf control")
			run := f.queuedRun(t, action)
			controlRun := f.queuedRun(t, controlAction)
			person.send(execRunFrame(run, action))
			forwarded := runner.receive()
			person.send(execRunFrame(controlRun, controlAction))
			controlForwarded := runner.receive()
			if forwarded.Type != workspacesession.TypeExecRun || forwarded.Stream == "" || controlForwarded.Type != workspacesession.TypeExecRun || controlForwarded.Stream == "" {
				t.Fatalf("admitted frames: target=%+v control=%+v", forwarded, controlForwarded)
			}
			relay := f.service.workspaces.relay
			relay.mu.Lock()
			room := relay.rooms[f.workspace]
			connection := room.runner
			state := room.execRuns[forwarded.Stream]
			var snapshot execRunState
			if state != nil {
				snapshot = *state
				snapshot.output = append([]byte(nil), state.output...)
			}
			relay.mu.Unlock()
			if connection == nil || state == nil {
				t.Fatal("completion has no admitted runner or execution state")
			}
			ctx, cancel := context.WithCancel(t.Context())
			admin := workspaceTestDB(t, f.service)
			blocker, err := admin.BeginTx(ctx, nil)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			releaseCommit := make(chan struct{})
			var commitOnce sync.Once
			t.Cleanup(func() {
				commitOnce.Do(func() { close(releaseCommit) })
				cancel()
				_ = blocker.Rollback()
				workers.Wait()
			})
			if _, err := blocker.ExecContext(ctx, `SELECT id FROM project_action_runs WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, f.project.OrganizationID, f.project.ID, run); err != nil {
				t.Fatal(err)
			}
			var blockerPID int
			if err := blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			releaseReady := make(chan struct{})
			releaseResult := make(chan error, 1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				releaseResult <- f.service.database.postgres.WriteLeases(ctx, f.project.OrganizationID, corestore.Background, func(w leasestore.Writer) error {
					lease, err := w.Release(ctx, leasestore.Fence{LeaseID: string(f.lease.ID), FencingToken: int64(f.lease.FencingToken)}, "released")
					if err != nil {
						return err
					}
					if lease.ReleasedAt == nil {
						return errors.New("lease release has no release time")
					}
					close(releaseReady)
					select {
					case <-releaseCommit:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-releaseReady:
			case err := <-releaseResult:
				t.Fatalf("lease release did not reach its commit barrier: %v", err)
			}
			completionResult := make(chan error, 1)
			response := httptest.NewRecorder()
			code := 0
			request := newHubAPIRequest(t, http.MethodPost, f.base+"/workspaces/"+f.workspace+"/worker/action-runs", f.runner.redemption.Credential,
				workspaceActionRunReport{WorkspaceIdentity: workspaceIdentity(f.lease), ActionID: action.ID, RunID: run, Status: workspacesession.RunSucceeded, ExitCode: &code}).WithContext(ctx)
			workers.Add(1)
			go func() {
				defer workers.Done()
				if workerHTTP {
					f.service.Handler().ServeHTTP(response, request)
					completionResult <- nil
					return
				}
				completionResult <- f.service.workspaces.finishRelayActionRunRecord(ctx, &snapshot, connection, 0)
			}()
			testenv.WaitUntil(t, ctx, func() error {
				select {
				case err := <-completionResult:
					t.Fatalf("completion returned before the transaction barriers: %v", err)
				default:
				}
				var waiting bool
				if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND ((query LIKE '-- name: LockLease :one%' AND cardinality(pg_blocking_pids(pid))>0) OR (query LIKE '-- name: ReportActionRun :one%' AND $1=ANY(pg_blocking_pids(pid)))))`, blockerPID).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("completion has not reached its lease or action row lock")
				}
				return nil
			})
			commitOnce.Do(func() { close(releaseCommit) })
			if err := <-releaseResult; err != nil {
				t.Fatal(err)
			}
			if err := blocker.Rollback(); err != nil {
				t.Fatal(err)
			}
			err = <-completionResult
			if workerHTTP {
				requireNativeCode(t, response, http.StatusConflict, "stale_execution")
			} else {
				var refusal *nativeError
				if !errors.As(err, &refusal) || refusal.Code != "stale_execution" {
					t.Errorf("completion after concurrent lease release=%v, want stale_execution", err)
				}
			}
			for _, id := range []string{run, controlRun} {
				row := f.runRow(t, id)
				if row.Status != workspacesession.RunRunning || row.ExitCode != nil || row.FinishedAt != nil {
					t.Errorf("lease release published an action completion: %+v", row)
				}
			}
		})
	}
}
