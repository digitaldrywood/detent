package executionrecords_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptEventDoesNotDeadlockIssueTransition(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	pgstoretest.SeedLeaseFamily(t, repository, "org_first")
	lease := pgstoretest.ClaimLease(t, repository, "org_first", pgstoretest.LeaseClaim(1))
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	transition := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.LockWorkItem(ctx, "prj_1", "wi_1")
			if err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			locker := w.(leasestore.UnreleasedIssueLeaseLocker)
			_, err = locker.LockUnreleasedIssueLease(ctx, item.ID)
			return err
		})
	})
	pgstoretest.WaitRepositorySignal(t, locked)
	var transitionPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItem :one%'`, login).Scan(&transitionPID); err != nil {
		t.Fatal(err)
	}
	attempt := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteLeases(ctx, "org_first", corestore.Background, func(w leasestore.Writer) error {
			_, err := w.RecordAttempt(ctx, pgstoretest.AttemptEvent(lease))
			return err
		})
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)) AND (query LIKE '-- name: BindAttemptRun :exec%' OR query LIKE '-- name: LockAttemptIssue :one%' OR query LIKE '-- name: LockAttemptProject :one%'))`, login, transitionPID).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("attempt event has not reached the project or issue lock")
		}
		return nil
	})
	releaseOnce.Do(func() { close(release) })
	if err := transition.Wait(t); err != nil {
		t.Errorf("issue transition lock: %v", err)
	}
	if err := attempt.Wait(t); err != nil {
		t.Errorf("attempt event: %v", err)
	}
}

func TestAttemptEventDoesNotDeadlockUsageAccounting(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	pgstoretest.SeedLeaseFamily(t, repository, "org_first")
	lease := pgstoretest.ClaimLease(t, repository, "org_first", pgstoretest.LeaseClaim(1))
	start := pgstoretest.RecordLeaseAttempt(t, repository, "org_first", pgstoretest.AttemptEvent(lease))
	if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET runner_tokens_recorded=false WHERE organization_id=$1 AND id=$2`, "org_first", start.ID); err != nil {
		t.Fatal(err)
	}
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	usage := []tracker.NativeUsage{{Model: "example-model", Input: 5, Output: 2, To: time.Now().UTC()}}
	event := pgstoretest.AttemptEvent(lease)
	event.Sequence, event.PayloadObjectKey, event.Usage = 2, "example/next", usage
	transition := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.LockWorkItem(ctx, "prj_1", "wi_1"); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := w.RecordAttempt(ctx, event)
			return err
		})
	})
	pgstoretest.WaitRepositorySignal(t, locked)
	var transitionPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItem :one%'`, login).Scan(&transitionPID); err != nil {
		t.Fatal(err)
	}
	accounting := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_first", corestore.Background, func(w tenantstore.Writer) error {
			return w.(leasestore.RunnerUsageWriter).AccountRunnerTokenUsage(ctx, start, usage)
		})
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-accounting.Done:
			t.Fatalf("accounting ended before lock observation: %v", accounting.Err)
		default:
		}
		var waiting bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)) AND (query LIKE 'INSERT INTO runner_issue_tokens%' OR query LIKE 'INSERT INTO runner_attempt_token_usage%' OR query LIKE 'INSERT INTO runner_project_daily_tokens%' OR query LIKE '-- name: LockAttemptProject :one%' OR query LIKE '-- name: LockAttemptIssue :one%' OR query LIKE '-- name: LockClaimIssue :one%'))`, login, transitionPID).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("usage accounting has not reached the project or issue lock")
		}
		return nil
	})
	releaseOnce.Do(func() { close(release) })
	if err := transition.Wait(t); err != nil {
		t.Errorf("attempt event: %v", err)
	}
	if err := accounting.Wait(t); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Errorf("stale usage accounting error = %v", err)
	}
}

func TestImportedWorkItemLocksProjectBeforeWriting(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 5, ServingConnections: 2, BackgroundConnections: 2})
	pgstoretest.SeedLeaseFamily(t, repository, "org_first")
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_first", ProjectID: "prj_1", WorkItemID: "wi_imported", RecordID: "wi_imported", Revision: 1}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_first", ActorID: "production-import", Operation: "core-work-item", CommandKey: "imported-issue", RequestHash: artifact.Digest([]byte("imported-issue")), CreatedAt: now}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	record := postgres.CoreWorkItemImport{
		Item:     corestore.WorkItem{OrganizationID: "org_first", ProjectID: "prj_1", NativeID: "wi_imported", ID: 3, Number: 2, Revision: 1, Title: "Example"},
		Facts:    claimstore.WorkItemFacts{OrganizationID: "org_first", ProjectID: "prj_1", WorkItemID: "wi_imported"},
		Versions: []eventstore.CollaborationVersion{version}, Receipt: receipt,
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	owner := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, "prj_1"); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	pgstoretest.WaitRepositorySignal(t, locked)
	var ownerPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItemProject :one%'`, login).Scan(&ownerPID); err != nil {
		t.Fatal(err)
	}
	imported := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error { return repository.ImportCoreWorkItem(ctx, record) })
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-imported.Done:
			t.Fatalf("import finished before acquiring the project lock: %v", imported.Err)
		default:
		}
		var query string
		err := admin.QueryRowContext(t.Context(), `SELECT query FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid))`, login, ownerPID).Scan(&query)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("import has not reached the project lock")
		}
		if err == nil && !strings.HasPrefix(query, "-- name: LockWorkItemProject :one") {
			t.Fatalf("import wrote before acquiring the project lock: %s", query)
		}
		return err
	})
	replayed := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error { return repository.ImportCoreWorkItem(ctx, record) })
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-replayed.Done:
			t.Fatalf("concurrent replay finished before acquiring the project lock: %v", replayed.Err)
		default:
		}
		var waiting int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND query LIKE '-- name: LockWorkItemProject :one%'`, login).Scan(&waiting); err != nil {
			return err
		}
		if waiting != 2 {
			return errors.New("concurrent import has not reached the project lock")
		}
		return nil
	})
	releaseOnce.Do(func() { close(release) })
	if err := owner.Wait(t); err != nil {
		t.Fatal(err)
	}
	if err := imported.Wait(t); err != nil {
		t.Fatal(err)
	}
	if err := replayed.Wait(t); err != nil {
		t.Fatalf("concurrent replay: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE projects SET deleted_at=$1 WHERE organization_id='org_first' AND id='prj_1'`, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.ImportCoreWorkItem(t.Context(), record); err != nil {
		t.Fatalf("committed replay after project deletion: %v", err)
	}
	record.Receipt.CommandKey = "fresh-import"
	if err := repository.ImportCoreWorkItem(t.Context(), record); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("fresh import after project deletion: %v", err)
	}
}

