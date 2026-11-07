package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkItemDeletionRetainsHistoryAndReceipts(t *testing.T) {
	var baseline [2]int64
	for _, size := range []int{2, 50} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			admin := template.Database(t)
			repository, pool := repositoryFor(t, admin, smallPool())
			fixtures := map[tracker.OrganizationID]historyFixture{}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedRepository(t, repository, org)
				fixtures[org] = seedHistory(t, repository, org, "retained")
				for i := 1; i < size; i++ {
					event := historyEvent(t, fixtures[org].content, org, fmt.Sprintf("history_%d", i))
					if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
						t.Fatal(err)
					}
				}
			}
			repository, counts := attemptCountedRepository(t, repository, pool)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				f := fixtures[org]
				counts.statements.Store(0)
				counts.exchanges.Store(0)
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_first", "wi_first_1") })
				if err != nil {
					t.Fatal(err)
				}
				cost := [2]int64{counts.statements.Load(), counts.exchanges.Load()}
				if baseline == [2]int64{} {
					baseline = cost
				} else if cost != baseline {
					t.Fatalf("deletion cost grew: baseline=%v cost=%v", baseline, cost)
				}
				t.Logf("tenant=%s history=%d statements=%d exchanges=%d", org, size, cost[0], cost[1])
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					if _, err := r.WorkItem(t.Context(), "prj_first", "wi_first_1"); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("deleted work item=%v", err)
					}
					events, err := r.Events(t.Context(), "prj_first", "wi_first_1", 0, 100)
					if err != nil {
						return err
					}
					if len(events) != size {
						t.Fatalf("retained events=%d want=%d", len(events), size)
					}
					snapshot, err := r.CollaborationVersion(t.Context(), f.snapshot.RecordID, 2)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(snapshot, f.snapshot) {
						t.Fatal("snapshot changed during deletion")
					}
					receipt, err := r.Receipt(t.Context(), f.receipt.ActorID, f.receipt.Operation, f.receipt.CommandKey)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(receipt, f.receipt) {
						t.Fatal("receipt changed during deletion")
					}
					change, err := r.Change(t.Context(), f.change.ID)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(change, f.change) {
						t.Fatal("change changed during deletion")
					}
					versions, err := r.Versions(t.Context(), f.change.ID, 0, 10)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(versions, []eventstore.Version{f.version}) {
						t.Fatal("version changed during deletion")
					}
					evidence, err := r.Evidence(t.Context(), f.change.ID, 0, 10)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(evidence, []eventstore.Evidence{f.evidence}) {
						t.Fatal("evidence changed during deletion")
					}
					comments, err := r.Comments(t.Context(), "prj_first", "wi_first_1", 0, 10)
					if err != nil {
						return err
					}
					if len(comments) != 0 {
						t.Fatal("live comments survived deletion")
					}
					_, err = r.WorkItem(t.Context(), "prj_first", "wi_first_2")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				event := historyEvent(t, f.content, org, "after_delete")
				err = repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err })
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("late event=%v", err)
				}
				_, err = admin.ExecContext(t.Context(), `INSERT INTO collaboration_events (organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT organization_id,project_id,work_item_id,'late_event',999,'issue.edited',1,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE organization_id=$1 LIMIT 1`, org)
				requireSQLState(t, err, "23503")
				err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_first", NativeID: "wi_first_1", Number: 3, Title: "Example"})
					return err
				})
				requireSQLState(t, err, "23505")
				_, err = admin.ExecContext(t.Context(), `DELETE FROM collaboration_events WHERE organization_id=$1`, org)
				requireSQLState(t, err, "23514")
				_, err = admin.ExecContext(t.Context(), `UPDATE issues SET native_id='changed' WHERE organization_id=$1 AND native_id='wi_first_2'`, org)
				requireSQLState(t, err, "23514")
				if org == "org_first" {
					if err := repository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.WorkItem(t.Context(), "prj_first", "wi_first_1")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestWorkItemDeletionRetainsMigratedIdentities(t *testing.T) {
	admin := template.Database(t)
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(t.Context()); err != nil {
		t.Fatal(err)
	}
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM work_item_identities WHERE organization_id=$1`, org).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 4 {
			t.Fatalf("backfilled identities=%d", count)
		}
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_first", "wi_first_1") }); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Down(t.Context())
	requireSQLState(t, err, "23514")
}

func TestWorkItemDeletionRefusesActiveExecution(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		lease := claimLease(t, repository, org, leaseClaim(1))
		remove := func() error {
			return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_1", "wi_1") })
		}
		requireSQLState(t, remove(), "23503")
		expireLeaseFixture(t, admin, org, lease.LeaseID)
		requireSQLState(t, remove(), "23503")
		if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=$3::timestamptz,renewed_at=$3::timestamptz,expires_at=$3::timestamptz+interval '1 hour' WHERE organization_id=$1 AND lease_id=$2`, org, lease.LeaseID, lease.RenewedAt); err != nil {
			t.Fatal(err)
		}
		recordLeaseAttempt(t, repository, org, attemptEvent(lease))
		if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=renewed_at WHERE organization_id=$1 AND lease_id=$2`, org, lease.LeaseID); err != nil {
			t.Fatal(err)
		}
		requireSQLState(t, remove(), "23503")
		if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET status='cancelled' WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if err := remove(); err != nil {
			t.Fatal(err)
		}
		_, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=NULL WHERE organization_id=$1`, org)
		requireSQLState(t, err, "23503")
		_, err = admin.ExecContext(t.Context(), `UPDATE native_attempts SET status='running' WHERE organization_id=$1`, org)
		requireSQLState(t, err, "23503")
		_, err = admin.ExecContext(t.Context(), `INSERT INTO native_attempts SELECT * FROM native_attempts WHERE organization_id=$1`, org)
		requireSQLState(t, err, "23503")
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND status='cancelled'`, org).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatal("terminal attempt was lost")
		}
	}
}

func TestWorkItemDeletionOrdersConcurrentHistoryInsertion(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	admin.SetMaxOpenConns(2)
	seedRepository(t, repository, "org_first")
	seedHistory(t, repository, "org_first", "concurrent")
	insert := `INSERT INTO collaboration_events (organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT organization_id,project_id,work_item_id,'concurrent_event',999,'issue.edited',1,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE organization_id='org_first' LIMIT 1`
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var rollbackOnce sync.Once
	t.Cleanup(func() {
		rollbackOnce.Do(func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error(err)
			}
		})
	})
	if _, err := tx.ExecContext(t.Context(), insert); err != nil {
		t.Fatal(err)
	}
	deletion := startRepositoryTask(t, func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_first", corestore.Background, func(w tenantstore.Writer) error { return w.DeleteWorkItem(ctx, "prj_first", "wi_first_1") })
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("deletion is not waiting for the history insertion")
		}
		return nil
	})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := deletion.wait(t); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id='org_first'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("retained concurrent events=%d", count)
	}
}

func TestRetainedWorkItemIdentitiesEnforceTenantReadOnlyAccess(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		tx, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `SET LOCAL ROLE detent_runtime_v2`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM work_item_identities WHERE native_id='wi_first_1'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("tenant identity visibility=%d", count)
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO work_item_identities VALUES($1,999,'prj_first','fabricated')`, org)
		requireSQLState(t, err, "42501")
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}
