package billingrecords_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
)

func TestReportUsageImportRetriesDeadlockVictim(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	ctx := t.Context()
	var database, login string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT session_user`).Scan(&login); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "ALTER ROLE "+pgx.Identifier{login}.Sanitize()+" IN DATABASE "+pgx.Identifier{database}.Sanitize()+" SET deadlock_timeout='10ms'"); err != nil {
		t.Fatal(err)
	}
	pool.Reset()
	for _, org := range []tracker.OrganizationID{"org_retry_first", "org_retry_second"} {
		t.Run(string(org), func(t *testing.T) {
			if _, err := admin.ExecContext(ctx, `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, pgstoretest.HistoryAt()); err != nil {
				t.Fatal(err)
			}
			pgstoretest.SeedLeaseFamily(t, repository, org)
			lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
			ref, err := pgstoretest.NewHistoryObjects().StoreDetail(ctx, string(org), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: "attempt_retry", Fence: pgstoretest.LeaseFence(lease), RunID: "run_retry", Sequence: 1, Status: "running", StartedAt: pgstoretest.HistoryAt(), PayloadObjectKey: ref.Key})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var deadlocks int64
			if err := admin.QueryRowContext(ctx, `SELECT deadlocks FROM pg_stat_database WHERE datname=current_database()`).Scan(&deadlocks); err != nil {
				t.Fatal(err)
			}
			holder, err := admin.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Close() }()
			tx, err := holder.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, `SET LOCAL deadlock_timeout='1h'`); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM native_attempts WHERE organization_id=$1 AND id='attempt_retry' FOR UPDATE`, org); err != nil {
				t.Fatal(err)
			}
			record := reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_retry", Period: pgstoretest.HistoryAt().Truncate(time.Hour), Provider: "example", Model: "example-model", Input: 7, Output: 3, Cost: 1.5, Currency: "USD"}, UpdatedAt: pgstoretest.HistoryAt()}
			importer := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error { return repository.ImportReportUsageFact(ctx, record) })
			testenv.WaitUntil(t, ctx, func() error {
				select {
				case <-importer.Done:
					t.Fatalf("import finished before the conflicting attempt lock: %v", importer.Err)
				default:
				}
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock')`, login).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("import has not reached the attempt lock")
				}
				return nil
			})
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM issues WHERE organization_id=$1 AND project_id='prj_1' AND native_id='wi_1' FOR UPDATE`, org); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := holder.Close(); err != nil {
				t.Fatal(err)
			}
			if err := importer.Wait(t); err != nil {
				t.Fatalf("deadlock victim import error: %v", err)
			}
			testenv.WaitUntil(t, ctx, func() error {
				var current int64
				if err := admin.QueryRowContext(ctx, `SELECT deadlocks FROM pg_stat_database WHERE datname=current_database()`).Scan(&current); err != nil {
					return err
				}
				if current <= deadlocks {
					return errors.New("import was not chosen as a deadlock victim")
				}
				return nil
			})
			var rows int
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM attempt_usage WHERE organization_id=$1 AND attempt_id='attempt_retry'`, org).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("imported usage rows=%d, want 1", rows)
			}
		})
	}
}