func TestImportedLeaseWaitsForIssueBeforeFencingAllocation(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 5, ServingConnections: 1, BackgroundConnections: 3})
	pgstoretest.SeedLeaseFamily(t, repository, "org_first")
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt, err := eventstore.NewContent(pgstoretest.NewHistoryObjects()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_first", ActorID: "production-import", Operation: "core-lease", CommandKey: "imported-lease", RequestHash: artifact.Digest([]byte("imported-lease")), CreatedAt: now}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	lease := leasestore.Lease{OrganizationID: "org_first", LeaseID: "imported-lease", FencingToken: 1000000, IssueID: 1, MachineID: "machine_1", SessionID: "imported-session", WorkItemRevision: 1, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(time.Hour)}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	transition := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.LockWorkItem(ctx, "prj_1", "wi_1"); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	pgstoretest.WaitRepositorySignal(t, locked)
	var transitionPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItem :one%'`, login).Scan(&transitionPID); err != nil {
		t.Fatal(err)
	}
	imported := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.ImportCoreLease(ctx, postgres.CoreLeaseImport{Lease: lease, Receipt: receipt})
	})
	duplicate := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		return repository.ImportCoreLease(ctx, postgres.CoreLeaseImport{Lease: lease, Receipt: receipt})
	})
	var importPID int
	testenv.WaitUntil(t, t.Context(), func() error {
		err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)) AND (query LIKE '-- name: LockAttemptIssue :one%' OR query LIKE '-- name: LockAttemptProject :one%' OR query LIKE 'INSERT INTO leases%')`, login, transitionPID).Scan(&importPID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("lease import has not reached the issue lock")
		}
		return err
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND (query LIKE '-- name: LockAttemptIssue :one%' OR query LIKE '-- name: LockAttemptProject :one%' OR query LIKE 'INSERT INTO leases%' OR query LIKE 'SELECT allocate_imported_lease_fencing_token%')`, login).Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			return fmt.Errorf("concurrent imports waiting=%d, want2", count)
		}
		return nil
	})
	var holdsAllocator bool
	if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted)`, importPID).Scan(&holdsAllocator); err != nil {
		t.Fatal(err)
	}
	if holdsAllocator {
		t.Error("lease import holds the fencing allocator while waiting for the issue")
	}
	releaseOnce.Do(func() { close(release) })
	if err := transition.Wait(t); err != nil {
		t.Errorf("issue transition lock: %v", err)
	}
	if err := imported.Wait(t); err != nil {
		t.Errorf("lease import: %v", err)
	}
	if err := duplicate.Wait(t); err != nil {
		t.Errorf("concurrent lease replay: %v", err)
	}
	var last int64
	if err := admin.QueryRowContext(t.Context(), `SELECT last_value FROM leases_fencing_token_seq`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != lease.FencingToken {
		t.Fatalf("concurrent import replay advanced the fencing allocator: %d, want%d", last, lease.FencingToken)
	}
}

func prepareLeaseCancellationFixture(t *testing.T, org tracker.OrganizationID, lease leasestore.Lease, current leasestore.AttemptEvent) (*leasestore.AttemptCancellation, *eventstore.Content) {
	t.Helper()
	objects := eventstore.NewContent(pgstoretest.NewHistoryObjects())
	now := lease.AcquiredAt
	run := tracker.NativeRunData{AttemptID: current.ID, RunID: current.RunID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken),
		Sequence: current.Sequence + 1, Outcome: "cancelled", TerminalFailure: &tracker.NativeTerminalFailure{ErrorClass: "lease_released", Stage: "lease_release", ObservedAt: now, Error: "Example lease expired"}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := objects.PreparePayload(t.Context(), org, raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{
		ID: "evt_cancel_" + current.ID, OrganizationID: org, ProjectID: tracker.ProjectID(fmt.Sprintf("prj_%d", lease.IssueID)),
		AggregateType: "issue", AggregateID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", lease.IssueID)), Type: "run.finished", SchemaVersion: 1,
		RecordedAt: now, Actor: tracker.Actor{Kind: "integration", PrincipalID: "lease_release"}, Data: tracker.CollaborationData{Run: &run},
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := objects.PrepareEvent(t.Context(), org, raw)
	if err != nil {
		t.Fatal(err)
	}
	return &leasestore.AttemptCancellation{
		Attempt: leasestore.AttemptEvent{ID: current.ID, RunID: current.RunID, Fence: pgstoretest.LeaseFence(lease), Sequence: run.Sequence, Status: "cancelled",
			DispatchGeneration: current.DispatchGeneration, PayloadObjectKey: payload.Key, PayloadReference: payload},
		History: history,
	}, objects
}

func expireLeaseFixture(t *testing.T, admin *sql.DB, org tracker.OrganizationID, id string) {
	t.Helper()
	_, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '3 hours',renewed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE organization_id=$1 AND lease_id=$2`, org, id)
	if err != nil {
		t.Fatal(err)
	}
}

