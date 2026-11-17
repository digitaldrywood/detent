package billingrecords_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type deadlockVictimTrace struct{ victims atomic.Int64 }

func (*deadlockVictimTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (trace *deadlockVictimTrace) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	var failure *pgconn.PgError
	if errors.As(data.Err, &failure) && failure.Code == "40P01" {
		trace.victims.Add(1)
	}
}

func TestReportUsageImportRetriesDeadlockVictim(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	config := pgstoretest.SmallPool()
	repository, pool := pgstoretest.RepositoryFor(t, admin, config)
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
	if _, err := admin.ExecContext(ctx, "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+fmt.Sprintf(" CONNECTION LIMIT %d", config.MaxConnections+2)); err != nil {
		t.Fatal(err)
	}
	admin.SetMaxOpenConns(2)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	trace := &deadlockVictimTrace{}
	traced := pgstoretest.TracedFeaturePool(t, pool, trace)
	pool.Close()
	pool = traced
	repository, err := postgres.NewRepository(ctx, pool, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
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
			victims := trace.victims.Load()
			sharers := [2]*attemptSharer{shareAttempt(t, admin, org), shareAttempt(t, admin, org)}
			record := reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_retry", Period: pgstoretest.HistoryAt().Truncate(time.Hour), Provider: "example", Model: "example-model", Input: 7, Output: 3, Cost: 1.5, Currency: "USD"}, UpdatedAt: pgstoretest.HistoryAt()}
			importer := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error { return repository.ImportReportUsageFact(ctx, record) })
			var first, second *attemptSharer
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-importer.Done:
					t.Fatalf("import finished before the conflicting attempt lock: %v", importer.Err)
				default:
				}
				var blocker int64
				if err := pool.QueryRow(ctx, `SELECT coalesce((SELECT b.pid FROM pg_stat_activity w CROSS JOIN LATERAL unnest(pg_blocking_pids(w.pid)) b(pid) WHERE w.datname=current_database() AND w.usename=$1 AND w.wait_event_type='Lock' LIMIT 1),0)`, login).Scan(&blocker); err != nil {
					return err
				}
				for i, sharer := range sharers {
					if sharer.pid == blocker {
						first, second = sharer, sharers[1-i]
						return nil
					}
				}
				return errors.New("import has not reached the attempt lock")
			})
			issueLocked := make(chan error, 1)
			go func() {
				_, err := second.tx.ExecContext(t.Context(), `SELECT 1 FROM issues WHERE organization_id=$1 AND project_id='prj_1' AND native_id='wi_1' FOR UPDATE`, org)
				issueLocked <- err
			}()
			issueJoined := false
			t.Cleanup(func() {
				if !issueJoined {
					<-issueLocked
				}
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case err := <-issueLocked:
					issueJoined = true
					t.Fatalf("the second attempt sharer did not wait for the import's issue lock: %v", err)
				default:
				}
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, second.pid).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("the second attempt sharer has not reached the import's issue lock")
				}
				return nil
			})
			first.finish(t)
			err = <-issueLocked
			issueJoined = true
			if err != nil {
				t.Fatal(err)
			}
			second.finish(t)
			if err := importer.Wait(t); err != nil {
				t.Fatalf("deadlock victim import error: %v", err)
			}
			if trace.victims.Load() <= victims {
				t.Fatal("import was not chosen as a deadlock victim")
			}
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

type attemptSharer struct {
	conn *sql.Conn
	tx   *sql.Tx
	pid  int64
}

func shareAttempt(t *testing.T, admin *sql.DB, org tracker.OrganizationID) *attemptSharer {
	t.Helper()
	ctx := t.Context()
	conn, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	sharer := &attemptSharer{conn: conn, tx: tx}
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&sharer.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL deadlock_timeout='1h'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM native_attempts WHERE organization_id=$1 AND id='attempt_retry' FOR SHARE`, org); err != nil {
		t.Fatal(err)
	}
	return sharer
}

func (sharer *attemptSharer) finish(t *testing.T) {
	t.Helper()
	if err := sharer.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := sharer.conn.Close(); err != nil {
		t.Fatal(err)
	}
}
