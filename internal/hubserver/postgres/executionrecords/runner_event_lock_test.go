package executionrecords_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerEventDoesNotDeadlockMachineHeartbeat(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			pgstoretest.SeedLeaseFamily(t, repository, org)
			now := time.Now().UTC()
			pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", now, true)
			lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: "runner_shared", LeaseID: lease.LeaseID, AssignedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			event := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, repository, org, event)
			event.Sequence, event.PayloadObjectKey = 2, "example/next"
			locked, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			heartbeat := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteRunnerHeartbeat(ctx, org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
					if err := w.HeartbeatMachine(ctx, leasestore.Machine{OrganizationID: org, ID: "machine_1", DisplayName: "updated", Capacity: 1, Version: "test"}, now); err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					return w.ReplaceRunnerProjectObservation(ctx, runnerstore.ProjectObservation{OrganizationID: org, RunnerID: "runner_shared", ProjectID: "prj_1", ObservedAt: now, SnapshotDigest: strings.Repeat("a", 64)}, nil)
				})
			})
			defer releaseOnce.Do(func() { close(release) })
			pgstoretest.WaitRepositorySignal(t, locked)
			var heartbeatPID int
			if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE 'UPDATE machines SET%'`, login).Scan(&heartbeatPID); err != nil {
				t.Fatal(err)
			}
			appendEvent := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					facts := w.(interface {
						WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
					})
					return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, AttemptID: event.ID, Operation: "append-event"}, func(w tenantstore.Writer) error {
						if _, err := w.RunnerForLease(ctx, lease.LeaseID); err != nil {
							return err
						}
						_, _, err := w.RecordAttemptEvent(ctx, event)
						return err
					})
				})
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-appendEvent.Done:
					return fmt.Errorf("event ended before lock observation: %v", appendEvent.Err)
				default:
				}
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)))`, login, heartbeatPID).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("event has not reached the heartbeat runner lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			if err := heartbeat.Wait(t); err != nil {
				t.Errorf("heartbeat (SQLSTATE=%s): %v", pgtransport.SQLState(err), err)
			}
			if err := appendEvent.Wait(t); err != nil {
				t.Errorf("append event (SQLSTATE=%s): %v", pgtransport.SQLState(err), err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				observation, _, err := r.RunnerProjectObservation(t.Context(), "runner_shared", "prj_1", "")
				if err != nil {
					return err
				}
				if observation.SnapshotDigest != strings.Repeat("a", 64) {
					return errors.New("heartbeat observation did not commit")
				}
				attempt, err := r.Attempt(t.Context(), event.ID)
				if err == nil && attempt.Sequence != 2 {
					return fmt.Errorf("attempt sequence=%d, want 2", attempt.Sequence)
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRunnerCompletionAcquiresProjectLockBeforePublication(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", pgstoretest.HistoryAt(), true)
		lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		if err := repository.WriteRunnerEventCommand(t.Context(), org, "runner_shared", "runner_shared", eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID}, func(ctx context.Context, w tenantstore.Writer) error {
			var id string
			if err := admin.QueryRowContext(ctx, `SELECT id FROM api_tokens WHERE organization_id=$1 AND id='runner_shared' FOR SHARE NOWAIT`, org).Scan(&id); err != nil {
				return fmt.Errorf("runner command blocks concurrent credential authorization: %w", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, completion := range []bool{false, true} {
			err := repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
				facts, ok := w.(interface {
					WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
				})
				if !ok {
					return fmt.Errorf("runner command facts unavailable")
				}
				return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, Completion: completion}, func(tenantstore.Writer) error {
					var project string
					err := admin.QueryRowContext(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id='prj_1' FOR SHARE NOWAIT`, org).Scan(&project)
					if completion {
						if pgtransport.SQLState(err) != pgtransport.LockTimeout {
							return fmt.Errorf("completion project lock permits concurrent shared lock: %w", err)
						}
					} else if err != nil {
						return err
					}
					return nil
				})
			})
			if err != nil {
				t.Errorf("tenant=%s completion=%t: %v", org, completion, err)
			}
		}
	}
}

func TestRunnerEventQueuedFenceDiagnostics(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION expire_queued_run_lease() RETURNS trigger LANGUAGE plpgsql AS $$DECLARE changed integer;BEGIN IF NEW.id='queue_expiry' THEN UPDATE public.leases SET acquired_at=clock_timestamp()-interval '3 hours',renewed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE organization_id=NEW.organization_id AND issue_id=(SELECT id FROM public.issues WHERE organization_id=NEW.organization_id AND project_id='prj_1' AND native_id='wi_1') AND released_at IS NULL; GET DIAGNOSTICS changed=ROW_COUNT; IF changed<>1 THEN RAISE EXCEPTION 'queue expiry scope mismatch'; END IF; END IF; RETURN NEW; END$$; CREATE TRIGGER expire_queued_run_lease BEFORE INSERT ON archive_objects FOR EACH ROW EXECUTE FUNCTION expire_queued_run_lease()`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", pgstoretest.HistoryAt(), true)
		lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		for _, test := range []struct {
			name                          string
			wrongToken, released, expired bool
		}{
			{name: "wrong token", wrongToken: true},
			{name: "released", released: true},
			{name: "expired", expired: true},
		} {
			if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '3 hours',renewed_at=clock_timestamp()-interval '2 hours',released_at=CASE WHEN $3 THEN clock_timestamp() END,expires_at=clock_timestamp()+CASE WHEN $4 THEN interval '-1 hour' ELSE interval '1 hour' END WHERE organization_id=$1 AND lease_id=$2`, org, lease.LeaseID, test.released, test.expired); err != nil {
				t.Fatal(err)
			}
			err := repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
				facts := w.(interface {
					WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
				})
				return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID}, func(w tenantstore.Writer) error {
					token := lease.FencingToken
					if test.wrongToken {
						token++
					}
					_, _, err := w.RecordAttemptEvent(ctx, leasestore.AttemptEvent{ID: "example-attempt", RunID: "example-run", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: token}, Sequence: 1, Status: "running", PayloadObjectKey: "example-object"})
					return err
				})
			})
			var fence *leasestore.FenceError
			if !errors.Is(err, tracker.ErrStaleFencingToken) || !errors.As(err, &fence) || fence.Check != "attempt_locked_identity" {
				t.Errorf("tenant=%s %s diagnostic=%v", org, test.name, err)
			}
		}
		t.Run(string(org)+"/database-time queued expiry", func(t *testing.T) {
			result, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '2 hours',renewed_at=clock_timestamp()-interval '1 hour',released_at=NULL,expires_at=clock_timestamp()+interval '1 hour' WHERE organization_id=$1 AND lease_id=$2`, org, lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("live lease rows=%d error=%v", n, err)
			}
			ref, err := pgstoretest.NewHistoryObjects().StoreDetail(t.Context(), string(org), []byte(`{"fence":"queued"}`))
			if err != nil {
				t.Fatal(err)
			}
			err = repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
				facts := w.(interface {
					WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
				})
				return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID}, func(w tenantstore.Writer) error {
					if _, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: org, ID: "queue_expiry", Reference: ref, CreatedAt: pgstoretest.HistoryAt()}); err != nil {
						return err
					}
					_, _, err := w.RecordAttemptEvent(ctx, leasestore.AttemptEvent{ID: "queued_expiry_attempt", RunID: "queued_expiry_run", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, Sequence: 1, Status: "running", PayloadObjectKey: ref.Key, PayloadReference: ref})
					return err
				})
			})
			var fence *leasestore.FenceError
			if !errors.As(err, &fence) || fence.Check != "attempt_live_lease" {
				t.Fatalf("queued database expiry=%v", err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				if _, err := r.Attempt(t.Context(), "queued_expiry_attempt"); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("expired queued attempt persisted: %v", err)
				}
				if _, err := r.ArchiveObject(t.Context(), "queue_expiry"); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("expiry preparation survived rollback: %v", err)
				}
				current, err := r.Lease(t.Context(), lease.LeaseID)
				if err == nil && (current.ReleasedAt != nil || !current.ExpiresAt.After(time.Now().UTC())) {
					t.Fatal("queued fixture expiry survived rollback")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
		t.Run(string(org)+"/activity delivery", func(t *testing.T) {
			testRunnerEventActivityDelivery(t, repository, admin, org)
		})
	}
}

func testRunnerEventActivityDelivery(t *testing.T, repository *postgres.Repository, admin *sql.DB, org tracker.OrganizationID) {
	t.Helper()
	const project tracker.ProjectID = "prj_2"
	const item tracker.NativeWorkItemID = "wi_2"
	const runner = "runner_delivery"

	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	pgstoretest.SeedRunnerIdentity(t, repository, org, runner, "machine_2", now, false)
	lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(2))
	if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.AssignRunnerLease(ctx, runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: runner, LeaseID: lease.LeaseID, AssignedAt: now})
	}); err != nil {
		t.Fatal(err)
	}

	var before viewstore.BoardSequence
	if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		before, err = r.BoardSequence(ctx, project)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
	attempt := pgstoretest.AttemptEvent(lease)
	prepare := func(id, kind string, sequence int64) eventstore.Event {
		t.Helper()
		row, err := content.PrepareEvent(ctx, org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{
			OrganizationID: org, ProjectID: project, AggregateID: item, AggregateType: "issue",
			ID: id, Type: kind, SchemaVersion: 1, RecordedAt: now,
			Actor: tracker.Actor{Kind: "runner", PrincipalID: runner},
			Data: tracker.CollaborationData{Run: &tracker.NativeRunData{
				RunID: attempt.RunID, AttemptID: attempt.ID, RunnerID: runner, Sequence: sequence,
				LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken),
			}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	started := prepare("event_delivery_started", "run.started", 1)
	failed := prepare("event_delivery_refused", "run.observed", 2)
	wakes := 0
	repository.SetBoardCommitNotifier(func() {
		wakes++
		var eventVisible, noticeVisible bool
		err := admin.QueryRowContext(ctx, `SELECT
 EXISTS (SELECT 1 FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND id=$4),
 EXISTS (SELECT 1 FROM native_board_deltas WHERE organization_id=$1 AND project_id=$2 AND topic='activity' AND entity_id=$4)`,
			org, project, item, started.ID).Scan(&eventVisible, &noticeVisible)
		if err != nil || !eventVisible || !noticeVisible {
			t.Errorf("wake before event and notice commit: event=%t notice=%t err=%v", eventVisible, noticeVisible, err)
		}
	})
	defer repository.SetBoardCommitNotifier(nil)

	write := func(event eventstore.Event, attempt leasestore.AttemptEvent, replay, reject bool) (eventstore.Event, error) {
		t.Helper()
		command := eventstore.RunCommand{
			Project: project, WorkItem: item, LeaseID: lease.LeaseID, AttemptID: attempt.ID,
			Sequences: []int64{attempt.Sequence}, EventIDs: []string{event.ID},
		}
		var stored eventstore.Event
		beforeWakes := wakes
		err := repository.WriteRunnerEventCommand(ctx, org, runner, runner, command, func(ctx context.Context, w tenantstore.Writer) error {
			if _, err := w.RunnerForLease(ctx, lease.LeaseID); err != nil {
				return err
			}
			if _, duplicate, err := w.RecordAttemptEvent(ctx, attempt); err != nil {
				return err
			} else if duplicate != replay {
				return fmt.Errorf("attempt replay=%t, want %t", duplicate, replay)
			}
			var err error
			stored, err = w.AppendEvent(ctx, event)
			if err != nil {
				return err
			}
			if wakes != beforeWakes {
				return errors.New("runner event woke delivery before commit")
			}
			if reject {
				stale := pgstoretest.LeaseFence(lease)
				stale.FencingToken++
				_, err = w.Release(ctx, stale, "stale fence")
			}
			return err
		})
		return stored, err
	}
	checkCommitted := func() {
		t.Helper()
		err := repository.ReadTenantSnapshot(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			head, err := r.BoardSequence(ctx, project)
			if err != nil {
				return err
			}
			deltas, err := r.BoardDeltas(ctx, project, before.Sequence, 2)
			if err != nil {
				return err
			}
			if head.Sequence != before.Sequence+1 || head.ReplayFloor != before.ReplayFloor || len(deltas) != 1 {
				return fmt.Errorf("runner event delivery head=%+v before=%+v deltas=%+v", head, before, deltas)
			}
			delta := deltas[0]
			if delta.Sequence != head.Sequence || delta.Topic != "activity" || delta.Kind != "upsert" ||
				delta.EntityID != started.ID || delta.RelatedID != string(item) || delta.Previous != nil || delta.Current != nil {
				return fmt.Errorf("runner event delivery notice=%+v", delta)
			}
			events, err := r.Events(ctx, project, item, 0, 2)
			if err != nil {
				return err
			}
			if len(events) != 1 || events[0].OrganizationID != org || events[0].ID != started.ID ||
				events[0].Type != started.Type || events[0].AttemptID != attempt.ID || events[0].Sequence != 1 {
				return fmt.Errorf("committed runner events=%+v", events)
			}
			current, err := r.Attempt(ctx, attempt.ID)
			if err != nil {
				return err
			}
			if current.Sequence != 1 || current.Status != "running" {
				return fmt.Errorf("committed attempt sequence=%d status=%s", current.Sequence, current.Status)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if wakes != 1 {
			t.Fatalf("runner event delivery wakes=%d, want 1", wakes)
		}
	}

	stored, err := write(started, attempt, false, false)
	if err != nil || stored.ID != started.ID || stored.Sequence != 1 {
		t.Fatalf("runner event commit=%+v err=%v", stored, err)
	}
	checkCommitted()

	replayed, err := write(started, attempt, true, false)
	if err != nil || replayed.ID != stored.ID || replayed.Sequence != stored.Sequence || replayed.ProjectSequence == 0 {
		t.Fatalf("runner event replay=%+v err=%v", replayed, err)
	}
	checkCommitted()

	next := attempt
	next.Sequence = 2
	next.PayloadObjectKey = "example/next"
	refused, err := write(failed, next, false, true)
	if !errors.Is(err, tracker.ErrStaleFencingToken) || refused.ID != failed.ID || refused.Sequence != 2 {
		t.Fatalf("queued runner event fence refusal=%+v err=%v", refused, err)
	}
	checkCommitted()

	finished := prepare("event_delivery_finished", "run.finished", 2)
	next.Status, next.WorkspaceRetired = "succeeded", true
	if _, err := write(finished, next, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := write(finished, next, true, false); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenantSnapshot(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Attempt(ctx, attempt.ID)
		if err == nil && (current.CleanupAcknowledgedAt == nil || current.Status != "succeeded" || current.Sequence != 2) {
			t.Errorf("terminal cleanup receipt missing from committed attempt: %+v", current)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