func requireLeaseUsage(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, machine string, want int64) {
	t.Helper()
	err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
		got, err := reader.Usage(t.Context(), machine)
		if err == nil && got != want {
			t.Errorf("usage = %d, want %d", got, want)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLeaseAttemptLifecycle(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, r, org)
		for _, id := range []int64{1, 2} {
			t.Run(fmt.Sprintf("%s/%d", org, id), func(t *testing.T) {
				request := pgstoretest.LeaseClaim(id)
				lease := pgstoretest.ClaimLease(t, r, org, request)
				request.LeaseID = "unused_replay_id"
				replay := pgstoretest.ClaimLease(t, r, org, request)
				if replay.LeaseID != lease.LeaseID || replay.FencingToken != lease.FencingToken || !replay.ExpiresAt.Equal(lease.ExpiresAt) || lease.WorkItemRevision != 1 {
					t.Fatalf("claim replay changed ownership: %+v, first %+v", replay, lease)
				}
				event := pgstoretest.AttemptEvent(lease)
				event.RuntimeHeartbeatAt = new(lease.RenewedAt.Add(-time.Minute))
				event.WorkerCleanupRequired = new(id == 1)
				start := pgstoretest.RecordLeaseAttempt(t, r, org, event)
				if start.Sequence != 1 || start.Status != "running" || start.WorkItemRevision != lease.WorkItemRevision || start.RuntimeSummarySequence != 1 || start.WorkerCleanupRequired == nil || *start.WorkerCleanupRequired != (id == 1) || start.RuntimeHeartbeatAt == nil || !start.RuntimeHeartbeatAt.Equal(*event.RuntimeHeartbeatAt) {
					t.Fatalf("start = %+v", start)
				}
				legacy := struct {
					ID                  string
					Fence               leasestore.Fence
					RunID               string
					Sequence            int64
					Status              string
					DispatchGeneration  int64
					StartedAt           time.Time
					PayloadObjectKey    string
					CheckpointObjectKey *string
					ArtifactsObjectKey  *string
					UsageCorrection     bool
				}{event.ID, event.Fence, event.RunID, event.Sequence, event.Status, event.DispatchGeneration, event.StartedAt, event.PayloadObjectKey, event.CheckpointObjectKey, event.ArtifactsObjectKey, event.UsageCorrection}
				raw, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(raw)
				if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempt_events SET request_hash=$1 WHERE organization_id=$2 AND attempt_id=$3 AND sequence=$4", hex.EncodeToString(digest[:]), org, event.ID, event.Sequence); err != nil {
					t.Fatal(err)
				}
				if replay := pgstoretest.RecordLeaseAttempt(t, r, org, event); replay.ID != start.ID || replay.Sequence != start.Sequence || replay.FencingToken != start.FencingToken {
					t.Fatalf("legacy receipt replay changed attempt: %+v", replay)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET runtime_heartbeat_at=NULL,runtime_summary_sequence=NULL,worker_cleanup_required=NULL WHERE organization_id=$1 AND id=$2`, org, event.ID); err != nil {
					t.Fatal(err)
				}
				for _, sequence := range []int64{2, 1} {
					if err := r.WriteLeases(t.Context(), org, corestore.Background, func(w leasestore.Writer) error {
						return w.SetAttemptRuntimeHeartbeat(t.Context(), event.ID, sequence, event.RuntimeHeartbeatAt, id == 1)
					}); err != nil {
						t.Fatal(err)
					}
					if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
						got, err := reader.Attempt(t.Context(), event.ID)
						if err == nil && ((got.WorkerCleanupRequired != nil) != (sequence == 1) || (got.RuntimeHeartbeatAt != nil) != (sequence == 1) || sequence == 1 && (*got.WorkerCleanupRequired != (id == 1) || got.RuntimeSummarySequence != 1 || !got.RuntimeHeartbeatAt.Equal(*event.RuntimeHeartbeatAt))) {
							t.Fatalf("backfill sequence %d: %+v", sequence, got)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				checkpoint := &tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"}
				if id == 1 {
					checkpoint.WorktreeState = "dirty"
				}
				checkpointRaw, err := json.Marshal(struct {
					Checkpoint *tracker.NativeCheckpoint `json:"checkpoint"`
				}{Checkpoint: checkpoint})
				if err != nil {
					t.Fatal(err)
				}
				checkpointObjects := pgstoretest.NewHistoryObjects()
				checkpointRef, err := checkpointObjects.StoreDetail(t.Context(), string(org), checkpointRaw)
				if err != nil {
					t.Fatal(err)
				}
				event.Sequence, event.PayloadObjectKey = 2, "example/checkpoint"
				event.CheckpointObjectKey, event.CheckpointReference = &checkpointRef.Key, checkpointRef
				event.CheckpointPreservesSource = new(checkpoint.PreservesSource())
				event.ArtifactsObjectKey = new("example/artifacts")
				event.RuntimeHeartbeatAt = nil
				event.WorkerCleanupRequired = nil
				updated := pgstoretest.RecordLeaseAttempt(t, r, org, event)
				if updated.WorkerCleanupRequired != nil || updated.RuntimeHeartbeatAt != nil || updated.RuntimeSummarySequence != 2 {
					t.Fatalf("runtime summary survived absent runtime: %+v", updated)
				}
				if err := r.WriteLeases(t.Context(), org, corestore.Background, func(w leasestore.Writer) error {
					return w.SetAttemptRuntimeHeartbeat(t.Context(), event.ID, 1, start.RuntimeHeartbeatAt, true)
				}); err != nil {
					t.Fatal(err)
				}
				if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
					got, err := reader.Attempt(t.Context(), event.ID)
					if err == nil && (got.WorkerCleanupRequired != nil || got.RuntimeHeartbeatAt != nil || got.RuntimeSummarySequence != 2) {
						t.Fatalf("stale backfill replaced current summary: %+v", got)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				event.Sequence, event.Status, event.PayloadObjectKey = 3, "succeeded", "example/finish"
				event.CheckpointObjectKey, event.ArtifactsObjectKey = nil, nil
				event.CheckpointPreservesSource = nil
				finished := pgstoretest.RecordLeaseAttempt(t, r, org, event)
				if finished.Status != "succeeded" || finished.Sequence != 3 || finished.CheckpointObjectKey == nil || *finished.CheckpointObjectKey != checkpointRef.Key || finished.ArtifactsObjectKey == nil || *finished.ArtifactsObjectKey != "example/artifacts" || finished.CheckpointPreservesSource == nil || *finished.CheckpointPreservesSource != checkpoint.PreservesSource() {
					t.Fatalf("finish lost checkpoint: %+v", finished)
				}
				requireLeaseUsage(t, r, org, lease.MachineID, 1)
				err = r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					renewed, err := w.RenewWithLandingCapacity(t.Context(), pgstoretest.LeaseFence(lease), 2*time.Hour, 3)
					if err != nil {
						return err
					}
					if renewed.FencingToken != lease.FencingToken || !renewed.ExpiresAt.After(lease.ExpiresAt) || renewed.LandingCapacity != 3 {
						t.Errorf("renew changed owner or failed to extend expiry: %+v", renewed)
					}
					released, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
					if err == nil && (released.ReleasedAt == nil || released.ReleaseReason != "finished") {
						t.Errorf("release = %+v", released)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				pgstoretest.RecordLeaseAttempt(t, r, org, event)
				requireLeaseUsage(t, r, org, lease.MachineID, 0)
				request.LeaseID, request.SessionID = lease.LeaseID+"_next", lease.SessionID+"_next"
				next := pgstoretest.ClaimLease(t, r, org, request)
				if next.FencingToken <= lease.FencingToken {
					t.Errorf("next fence = %d, previous %d", next.FencingToken, lease.FencingToken)
				}
				continued := pgstoretest.AttemptEvent(next)
				continued.RunID = event.RunID
				pgstoretest.RecordLeaseAttempt(t, r, org, continued)
			})
		}
	}
}

func TestLeaseIssueReadsReturnActiveLeaseAndBoundedAttemptPages(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	pgstoretest.SeedLeaseFamily(t, r, "org_first")
	var attempts []leasestore.Attempt
	for sequence := 1; sequence <= 3; sequence++ {
		request := pgstoretest.LeaseClaim(1)
		request.LeaseID = fmt.Sprintf("lease_page_%d", sequence)
		request.SessionID = fmt.Sprintf("session_page_%d", sequence)
		lease := pgstoretest.ClaimLease(t, r, "org_first", request)
		if sequence == 1 {
			err := r.ReadTenant(t.Context(), "org_first", corestore.Serving, func(reader tenantstore.Reader) error {
				active, err := reader.ActiveLeaseForIssue(t.Context(), 1)
				if err == nil && active.LeaseID != lease.LeaseID {
					t.Errorf("active lease = %s, want %s", active.LeaseID, lease.LeaseID)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		event := pgstoretest.AttemptEvent(lease)
		event.ID = fmt.Sprintf("attempt_page_%d", sequence)
		event.RunID = fmt.Sprintf("run_page_%d", sequence)
		attempts = append(attempts, pgstoretest.RecordLeaseAttempt(t, r, "org_first", event))
		if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(writer leasestore.Writer) error {
			_, err := writer.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	pgstoretest.SeedLeaseFamily(t, r, "org_second")
	foreignLease := pgstoretest.ClaimLease(t, r, "org_second", pgstoretest.LeaseClaim(1))
	foreignEvent := pgstoretest.AttemptEvent(foreignLease)
	foreignEvent.ID, foreignEvent.RunID = attempts[0].ID, attempts[0].RunID
	foreignAttempt := pgstoretest.RecordLeaseAttempt(t, r, "org_second", foreignEvent)
	if err := r.ReadTenant(t.Context(), "org_second", corestore.Serving, func(reader tenantstore.Reader) error {
		page, err := reader.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{IssueID: 1, Oldest: true, ThroughFence: attempts[1].FencingToken}, 1)
		if err != nil {
			return err
		}
		if len(page.Items) != 0 {
			t.Fatalf("frozen attempt page crossed tenant scope: %+v", page)
		}
		page, err = reader.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{IssueID: 1, Oldest: true, ThroughFence: foreignAttempt.FencingToken}, 1)
		if err == nil && (len(page.Items) != 1 || page.Items[0].FencingToken != foreignAttempt.FencingToken) {
			t.Fatalf("second tenant frozen attempt page unusable: %+v", page)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := r.ReadTenant(t.Context(), "org_first", corestore.Serving, func(reader tenantstore.Reader) error {
		if _, err := reader.ActiveLeaseForIssue(t.Context(), 1); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("active lease after release = %v, want no rows", err)
		}
		latest, err := reader.LatestAttempt(t.Context(), 1)
		if err != nil {
			return err
		}
		if latest.ID != attempts[2].ID || latest.FencingToken != attempts[2].FencingToken {
			t.Errorf("latest attempt = %+v, want %+v", latest, attempts[2])
		}
		first, err := reader.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{}, 1)
		if err != nil {
			return err
		}
		if len(first.Items) != 1 || first.Items[0].ID != attempts[2].ID || first.Next.IssueID != 1 || first.Next.BeforeFence != attempts[2].FencingToken {
			t.Fatalf("first attempt page = %+v", first)
		}
		second, err := reader.AttemptPage(t.Context(), 1, first.Next, 1)
		if err != nil {
			return err
		}
		if len(second.Items) != 1 || second.Items[0].ID != attempts[1].ID || second.Next.BeforeFence != attempts[1].FencingToken {
			t.Fatalf("second attempt page = %+v", second)
		}
		third, err := reader.AttemptPage(t.Context(), 1, second.Next, 1)
		if err != nil {
			return err
		}
		if len(third.Items) != 1 || third.Items[0].ID != attempts[0].ID || third.Next != (leasestore.AttemptCursor{}) {
			t.Fatalf("third attempt page = %+v", third)
		}
		if _, err := reader.AttemptPage(t.Context(), 2, first.Next, 1); !errors.Is(err, corestore.ErrInvalidCursor) {
			t.Errorf("cross-issue attempt cursor error = %v, want invalid cursor", err)
		}
		oldestFirst, err := reader.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{IssueID: 1, Oldest: true}, 1)
		if err != nil {
			return err
		}
		if len(oldestFirst.Items) != 1 || oldestFirst.Items[0].ID != attempts[0].ID || oldestFirst.Next.IssueID != 1 || !oldestFirst.Next.Oldest || oldestFirst.Next.AfterFence != attempts[0].FencingToken {
			t.Fatalf("oldest first attempt page = %+v", oldestFirst)
		}
		bounded, err := reader.AttemptPage(t.Context(), 1, leasestore.AttemptCursor{IssueID: 1, Oldest: true, ThroughFence: attempts[1].FencingToken}, 1)
		if err != nil {
			return err
		}
		if len(bounded.Items) != 1 || bounded.Items[0].ID != attempts[0].ID || bounded.Next.ThroughFence != attempts[1].FencingToken {
			t.Fatalf("bounded first attempt page: %+v", bounded)
		}
		boundedLast, err := reader.AttemptPage(t.Context(), 1, bounded.Next, 1)
		if err != nil {
			return err
		}
		if len(boundedLast.Items) != 1 || boundedLast.Items[0].ID != attempts[1].ID || boundedLast.Next != (leasestore.AttemptCursor{}) {
			t.Fatalf("bounded pass crossed its frozen head: %+v", boundedLast)
		}
		for _, cursor := range []leasestore.AttemptCursor{
			{IssueID: 1, Oldest: true, ThroughFence: -1},
			{IssueID: 1, Oldest: true, AfterFence: attempts[2].FencingToken, ThroughFence: attempts[1].FencingToken},
			{IssueID: 1, BeforeFence: attempts[2].FencingToken, ThroughFence: attempts[1].FencingToken},
		} {
			if _, err := reader.AttemptPage(t.Context(), 1, cursor, 1); !errors.Is(err, corestore.ErrInvalidCursor) {
				t.Fatalf("invalid frozen attempt cursor accepted: %+v error=%v", cursor, err)
			}
		}
		oldestSecond, err := reader.AttemptPage(t.Context(), 1, oldestFirst.Next, 1)
		if err != nil {
			return err
		}
		if len(oldestSecond.Items) != 1 || oldestSecond.Items[0].ID != attempts[1].ID || oldestSecond.Next.AfterFence != attempts[1].FencingToken {
			t.Fatalf("oldest second attempt page = %+v", oldestSecond)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLeaseRejectsStaleAndInvalidMutations(t *testing.T) {
	for _, state := range []string{"live", "expired", "released", "reassigned"} {
		t.Run(state, func(t *testing.T) {
			admin := pgstoretest.Template.Database(t)
			r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
			pgstoretest.SeedLeaseFamily(t, r, "org_first")
			lease := pgstoretest.ClaimLease(t, r, "org_first", pgstoretest.LeaseClaim(1))
			event := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, r, "org_first", event)
			if state == "expired" || state == "reassigned" {
				expireLeaseFixture(t, admin, "org_first", lease.LeaseID)
			}
			if state == "released" {
				if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "cancelled")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := r.ReadLeases(t.Context(), "org_first", corestore.Serving, func(reader leasestore.Reader) error {
					current, err := reader.Attempt(t.Context(), event.ID)
					if err == nil && (current.Status != "cancelled" || current.RuntimeSummarySequence != current.Sequence) {
						t.Fatalf("released attempt runtime coverage = %+v", current)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "reassigned" {
				request := pgstoretest.LeaseClaim(1)
				request.LeaseID, request.SessionID = "replacement", "replacement"
				request.PreviousAttemptCancellation, _ = prepareLeaseCancellationFixture(t, "org_first", lease, event)
				pgstoretest.ClaimLease(t, r, "org_first", request)
			}
			for _, operation := range []string{"renew", "release", "finish", "wrong fence", "expire live"} {
				if state == "live" && operation != "wrong fence" && operation != "expire live" || state != "live" && operation == "expire live" {
					continue
				}
				t.Run(operation, func(t *testing.T) {
					err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
						fence := pgstoretest.LeaseFence(lease)
						switch operation {
						case "renew":
							_, err := w.Renew(t.Context(), fence, time.Hour)
							return err
						case "release":
							_, err := w.Release(t.Context(), fence, "stale")
							return err
						case "finish":
							finish := event
							finish.Sequence, finish.Status = 2, "succeeded"
							_, err := w.RecordAttempt(t.Context(), finish)
							return err
						case "wrong fence":
							fence.FencingToken++
							_, err := w.Renew(t.Context(), fence, time.Hour)
							return err
						default:
							_, err := w.Expire(t.Context(), fence)
							return err
						}
					})
					if !errors.Is(err, tracker.ErrStaleFencingToken) {
						t.Fatalf("%s error = %v", operation, err)
					}
				})
			}
			want := int64(0)
			if state == "live" || state == "reassigned" {
				want = 1
			}
			requireLeaseUsage(t, r, "org_first", lease.MachineID, want)
		})
	}
}

func TestLeaseAttemptRejectsConflictingSequences(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	pgstoretest.SeedLeaseFamily(t, r, "org_first")
	lease := pgstoretest.ClaimLease(t, r, "org_first", pgstoretest.LeaseClaim(1))
	event := pgstoretest.AttemptEvent(lease)
	pgstoretest.RecordLeaseAttempt(t, r, "org_first", event)
	for _, tc := range []struct {
		name string
		edit func(*leasestore.AttemptEvent)
	}{
		{"changed receipt", func(e *leasestore.AttemptEvent) { e.PayloadObjectKey = "example/different" }},
		{"skipped sequence", func(e *leasestore.AttemptEvent) { e.Sequence = 3 }},
		{"changed run", func(e *leasestore.AttemptEvent) { e.Sequence, e.RunID = 2, "different" }},
		{"changed generation", func(e *leasestore.AttemptEvent) { e.Sequence, e.DispatchGeneration = 2, 3 }},
		{"empty payload", func(e *leasestore.AttemptEvent) { e.PayloadObjectKey = "" }},
		{"invalid status", func(e *leasestore.AttemptEvent) { e.Status = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := event
			tc.edit(&changed)
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error { _, err := w.RecordAttempt(t.Context(), changed); return err })
			if !errors.Is(err, leasestore.ErrAttemptConflict) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if got := pgstoretest.RecordLeaseAttempt(t, r, "org_first", event); got.Sequence != 1 || got.PayloadObjectKey != event.PayloadObjectKey {
		t.Fatalf("rejections changed attempt: %+v", got)
	}
}

func TestLeaseAttemptUsageCorrectionPreservesTerminalLifecycle(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	pgstoretest.SeedLeaseFamily(t, r, "org_first")
	lease := pgstoretest.ClaimLease(t, r, "org_first", pgstoretest.LeaseClaim(1))
	started := pgstoretest.AttemptEvent(lease)
	pgstoretest.RecordLeaseAttempt(t, r, "org_first", started)
	finished := started
	finished.Sequence = 2
	finished.Status = "succeeded"
	finished.PayloadObjectKey = "example/finish"
	terminal := pgstoretest.RecordLeaseAttempt(t, r, "org_first", finished)
	corrected := finished
	corrected.Sequence = 3
	corrected.PayloadObjectKey = "example/finish-corrected"
	corrected.UsageCorrection = true
	updated := pgstoretest.RecordLeaseAttempt(t, r, "org_first", corrected)
	if updated.Sequence != 3 || updated.Status != terminal.Status || !updated.UpdatedAt.Equal(terminal.UpdatedAt) || updated.PayloadObjectKey != corrected.PayloadObjectKey {
		t.Fatalf("usage correction changed terminal attempt lifecycle: before=%+v after=%+v", terminal, updated)
	}
}

func TestLeaseCompetingClaims(t *testing.T) {
	for _, sameSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_session_%t", sameSession), func(t *testing.T) {
			admin := pgstoretest.Template.Database(t)
			r, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
			pgstoretest.SeedLeaseFamily(t, r, "org_first")
			admin.SetMaxOpenConns(2)
			blocker, err := admin.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback()
			if _, err := blocker.ExecContext(t.Context(), `SELECT id FROM issues WHERE organization_id='org_first' AND id=1 FOR NO KEY UPDATE`); err != nil {
				t.Fatal(err)
			}
			var leases [2]leasestore.Lease
			var tasks []*pgstoretest.RepositoryTask
			for i, workload := range []corestore.Workload{corestore.Serving, corestore.Background} {
				tasks = append(tasks, pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteLeases(ctx, "org_first", workload, func(w leasestore.Writer) error {
						request := pgstoretest.LeaseClaim(1)
						if !sameSession {
							request.LeaseID, request.SessionID = fmt.Sprintf("lease_%d", i), fmt.Sprintf("session_%d", i)
						}
						var err error
						leases[i], err = w.Claim(ctx, request)
						return err
					})
				}))
			}
			pgstoretest.WaitLeaseLock(t, admin, 2, tasks...)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			var winners, conflicts int
			for _, task := range tasks {
				err := task.Wait(t)
				if err == nil {
					winners++
				} else if errors.Is(err, tracker.ErrLeaseConflict) {
					conflicts++
				} else {
					t.Fatal(err)
				}
			}
			if !sameSession && (winners != 1 || conflicts != 1) || sameSession && (winners != 2 || leases[0].FencingToken != leases[1].FencingToken) {
				t.Fatalf("winners=%d conflicts=%d leases=%+v", winners, conflicts, leases)
			}
			var active int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM leases WHERE released_at IS NULL`).Scan(&active); err != nil || active != 1 {
				t.Fatalf("active=%d error=%v", active, err)
			}
		})
	}
}

