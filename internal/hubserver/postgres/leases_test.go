package postgres_test

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

func seedLeaseFamily(t *testing.T, r *postgres.Repository, org tracker.OrganizationID) {
	t.Helper()
	err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
		if err := seedCoreOrganization(t.Context(), w, org); err != nil {
			return err
		}
		for id := int64(1); id <= 2; id++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: id, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", id)), Number: 1, Title: "Example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		for _, id := range []string{"machine_1", "machine_2"} {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: id, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func leaseClaim(id int64) leasestore.Claim {
	return leasestore.Claim{IssueID: id, ExpectedRevision: 1, MachineID: fmt.Sprintf("machine_%d", id), SessionID: fmt.Sprintf("session_%d", id), LeaseID: fmt.Sprintf("lease_%d", id), TTL: time.Hour}
}

func claimLease(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, request leasestore.Claim) leasestore.Lease {
	t.Helper()
	var lease leasestore.Lease
	err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		var err error
		lease, err = w.Claim(t.Context(), request)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func leaseFence(lease leasestore.Lease) leasestore.Fence {
	return leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
}

func attemptEvent(lease leasestore.Lease) leasestore.AttemptEvent {
	return leasestore.AttemptEvent{ID: "attempt_" + lease.LeaseID, Fence: leaseFence(lease), RunID: "run_" + lease.LeaseID, Sequence: 1, Status: "running", PayloadObjectKey: "example/start", DispatchGeneration: 2}
}

func TestAttemptEventDoesNotDeadlockIssueTransition(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	seedLeaseFamily(t, repository, "org_first")
	lease := claimLease(t, repository, "org_first", leaseClaim(1))
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	transition := startRepositoryTask(t, func(ctx context.Context) error {
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
	waitRepositorySignal(t, locked)
	var transitionPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItem :one%'`, login).Scan(&transitionPID); err != nil {
		t.Fatal(err)
	}
	attempt := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteLeases(ctx, "org_first", corestore.Background, func(w leasestore.Writer) error {
			_, err := w.RecordAttempt(ctx, attemptEvent(lease))
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
	if err := transition.wait(t); err != nil {
		t.Errorf("issue transition lock: %v", err)
	}
	if err := attempt.wait(t); err != nil {
		t.Errorf("attempt event: %v", err)
	}
}

func TestAttemptEventDoesNotDeadlockUsageAccounting(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	seedLeaseFamily(t, repository, "org_first")
	lease := claimLease(t, repository, "org_first", leaseClaim(1))
	start := recordLeaseAttempt(t, repository, "org_first", attemptEvent(lease))
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
	event := attemptEvent(lease)
	event.Sequence, event.PayloadObjectKey, event.Usage = 2, "example/next", usage
	transition := startRepositoryTask(t, func(ctx context.Context) error {
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
	waitRepositorySignal(t, locked)
	var transitionPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItem :one%'`, login).Scan(&transitionPID); err != nil {
		t.Fatal(err)
	}
	accounting := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_first", corestore.Background, func(w tenantstore.Writer) error {
			return w.(leasestore.RunnerUsageWriter).AccountRunnerTokenUsage(ctx, start, usage)
		})
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-accounting.done:
			t.Fatalf("accounting ended before lock observation: %v", accounting.err)
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
	if err := transition.wait(t); err != nil {
		t.Errorf("attempt event: %v", err)
	}
	if err := accounting.wait(t); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Errorf("stale usage accounting error = %v", err)
	}
}

func TestImportedWorkItemLocksProjectBeforeWriting(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 5, ServingConnections: 2, BackgroundConnections: 2})
	seedLeaseFamily(t, repository, "org_first")
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	content := eventstore.NewContent(newHistoryObjects())
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
	owner := startRepositoryTask(t, func(ctx context.Context) error {
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
	waitRepositorySignal(t, locked)
	var ownerPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItemProject :one%'`, login).Scan(&ownerPID); err != nil {
		t.Fatal(err)
	}
	imported := startRepositoryTask(t, func(ctx context.Context) error { return repository.ImportCoreWorkItem(ctx, record) })
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-imported.done:
			t.Fatalf("import finished before acquiring the project lock: %v", imported.err)
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
	replayed := startRepositoryTask(t, func(ctx context.Context) error { return repository.ImportCoreWorkItem(ctx, record) })
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-replayed.done:
			t.Fatalf("concurrent replay finished before acquiring the project lock: %v", replayed.err)
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
	if err := owner.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := imported.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := replayed.wait(t); err != nil {
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
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 5, ServingConnections: 1, BackgroundConnections: 3})
	seedLeaseFamily(t, repository, "org_first")
	var login string
	if err := pool.QueryRow(t.Context(), `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt, err := eventstore.NewContent(newHistoryObjects()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_first", ActorID: "production-import", Operation: "core-lease", CommandKey: "imported-lease", RequestHash: artifact.Digest([]byte("imported-lease")), CreatedAt: now}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	lease := leasestore.Lease{OrganizationID: "org_first", LeaseID: "imported-lease", FencingToken: 1000000, IssueID: 1, MachineID: "machine_1", SessionID: "imported-session", WorkItemRevision: 1, AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(time.Hour)}
	locked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	transition := startRepositoryTask(t, func(ctx context.Context) error {
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
	waitRepositorySignal(t, locked)
	var transitionPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItem :one%'`, login).Scan(&transitionPID); err != nil {
		t.Fatal(err)
	}
	imported := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.ImportCoreLease(ctx, postgres.CoreLeaseImport{Lease: lease, Receipt: receipt})
	})
	duplicate := startRepositoryTask(t, func(ctx context.Context) error {
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
	if err := transition.wait(t); err != nil {
		t.Errorf("issue transition lock: %v", err)
	}
	if err := imported.wait(t); err != nil {
		t.Errorf("lease import: %v", err)
	}
	if err := duplicate.wait(t); err != nil {
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

func recordLeaseAttempt(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, event leasestore.AttemptEvent) leasestore.Attempt {
	t.Helper()
	var attempt leasestore.Attempt
	err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(t.Context(), event)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return attempt
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
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, r, org)
		for _, id := range []int64{1, 2} {
			t.Run(fmt.Sprintf("%s/%d", org, id), func(t *testing.T) {
				request := leaseClaim(id)
				lease := claimLease(t, r, org, request)
				request.LeaseID = "unused_replay_id"
				replay := claimLease(t, r, org, request)
				if replay.LeaseID != lease.LeaseID || replay.FencingToken != lease.FencingToken || !replay.ExpiresAt.Equal(lease.ExpiresAt) || lease.WorkItemRevision != 1 {
					t.Fatalf("claim replay changed ownership: %+v, first %+v", replay, lease)
				}
				event := attemptEvent(lease)
				start := recordLeaseAttempt(t, r, org, event)
				if start.Sequence != 1 || start.Status != "running" || start.WorkItemRevision != lease.WorkItemRevision {
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
				if replay := recordLeaseAttempt(t, r, org, event); replay.ID != start.ID || replay.Sequence != start.Sequence || replay.FencingToken != start.FencingToken {
					t.Fatalf("legacy receipt replay changed attempt: %+v", replay)
				}
				event.Sequence, event.PayloadObjectKey = 2, "example/checkpoint"
				event.CheckpointObjectKey, event.ArtifactsObjectKey = new("example/checkpoint"), new("example/artifacts")
				recordLeaseAttempt(t, r, org, event)
				event.Sequence, event.Status, event.PayloadObjectKey = 3, "succeeded", "example/finish"
				event.CheckpointObjectKey, event.ArtifactsObjectKey = nil, nil
				finished := recordLeaseAttempt(t, r, org, event)
				if finished.Status != "succeeded" || finished.Sequence != 3 || finished.CheckpointObjectKey == nil || *finished.CheckpointObjectKey != "example/checkpoint" || finished.ArtifactsObjectKey == nil || *finished.ArtifactsObjectKey != "example/artifacts" {
					t.Fatalf("finish lost checkpoint: %+v", finished)
				}
				requireLeaseUsage(t, r, org, lease.MachineID, 1)
				err = r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					renewed, err := w.RenewWithLandingCapacity(t.Context(), leaseFence(lease), 2*time.Hour, 3)
					if err != nil {
						return err
					}
					if renewed.FencingToken != lease.FencingToken || !renewed.ExpiresAt.After(lease.ExpiresAt) || renewed.LandingCapacity != 3 {
						t.Errorf("renew changed owner or failed to extend expiry: %+v", renewed)
					}
					released, err := w.Release(t.Context(), leaseFence(lease), "finished")
					if err == nil && (released.ReleasedAt == nil || released.ReleaseReason != "finished") {
						t.Errorf("release = %+v", released)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				recordLeaseAttempt(t, r, org, event)
				requireLeaseUsage(t, r, org, lease.MachineID, 0)
				request.LeaseID, request.SessionID = lease.LeaseID+"_next", lease.SessionID+"_next"
				next := claimLease(t, r, org, request)
				if next.FencingToken <= lease.FencingToken {
					t.Errorf("next fence = %d, previous %d", next.FencingToken, lease.FencingToken)
				}
				continued := attemptEvent(next)
				continued.RunID = event.RunID
				recordLeaseAttempt(t, r, org, continued)
			})
		}
	}
}

func TestLeaseIssueReadsReturnActiveLeaseAndBoundedAttemptPages(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
	var attempts []leasestore.Attempt
	for sequence := 1; sequence <= 3; sequence++ {
		request := leaseClaim(1)
		request.LeaseID = fmt.Sprintf("lease_page_%d", sequence)
		request.SessionID = fmt.Sprintf("session_page_%d", sequence)
		lease := claimLease(t, r, "org_first", request)
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
		event := attemptEvent(lease)
		event.ID = fmt.Sprintf("attempt_page_%d", sequence)
		event.RunID = fmt.Sprintf("run_page_%d", sequence)
		attempts = append(attempts, recordLeaseAttempt(t, r, "org_first", event))
		if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(writer leasestore.Writer) error {
			_, err := writer.Release(t.Context(), leaseFence(lease), "finished")
			return err
		}); err != nil {
			t.Fatal(err)
		}
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
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, smallPool())
			seedLeaseFamily(t, r, "org_first")
			lease := claimLease(t, r, "org_first", leaseClaim(1))
			event := attemptEvent(lease)
			recordLeaseAttempt(t, r, "org_first", event)
			if state == "expired" || state == "reassigned" {
				expireLeaseFixture(t, admin, "org_first", lease.LeaseID)
			}
			if state == "released" {
				if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.Release(t.Context(), leaseFence(lease), "cancelled")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "reassigned" {
				request := leaseClaim(1)
				request.LeaseID, request.SessionID = "replacement", "replacement"
				claimLease(t, r, "org_first", request)
			}
			for _, operation := range []string{"renew", "release", "finish", "wrong fence", "expire live"} {
				if state == "live" && operation != "wrong fence" && operation != "expire live" || state != "live" && operation == "expire live" {
					continue
				}
				t.Run(operation, func(t *testing.T) {
					err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
						fence := leaseFence(lease)
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
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
	lease := claimLease(t, r, "org_first", leaseClaim(1))
	event := attemptEvent(lease)
	recordLeaseAttempt(t, r, "org_first", event)
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
	if got := recordLeaseAttempt(t, r, "org_first", event); got.Sequence != 1 || got.PayloadObjectKey != event.PayloadObjectKey {
		t.Fatalf("rejections changed attempt: %+v", got)
	}
}

func TestLeaseAttemptUsageCorrectionPreservesTerminalLifecycle(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
	lease := claimLease(t, r, "org_first", leaseClaim(1))
	started := attemptEvent(lease)
	recordLeaseAttempt(t, r, "org_first", started)
	finished := started
	finished.Sequence = 2
	finished.Status = "succeeded"
	finished.PayloadObjectKey = "example/finish"
	terminal := recordLeaseAttempt(t, r, "org_first", finished)
	corrected := finished
	corrected.Sequence = 3
	corrected.PayloadObjectKey = "example/finish-corrected"
	corrected.UsageCorrection = true
	updated := recordLeaseAttempt(t, r, "org_first", corrected)
	if updated.Sequence != 3 || updated.Status != terminal.Status || !updated.UpdatedAt.Equal(terminal.UpdatedAt) || updated.PayloadObjectKey != corrected.PayloadObjectKey {
		t.Fatalf("usage correction changed terminal attempt lifecycle: before=%+v after=%+v", terminal, updated)
	}
}

func waitLeaseLock(t *testing.T, admin *sql.DB, count int, tasks ...*repositoryTask) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		for _, task := range tasks {
			select {
			case <-task.done:
				t.Fatalf("transaction exited before the lock barrier: %v", task.err)
			default:
			}
		}
		var waiting int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting < count {
			return fmt.Errorf("waiting transactions = %d, want %d", waiting, count)
		}
		return nil
	})
}

func TestLeaseCompetingClaims(t *testing.T) {
	for _, sameSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_session_%t", sameSession), func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
			seedLeaseFamily(t, r, "org_first")
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
			var tasks []*repositoryTask
			for i, workload := range []corestore.Workload{corestore.Serving, corestore.Background} {
				tasks = append(tasks, startRepositoryTask(t, func(ctx context.Context) error {
					return r.WriteLeases(ctx, "org_first", workload, func(w leasestore.Writer) error {
						request := leaseClaim(1)
						if !sameSession {
							request.LeaseID, request.SessionID = fmt.Sprintf("lease_%d", i), fmt.Sprintf("session_%d", i)
						}
						var err error
						leases[i], err = w.Claim(ctx, request)
						return err
					})
				}))
			}
			waitLeaseLock(t, admin, 2, tasks...)
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			var winners, conflicts int
			for _, task := range tasks {
				err := task.wait(t)
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
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
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
			request := leaseClaim(1)
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
	request := leaseClaim(1)
	request.ExpectedRevision = 2
	err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error { _, err := w.Claim(t.Context(), request); return err })
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("archived claim = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), "Example") {
		t.Fatal("error contains customer content")
	}
}

func TestLeaseClaimReplayOnlyRaisesLandingCapacity(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	seedLeaseFamily(t, r, "org_first")
	firstRequest := leaseClaim(1)
	firstRequest.LandingCapacity = 2
	first := claimLease(t, r, "org_first", firstRequest)

	replayRequest := firstRequest
	replayRequest.LeaseID = "lease_replay"
	replayRequest.LandingCapacity = 4
	replay := claimLease(t, r, "org_first", replayRequest)
	if replay.LeaseID != first.LeaseID || replay.FencingToken != first.FencingToken || replay.LandingCapacity != 4 {
		t.Fatalf("raised-capacity replay=%+v original=%+v", replay, first)
	}

	replayRequest.LeaseID = "lease_replay_lower"
	replayRequest.LandingCapacity = 1
	lowered := claimLease(t, r, "org_first", replayRequest)
	if lowered.LeaseID != first.LeaseID || lowered.FencingToken != first.FencingToken || lowered.LandingCapacity != 4 {
		t.Fatalf("lower-capacity replay=%+v original=%+v", lowered, first)
	}
}

func TestRunnerSpendRejectsCumulativeModelOverflow(t *testing.T) {
	for _, scenario := range []string{"repeated observations", "issue across days"} {
		t.Run(scenario, func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			day := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedLeaseFamily(t, r, org)
				lease := claimLease(t, r, org, leaseClaim(1))
				event := attemptEvent(lease)
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
					recordLeaseAttempt(t, r, org, event)
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
				recordLeaseAttempt(t, r, org, event)
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
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	day := time.Date(2026, 12, 25, 12, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	third := day.AddDate(0, 0, 2)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, r, org)
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
			request := leaseClaim(machine)
			request.IssueID, request.LeaseID, request.SessionID = issue, lease, "session_"+lease
			return request
		}
		release := func(lease leasestore.Lease) {
			t.Helper()
			if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), leaseFence(lease), "finished")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, flood := range []struct {
			claim leasestore.Claim
			at    time.Time
		}{
			{claim: leaseClaim(1), at: day},
			{claim: claim(1, 4, "lease_flood_4"), at: third},
		} {
			flooding := claimLease(t, r, org, flood.claim)
			event := attemptEvent(flooding)
			for batch := 0; batch < 7; batch++ {
				event.Sequence = int64(batch + 1)
				event.PayloadObjectKey = fmt.Sprintf("example/flood-%d", batch)
				event.Usage = nil
				for index := batch * 32; index < min((batch+1)*32, 200); index++ {
					event.Usage = append(event.Usage, tracker.NativeUsage{Model: fmt.Sprintf("example-fabricated-%03d", index), To: flood.at})
				}
				recordLeaseAttempt(t, r, org, event)
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
			lease := claimLease(t, r, org, completion.claim)
			finished := attemptEvent(lease)
			recordLeaseAttempt(t, r, org, finished)
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

func TestRunnerSpendRejectsCumulativeDecrease(t *testing.T) {
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
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedLeaseFamily(t, r, org)
				event := attemptEvent(claimLease(t, r, org, leaseClaim(1)))
				event.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 100, CachedInput: 40, Output: 1_000_000, To: yesterday}}
				recordLeaseAttempt(t, r, org, event)
				if err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: 3, ProjectID: "prj_1", NativeID: "wi_3", Number: 3, Title: "Example"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				claim := leaseClaim(2)
				claim.IssueID = 3
				other := attemptEvent(claimLease(t, r, org, claim))
				other.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 7, Output: 500, To: today}}
				recordLeaseAttempt(t, r, org, other)
				decreased := event
				decreased.Sequence, decreased.PayloadObjectKey = 2, "example/decreased"
				decreased.Usage = []tracker.NativeUsage{test.decrease}
				decreased.Usage[0].Model, decreased.Usage[0].To = "example-model", today
				err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
					_, err := w.RecordAttempt(t.Context(), decreased)
					return err
				})
				if !errors.Is(err, leasestore.ErrAttemptConflict) {
					t.Errorf("decreased cumulative usage error = %v, want attempt conflict", err)
				}
				grown := event
				grown.Sequence, grown.PayloadObjectKey = 2, "example/grown"
				grown.Usage = []tracker.NativeUsage{{Model: "example-model", Input: 110, CachedInput: 40, Output: 1_000_020, To: today}}
				recordLeaseAttempt(t, r, org, grown)
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
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	day := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, r, org)
		if err := r.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: 3, ProjectID: "prj_1", NativeID: "wi_3", Number: 3, Title: "Example"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		first := attemptEvent(claimLease(t, r, org, leaseClaim(1)))
		claim := leaseClaim(2)
		claim.IssueID = 3
		second := attemptEvent(claimLease(t, r, org, claim))
		for batch := 0; batch < 7; batch++ {
			first.Sequence = int64(batch + 1)
			first.PayloadObjectKey = fmt.Sprintf("example/observation-%d", batch)
			first.Usage = nil
			for index := batch * 32; index < min((batch+1)*32, 199); index++ {
				first.Usage = append(first.Usage, tracker.NativeUsage{Model: fmt.Sprintf("example-model-%03d", index), To: day})
			}
			recordLeaseAttempt(t, r, org, first)
		}
		first.Sequence++
		first.PayloadObjectKey = "example/last-model"
		first.Usage = []tracker.NativeUsage{{Model: "example-last", Input: 11, To: day}}
		second.Usage = []tracker.NativeUsage{{Model: "example-overflow", Input: 29, To: day}}
		var competing *repositoryTask
		if err := r.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			if _, err := w.RecordAttempt(t.Context(), first); err != nil {
				return err
			}
			competing = startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteLeases(ctx, org, corestore.Background, func(other leasestore.Writer) error {
					_, err := other.RecordAttempt(ctx, second)
					return err
				})
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-competing.done:
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
		if err := competing.wait(t); err != nil {
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
