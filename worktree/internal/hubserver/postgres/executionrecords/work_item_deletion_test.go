package executionrecords_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkItemDeletionRetainsHistoryAndReceipts(t *testing.T) {
	t.Parallel()
	var baseline [2]int64
	for _, size := range []int{2, 50} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			admin := pgstoretest.Template.Database(t)
			repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
			fixtures := map[tracker.OrganizationID]pgstoretest.HistoryFixture{}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				pgstoretest.SeedRepository(t, repository, org)
				fixtures[org] = pgstoretest.SeedHistory(t, repository, org, "retained")
				for i := 1; i < size; i++ {
					event := pgstoretest.HistoryEvent(t, fixtures[org].Content, org, fmt.Sprintf("history_%d", i))
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
					snapshot, err := r.CollaborationVersion(t.Context(), f.Snapshot.RecordID, 2)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(snapshot, f.Snapshot) {
						t.Fatal("snapshot changed during deletion")
					}
					receipt, err := r.Receipt(t.Context(), f.Receipt.ActorID, f.Receipt.Operation, f.Receipt.CommandKey)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(receipt, f.Receipt) {
						t.Fatal("receipt changed during deletion")
					}
					change, err := r.Change(t.Context(), f.Change.ID)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(change, f.Change) {
						t.Fatal("change changed during deletion")
					}
					versions, err := r.Versions(t.Context(), f.Change.ID, 0, 10)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(versions, []eventstore.Version{f.Version}) {
						t.Fatal("version changed during deletion")
					}
					evidence, err := r.Evidence(t.Context(), f.Change.ID, 0, 10)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(evidence, []eventstore.Evidence{f.Evidence}) {
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
				event := pgstoretest.HistoryEvent(t, f.Content, org, "after_delete")
				err = repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err })
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("late event=%v", err)
				}
				_, err = admin.ExecContext(t.Context(), `INSERT INTO collaboration_events (organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT organization_id,project_id,work_item_id,'late_event',999,'issue.edited',1,actor_kind,actor_principal_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE organization_id=$1 LIMIT 1`, org)
				pgstoretest.RequireSQLState(t, err, "23503")
				err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_first", NativeID: "wi_first_1", Number: 3, Title: "Example"})
					return err
				})
				pgstoretest.RequireSQLState(t, err, "23505")
				_, err = admin.ExecContext(t.Context(), `DELETE FROM collaboration_events WHERE organization_id=$1`, org)
				pgstoretest.RequireSQLState(t, err, "23514")
				_, err = admin.ExecContext(t.Context(), `UPDATE issues SET native_id='changed' WHERE organization_id=$1 AND native_id='wi_first_2'`, org)
				pgstoretest.RequireSQLState(t, err, "23514")
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