func TestLeaseClaimEligibility(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	pgstoretest.SeedLeaseFamily(t, r, "org_first")
	for _, tc := range []struct {
		name string
		edit func(*leasestore.Claim)
		want error
	}{
		{"changed revision", func(c *leasestore.Claim) { c.ExpectedRevision++ }, corestore.ErrRevisionConflict},
		{"missing item", func(c *leasestore.Claim) { c.IssueID = 99 }, corestore.ErrNotFound},
		{"zero TTL", func(c *leasestore.Claim) { c.TTL = 0 }, tracker.ErrInvalidClaimRequest},
		{"submicrosecond TTL", func(c *leasestore.Claim) { c.TTL = time.Nanosecond }, tracker.ErrInvalidClaimRequest},
		{"empty session", func(c *leasestore.Claim) { c.SessionID = " " }, tracker.ErrInvalidClaimRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := pgstoretest.LeaseClaim(1)
			tc.edit(&request)
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error { _, err := w.Claim(t.Context(), request); return err })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
	if err := r.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
		item, err := w.WorkItem(t.Context(), "prj_1", "wi_1")
		if err != nil {
			return err
		}
		item.Archived = true
		_, err = w.UpdateWorkItem(t.Context(), item)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := pgstoretest.LeaseClaim(1)
	request.ExpectedRevision = 2
	err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error { _, err := w.Claim(t.Context(), request); return err })
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("archived claim = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "Example") {
		t.Fatal("error contains customer content")
	}
}