func seedIdentityMigrationIssues(t *testing.T, admin *sql.DB, organization tracker.OrganizationID) {
	t.Helper()
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, organization, pgstoretest.HistoryAt()); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"first", "second"} {
		for _, statement := range []string{
			`INSERT INTO projects(organization_id,id,name,slug,profile) VALUES($1,'prj_'||$2,$2,$2,'native')`,
			`INSERT INTO issues(organization_id,native_id,project_id,number,title,native_source_key) SELECT $1,'wi_'||$2||'_'||n,'prj_'||$2,n,'Example','source_'||n FROM generate_series(1,2) n`,
		} {
			if _, err := admin.ExecContext(t.Context(), statement, organization, suffix); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestWorkItemDeletionRetainsMigratedIdentities(t *testing.T) {
	t.Parallel()
	const migrationVersion = 20261107060339
	admin, fixture := pgstoretest.MigrationDatabaseAtVersion(t, migrationVersion-1)
	provider, err := pgstoretest.MigrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	pool := pgstoretest.RolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedIdentityMigrationIssues(t, admin, org)
	}
	admin.SetMaxOpenConns(1)
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), migrationVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
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
	const organization, project, item = "org_first", "prj_first", "wi_first_1"
	ctx := t.Context()
	tx := pgstoretest.TenantTx(t, pgstoretest.RuntimeFor(t, pool, pool), pool, organization)
	if _, err := tx.Exec(ctx, `UPDATE projects SET duplicate_source_sequence=duplicate_source_sequence+1 WHERE organization_id=$1 AND id=$2`, organization, project); err != nil {
		t.Fatal(err)
	}
	deleted, err := tx.Exec(ctx, `DELETE FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, organization, project, item)
	if err != nil || deleted.RowsAffected() != 1 {
		t.Fatalf("delete migrated work item: rows=%d error=%v", deleted.RowsAffected(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Down(t.Context())
	pgstoretest.RequireSQLState(t, err, "23514")
}

func TestWorkItemDeletionRefusesActiveExecution(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		remove := func() error {
			return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_1", "wi_1") })
		}
		pgstoretest.RequireSQLState(t, remove(), "23503")
		expireLeaseFixture(t, admin, org, lease.LeaseID)
		pgstoretest.RequireSQLState(t, remove(), "23503")
		if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=$3::timestamptz,renewed_at=$3::timestamptz,expires_at=$3::timestamptz+interval '1 hour' WHERE organization_id=$1 AND lease_id=$2`, org, lease.LeaseID, lease.RenewedAt); err != nil {
			t.Fatal(err)
		}
		pgstoretest.RecordLeaseAttempt(t, repository, org, pgstoretest.AttemptEvent(lease))
		if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=renewed_at WHERE organization_id=$1 AND lease_id=$2`, org, lease.LeaseID); err != nil {
			t.Fatal(err)
		}
		pgstoretest.RequireSQLState(t, remove(), "23503")
		if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET status='cancelled' WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		if err := remove(); err != nil {
			t.Fatal(err)
		}
		_, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=NULL WHERE organization_id=$1`, org)
		pgstoretest.RequireSQLState(t, err, "23503")
		_, err = admin.ExecContext(t.Context(), `UPDATE native_attempts SET status='running' WHERE organization_id=$1`, org)
		pgstoretest.RequireSQLState(t, err, "23503")
		_, err = admin.ExecContext(t.Context(), `INSERT INTO native_attempts SELECT * FROM native_attempts WHERE organization_id=$1`, org)
		pgstoretest.RequireSQLState(t, err, "23503")
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
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	admin.SetMaxOpenConns(2)
	pgstoretest.SeedRepository(t, repository, "org_first")
	pgstoretest.SeedHistory(t, repository, "org_first", "concurrent")
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
	deletion := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
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
	if err := deletion.Wait(t); err != nil {
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
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
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
		pgstoretest.RequireSQLState(t, err, "42501")
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkItemDeletionRemovesOnlyLiveAttachmentReferences(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		pgstoretest.SeedTenantAccess(t, repository, org)
		pgstoretest.SeedHistory(t, repository, org, "attachment")
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachments(organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at) VALUES($1,'prj_first','attachment_shared','first','example.txt','text/plain',1,$2,$3)`, org, strings.Repeat("a", 64), pgstoretest.HistoryAt()); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_first", AttachmentID: "attachment_shared", WorkItemID: "wi_first_1", RetainWithWorkItem: true}); err != nil {
				return err
			}
			return w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_first", AttachmentID: "attachment_shared", WorkItemID: "wi_first_2", CommentID: "comment_first_2_1"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteComment(t.Context(), "prj_first", "wi_first_2", "comment_first_2_1")
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			refs, err := r.AttachmentReferences(t.Context(), "prj_first", "attachment_shared", 10)
			if err != nil {
				return err
			}
			if len(refs) != 1 || refs[0].WorkItemID != "wi_first_1" {
				t.Fatalf("comment cleanup references=%+v", refs)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_first", "wi_first_1") }); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			refs, err := r.AttachmentReferences(t.Context(), "prj_first", "attachment_shared", 10)
			if err != nil {
				return err
			}
			if len(refs) != 0 {
				t.Fatalf("issue cleanup references=%+v", refs)
			}
			record, err := r.Attachment(t.Context(), "prj_first", "attachment_shared")
			if err != nil {
				return err
			}
			if record.Metadata.DeletedAt != nil || record.ObjectDeletedAt != nil {
				t.Fatal("reference cleanup deleted attachment metadata")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if org == "org_first" {
			if err := repository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
				refs, err := r.AttachmentReferences(t.Context(), "prj_first", "attachment_shared", 10)
				if err == nil && len(refs) != 2 {
					t.Fatal("cleanup crossed tenants")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRetainedIdentityMigrationIncludesConcurrentIssueCreation(t *testing.T) {
	t.Parallel()
	const migrationVersion = 20261107060339
	admin, fixture := pgstoretest.MigrationDatabaseAtVersion(t, migrationVersion-1)
	provider, err := pgstoretest.MigrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	pool := pgstoretest.RolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	admin.SetMaxOpenConns(2)
	seedIdentityMigrationIssues(t, admin, "org_first")
	runtime := pgstoretest.RuntimeFor(t, pool, pool)
	inserted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	writer := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
		tx, err := runtime.Begin(ctx, "org_first", pgtransport.Options{})
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title) VALUES('org_first','prj_first','wi_during_migration',3,'Example')`); err != nil {
			return err
		}
		close(inserted)
		select {
		case <-release:
			return tx.Commit(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	pgstoretest.WaitRepositorySignal(t, inserted)
	migration := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error { _, err := provider.UpTo(ctx, migrationVersion); return err })
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE a.datname=current_database() AND l.relation='public.issues'::regclass AND l.mode='ShareRowExclusiveLock' AND NOT l.granted)`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("migration is not waiting for the concurrent issue writer")
		}
		return nil
	})
	releaseOnce.Do(func() { close(release) })
	if err := writer.Wait(t); err != nil {
		t.Fatal(err)
	}
	if err := migration.Wait(t); err != nil {
		t.Fatal(err)
	}
	var captured bool
	if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM work_item_identities WHERE organization_id='org_first' AND native_id='wi_during_migration')`).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	if !captured {
		t.Fatal("migration missed an issue committed before capture installation")
	}
}