func TestLeaseClaimReplayOnlyTightensLandingCapacity(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	pgstoretest.SeedLeaseFamily(t, r, "org_first")
	firstRequest := pgstoretest.LeaseClaim(1)
	first := pgstoretest.ClaimLease(t, r, "org_first", firstRequest)
	for _, test := range []struct {
		name     string
		capacity int64
		want     int64
	}{
		{name: "establish", capacity: 2, want: 2},
		{name: "cannot widen", capacity: 4, want: 2},
		{name: "tighten", capacity: 1, want: 1},
		{name: "cannot clear", capacity: 0, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			replayRequest := firstRequest
			replayRequest.LeaseID = "lease_replay"
			replayRequest.LandingCapacity = test.capacity
			replay := pgstoretest.ClaimLease(t, r, "org_first", replayRequest)
			if replay.LeaseID != first.LeaseID || replay.FencingToken != first.FencingToken || replay.LandingCapacity != test.want || !replay.ExpiresAt.Equal(first.ExpiresAt) {
				t.Fatalf("capacity replay=%+v original=%+v want capacity=%d", replay, first, test.want)
			}
		})
	}
}

func TestLandingLeaseRenewalPreservesFenceAndCeiling(t *testing.T) {
	for _, state := range []string{"live", "expired", "released", "reassigned"} {
		t.Run(state, func(t *testing.T) {
			admin := pgstoretest.Template.Database(t)
			r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
			pgstoretest.SeedLeaseFamily(t, r, "org_first")
			request := pgstoretest.LeaseClaim(1)
			request.LandingCapacity = 3
			lease := pgstoretest.ClaimLease(t, r, "org_first", request)
			if state == "expired" || state == "reassigned" {
				expireLeaseFixture(t, admin, "org_first", lease.LeaseID)
			}
			if state == "released" {
				if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "cancelled")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "reassigned" {
				request.LeaseID, request.SessionID = "replacement", "replacement"
				pgstoretest.ClaimLease(t, r, "org_first", request)
			}
			err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
				renewed, err := w.RenewWithLandingCapacity(t.Context(), pgstoretest.LeaseFence(lease), time.Hour, 1)
				if err != nil {
					return err
				}
				if renewed.LeaseID != lease.LeaseID || renewed.FencingToken != lease.FencingToken || renewed.LandingCapacity != 1 || !renewed.ExpiresAt.After(lease.ExpiresAt) {
					t.Fatalf("landing renewal=%+v original=%+v", renewed, lease)
				}
				wider, err := w.RenewWithLandingCapacity(t.Context(), pgstoretest.LeaseFence(lease), time.Hour, 4)
				if err == nil && wider.LandingCapacity != 1 {
					t.Fatalf("renewal widened ceiling to %d", wider.LandingCapacity)
				}
				return err
			})
			if state == "released" || state == "reassigned" {
				if !errors.Is(err, tracker.ErrStaleFencingToken) {
					t.Fatalf("stale landing renewal error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLeaseIsolationTierConstraint(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, r, organization)
		lease := pgstoretest.ClaimLease(t, r, organization, pgstoretest.LeaseClaim(1))
		for _, test := range []struct {
			name  string
			tier  *string
			valid bool
		}{
			{"sandbox", new("sandbox"), true},
			{"native trusted", new("native-trusted"), true},
			{"legacy null", nil, true},
			{"empty", new(""), false},
			{"unknown", new("root"), false},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				_, err := admin.ExecContext(t.Context(), `UPDATE leases SET isolation_tier=$3 WHERE organization_id=$1 AND lease_id=$2`, organization, lease.LeaseID, test.tier)
				if !test.valid {
					var state interface{ SQLState() string }
					if !errors.As(err, &state) || state.SQLState() != "23514" {
						t.Fatalf("invalid tier error = %v, want check violation", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := r.ReadLeases(t.Context(), organization, corestore.Serving, func(reader leasestore.Reader) error {
					stored, err := reader.Lease(t.Context(), lease.LeaseID)
					if err == nil && ((stored.IsolationTier == nil) != (test.tier == nil) || test.tier != nil && *stored.IsolationTier != *test.tier) {
						t.Fatalf("stored tier = %v, want %v", stored.IsolationTier, test.tier)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRunnerSpendRejectsCumulativeModelOverflow(t *testing.T) {
	for _, scenario := range []string{"repeated observations", "issue across days"} {
		t.Run(scenario, func(t *testing.T) {
			r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
			day := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				pgstoretest.SeedLeaseFamily(t, r, org)
				lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(1))
				event := pgstoretest.AttemptEvent(lease)
				for batch := 0; batch < 7; batch++ {
					event.Sequence = int64(batch + 1)
					event.PayloadObjectKey = fmt.Sprintf("example/observation-%d", batch)
					event.Usage = nil
					count := 32
					if batch == 6 {
						count = 8
					}
					for index := 0; index < count; index++ {
						event.Usage = append(event.Usage, tracker.NativeUsage{Model: fmt.Sprintf("example-model-%03d", batch*32+index), To: day})
					}
					pgstoretest.RecordLeaseAttempt(t, r, org, event)
				}
				overflow := event
				overflow.Sequence++
				overflow.PayloadObjectKey = "example/overflow"
				overflow.Usage = []tracker.NativeUsage{{Model: "example-model-overflow", To: day}}
				if scenario == "issue across days" {
					overflow.Usage[0].To = day.AddDate(0, 0, 1)
				}
				err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.RecordAttempt(t.Context(), overflow)
					return err
				})
				if !errors.Is(err, leasestore.ErrAttemptConflict) {
					t.Errorf("overflow error = %v, want attempt conflict", err)
				}
				event.Sequence++
				event.PayloadObjectKey = "example/correction"
				event.Usage = []tracker.NativeUsage{{Model: "example-model-000", Input: 20, CachedInput: 5, Output: 3, To: day}}
				pgstoretest.RecordLeaseAttempt(t, r, org, event)
				for _, at := range []*time.Time{&day, nil} {
					err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
						spend, err := reader.(leasestore.RunnerSpendReader).RunnerTokenSpend(t.Context(), "prj_1", "wi_1", at, nil)
						if err == nil && (len(spend.ByModel) != 200 || spend.InputTokens != 20 || spend.CachedInputTokens != 5 || spend.OutputTokens != 3 || spend.TotalTokens != 23 || spend.Sessions != 200) {
							t.Errorf("spend after rejection and correction = %+v", spend)
						}
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

type repositorySpend struct {
	repository   *postgres.Repository
	organization tracker.OrganizationID
}

func (s repositorySpend) read(ctx context.Context, project string, issue string, day *time.Time) (store.TokenSpend, error) {
	var spend store.TokenSpend
	err := s.repository.ReadLeases(ctx, s.organization, corestore.Serving, func(reader leasestore.Reader) error {
		var err error
		spend, err = reader.(leasestore.RunnerSpendReader).RunnerTokenSpend(ctx, tracker.ProjectID(project), issue, day, nil)
		return err
	})
	return spend, err
}

func (s repositorySpend) ProjectDailyTokenSpend(ctx context.Context, project string, day time.Time) (store.TokenSpend, error) {
	return s.read(ctx, project, "", &day)
}

func (s repositorySpend) IssueTokenSpend(ctx context.Context, issue store.IssueIdentity) (store.TokenSpend, error) {
	return s.read(ctx, issue.ProjectID, issue.IssueID, nil)
}

func TestRunnerSpendReportBoundAcceptsOtherAttempts(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	day := time.Date(2026, 12, 25, 12, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	third := day.AddDate(0, 0, 2)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, r, org)
		if err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
			for _, id := range []int64{3, 4} {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: id, ProjectID: "prj_1", NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", id)), Number: id, Title: "Example"}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		claim := func(machine, issue int64, lease string) leasestore.Claim {
			request := pgstoretest.LeaseClaim(machine)
			request.IssueID, request.LeaseID, request.SessionID = issue, lease, "session_"+lease
			return request
		}
		release := func(lease leasestore.Lease) {
			t.Helper()
			if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, flood := range []struct {
			claim leasestore.Claim
			at    time.Time
		}{
			{claim: pgstoretest.LeaseClaim(1), at: day},
			{claim: claim(1, 4, "lease_flood_4"), at: third},
		} {
			flooding := pgstoretest.ClaimLease(t, r, org, flood.claim)
			event := pgstoretest.AttemptEvent(flooding)
			for batch := 0; batch < 7; batch++ {
				event.Sequence = int64(batch + 1)
				event.PayloadObjectKey = fmt.Sprintf("example/flood-%d", batch)
				event.Usage = nil
				for index := batch * 32; index < min((batch+1)*32, 200); index++ {
					event.Usage = append(event.Usage, tracker.NativeUsage{Model: fmt.Sprintf("example-fabricated-%03d", index), To: flood.at})
				}
				pgstoretest.RecordLeaseAttempt(t, r, org, event)
			}
			release(flooding)
		}
		for _, completion := range []struct {
			claim leasestore.Claim
			usage tracker.NativeUsage
		}{
			{claim: claim(2, 3, "lease_2"), usage: tracker.NativeUsage{Model: "gpt-6-astra", Input: 29, Output: 1_000_000, To: day}},
			{claim: claim(1, 1, "lease_1_next"), usage: tracker.NativeUsage{Model: "gpt-6-astra", Input: 7, Output: 1_000_000, To: next}},
			{claim: claim(1, 4, "lease_4_input"), usage: tracker.NativeUsage{Model: "gpt-6-astra", Input: 5_000_000, To: third}},
			{claim: claim(2, 4, "lease_4_cached"), usage: tracker.NativeUsage{Model: "example-cached-only", CachedInput: 5_000_000, To: third}},
		} {
			lease := pgstoretest.ClaimLease(t, r, org, completion.claim)
			finished := pgstoretest.AttemptEvent(lease)
			pgstoretest.RecordLeaseAttempt(t, r, org, finished)
			finished.Sequence, finished.Status, finished.PayloadObjectKey = 2, "succeeded", "example/finished"
			finished.Usage = []tracker.NativeUsage{completion.usage}
			if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.RecordAttempt(t.Context(), finished)
				return err
			}); err != nil {
				t.Errorf("%s completion after fabricated models = %v, want accepted", completion.claim.LeaseID, err)
			}
			release(lease)
		}
		for _, want := range []struct {
			issue                   string
			day                     *time.Time
			models                  int
			input, output, sessions int64
			first                   store.ModelTokenSpend
		}{
			{day: &day, models: 201, input: 29, output: 1_000_000, sessions: 201, first: store.ModelTokenSpend{InputTokens: 29, OutputTokens: 1_000_000, TotalTokens: 1_000_029, Sessions: 1}},
			{day: &next, models: 1, input: 7, output: 1_000_000, sessions: 1, first: store.ModelTokenSpend{Model: "gpt-6-astra", InputTokens: 7, OutputTokens: 1_000_000, TotalTokens: 1_000_007, Sessions: 1}},
			{day: &third, models: 201, input: 5_000_000, sessions: 202, first: store.ModelTokenSpend{InputTokens: 5_000_000, CachedInputTokens: 5_000_000, TotalTokens: 5_000_000, Sessions: 2}},
			{issue: "wi_3", models: 1, input: 29, output: 1_000_000, sessions: 1, first: store.ModelTokenSpend{Model: "gpt-6-astra", InputTokens: 29, OutputTokens: 1_000_000, TotalTokens: 1_000_029, Sessions: 1}},
			{issue: "wi_1", models: 201, input: 7, output: 1_000_000, sessions: 201, first: store.ModelTokenSpend{InputTokens: 7, OutputTokens: 1_000_000, TotalTokens: 1_000_007, Sessions: 1}},
			{issue: "wi_4", models: 201, input: 5_000_000, sessions: 202, first: store.ModelTokenSpend{InputTokens: 5_000_000, CachedInputTokens: 5_000_000, TotalTokens: 5_000_000, Sessions: 2}},
		} {
			err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
				spend, err := reader.(leasestore.RunnerSpendReader).RunnerTokenSpend(t.Context(), "prj_1", want.issue, want.day, nil)
				if err == nil && (len(spend.ByModel) != want.models || spend.ByModel[0] != want.first || spend.InputTokens != want.input || spend.OutputTokens != want.output || spend.Sessions != want.sessions) {
					t.Errorf("%s issue %q day %v spend = %d models, first %+v, input %d output %d sessions %d", org, want.issue, want.day, len(spend.ByModel), spend.ByModel[:min(1, len(spend.ByModel))], spend.InputTokens, spend.OutputTokens, spend.Sessions)
				}
				return err
			})
			if err != nil {
				t.Errorf("%s issue %q day %v spend read = %v", org, want.issue, want.day, err)
			}
		}
		spend := repositorySpend{repository: r, organization: org}
		for _, limit := range []struct {
			config budget.Config
			issue  string
			now    time.Time
			want   budget.ReasonCode
		}{
			{config: budget.Config{ProjectID: "prj_1", BillingMode: "metered", Enabled: true, PerDayMaxUSD: 40}, now: day, want: budget.ReasonPerDayMaxUSD},
			{config: budget.Config{ProjectID: "prj_1", BillingMode: "metered", Enabled: true, PerIssueMaxUSD: 40}, issue: "wi_1", now: next, want: budget.ReasonPerIssueMaxUSD},
			{config: budget.Config{ProjectID: "prj_1", BillingMode: "metered", Enabled: true, PerDayMaxUSD: 40}, now: third, want: budget.ReasonPerDayMaxUSD},
			{config: budget.Config{ProjectID: "prj_1", BillingMode: "metered", Enabled: true, PerIssueMaxUSD: 40}, issue: "wi_4", now: third, want: budget.ReasonPerIssueMaxUSD},
		} {
			decision, err := budget.NewChecker(limit.config, spend, budget.DefaultPricingTable()).CheckDispatch(t.Context(), budget.DispatchRequest{IssueID: limit.issue, Model: "gpt-5.4-mini", Now: limit.now, Estimate: budget.TokenEstimate{OutputTokens: 1, TotalTokens: 1}})
			if err != nil || decision.Allowed || decision.Refusal == nil || decision.Refusal.Code != limit.want {
				t.Errorf("%s flooded %s %s %s dispatch = %+v error %v, want refusal", org, limit.want, limit.issue, limit.now.Format(time.DateOnly), decision, err)
			}
		}
	}
}

func TestRunnerSpendAcceptsCumulativeDecreaseAsZero(t *testing.T) {
	yesterday := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	today := yesterday.AddDate(0, 0, 1)
	for _, test := range []struct {
		name     string
		decrease tracker.NativeUsage
	}{
		{name: "input", decrease: tracker.NativeUsage{Input: 99, CachedInput: 40, Output: 1_000_000}},
		{name: "cached input", decrease: tracker.NativeUsage{Input: 100, CachedInput: 39, Output: 1_000_000}},
		{name: "output", decrease: tracker.NativeUsage{Input: 100, CachedInput: 40}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				pgstoretest.SeedLeaseFamily(t, r, org)
				event := pgstoretest.AttemptEvent(pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(1)))
				event.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 100, CachedInput: 40, Output: 1_000_000, To: yesterday}}
				pgstoretest.RecordLeaseAttempt(t, r, org, event)
				if err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: 3, ProjectID: "prj_1", NativeID: "wi_3", Number: 3, Title: "Example"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				claim := pgstoretest.LeaseClaim(2)
				claim.IssueID = 3
				other := pgstoretest.AttemptEvent(pgstoretest.ClaimLease(t, r, org, claim))
				other.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 7, Output: 500, To: today}}
				pgstoretest.RecordLeaseAttempt(t, r, org, other)
				decreased := event
				decreased.Sequence, decreased.PayloadObjectKey = 2, "example/decreased"
				decreased.Usage = []tracker.NativeUsage{test.decrease}
				decreased.Usage[0].Model, decreased.Usage[0].To = "example-model", today
				pgstoretest.RecordLeaseAttempt(t, r, org, decreased)
				grown := event
				grown.Sequence, grown.PayloadObjectKey = 3, "example/grown"
				grown.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 110, CachedInput: 40, Output: 1_000_020, To: today}}
				pgstoretest.RecordLeaseAttempt(t, r, org, grown)
				for _, want := range []struct {
					project       tracker.ProjectID
					issue         string
					day           *time.Time
					input, output int64
				}{
					{project: "prj_1", day: &yesterday, input: 100, output: 1_000_000},
					{project: "prj_1", day: &today, input: 17, output: 520},
					{project: "prj_1", issue: "wi_3", input: 7, output: 500},
					{project: "prj_1", issue: "wi_1", input: 110, output: 1_000_020},
				} {
					err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
						spend, err := reader.(leasestore.RunnerSpendReader).RunnerTokenSpend(t.Context(), want.project, want.issue, want.day, nil)
						if err == nil && (spend.InputTokens != want.input || spend.OutputTokens != want.output) {
							t.Errorf("%s %s %v spend = %+v, want input %d output %d", org, want.project, want.day, spend, want.input, want.output)
						}
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestRunnerSpendConcurrentModelAdmission(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	day := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, r, org)
		if err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: 3, ProjectID: "prj_1", NativeID: "wi_3", Number: 3, Title: "Example"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		first := pgstoretest.AttemptEvent(pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(1)))
		claim := pgstoretest.LeaseClaim(2)
		claim.IssueID = 3
		second := pgstoretest.AttemptEvent(pgstoretest.ClaimLease(t, r, org, claim))
		for batch := 0; batch < 7; batch++ {
			first.Sequence = int64(batch + 1)
			first.PayloadObjectKey = fmt.Sprintf("example/observation-%d", batch)
			first.Usage = nil
			for index := batch * 32; index < min((batch+1)*32, 199); index++ {
				first.Usage = append(first.Usage, tracker.NativeUsage{Model: fmt.Sprintf("example-model-%03d", index), To: day})
			}
			pgstoretest.RecordLeaseAttempt(t, r, org, first)
		}
		first.Sequence++
		first.PayloadObjectKey = "example/last-model"
		first.Usage = []tracker.NativeUsage{{Model: "example-last", Input: 11, To: day}}
		second.Usage = []tracker.NativeUsage{{Model: "example-overflow", Input: 29, To: day}}
		var competing *pgstoretest.RepositoryTask
		if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			if _, err := w.RecordAttempt(t.Context(), first); err != nil {
				return err
			}
			competing = pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteLeases(ctx, org, corestore.Background, func(other leasestore.Writer) error {
					_, err := other.RecordAttempt(ctx, second)
					return err
				})
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-competing.Done:
					return nil
				default:
				}
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%')`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("competing observation has not reached admission")
				}
				return nil
			})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := competing.Wait(t); err != nil {
			t.Errorf("concurrent observation beyond report bound = %v, want accepted", err)
		}
		if err := r.ReadLeases(t.Context(), org, corestore.Serving, func(reader leasestore.Reader) error {
			spend, err := reader.(leasestore.RunnerSpendReader).RunnerTokenSpend(t.Context(), "prj_1", "", &day, nil)
			if err == nil && (len(spend.ByModel) != 201 || spend.ByModel[0] != (store.ModelTokenSpend{InputTokens: 29, TotalTokens: 29, Sessions: 1}) || spend.InputTokens != 40 || spend.TotalTokens != 40 || spend.Sessions != 201) {
				t.Errorf("spend after concurrent admission = %+v", spend)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
