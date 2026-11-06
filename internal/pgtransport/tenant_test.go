package pgtransport_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var template = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE protected (tenant text,id text,PRIMARY KEY(tenant,id)); ALTER TABLE protected ENABLE ROW LEVEL SECURITY; ALTER TABLE protected FORCE ROW LEVEL SECURITY; CREATE POLICY tenant ON protected USING(tenant=current_setting('detent.organization_id',true)); GRANT SELECT,INSERT ON protected TO PUBLIC; INSERT INTO protected VALUES ('first','same'),('second','same')`)
	return err
})

func TestMain(m *testing.M) {
	code := m.Run()
	if err := template.Close(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
func fixture(t *testing.T) (*sql.DB, *pgxpool.Config) {
	t.Helper()
	admin := template.Database(t)
	role := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD 'fixture-only'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.WithoutCancel(t.Context()), `DROP ROLE `+role); err != nil {
			t.Error(err)
		}
	})
	var database string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `ALTER DATABASE "`+database+`" CONNECTION LIMIT 4`); err != nil {
		t.Fatal(err)
	}
	return admin, template.NativeConfig(t, admin, role, "fixture-only")
}
func openPools(t *testing.T, config *pgxpool.Config, writer *pgxpool.Config) *pgtransport.Pools {
	t.Helper()
	pools, err := pgtransport.Open(t.Context(), config, writer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	return pools
}
func begin(t *testing.T, pools *pgtransport.Pools, options pgtransport.Options) *pgtransport.Transaction {
	t.Helper()
	tx, err := pools.Begin(t.Context(), pgtransport.Scope{Organization: "first"}, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(t.Context()); err != nil {
			t.Error(err)
		}
	})
	return tx
}
func reuse(t *testing.T, pools *pgtransport.Pools) {
	t.Helper()
	for _, tenant := range []string{"first", "second", "first"} {
		tx, err := pools.Begin(t.Context(), pgtransport.Scope{Organization: tracker.OrganizationID(tenant)}, pgtransport.Options{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		var got string
		if err := tx.QueryRow(t.Context(), `SELECT tenant FROM protected WHERE id=$1`, "same").Scan(&got); err != nil || got != tenant {
			t.Fatalf("tenant=%s result=%s err=%v", tenant, got, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestTenantBinding(t *testing.T) {
	_, config := fixture(t)
	pools := openPools(t, config, nil)
	reuse(t, pools)
	if _, err := pools.Begin(t.Context(), pgtransport.Scope{}, pgtransport.Options{}); !errors.Is(err, pgtransport.ErrTenantRequired) {
		t.Fatalf("missing tenant: %v", err)
	}
	if _, err := pools.Begin(t.Context(), pgtransport.Scope{Organization: "first"}, pgtransport.Options{Isolation: "unsupported"}); err == nil {
		t.Fatal("unsupported isolation accepted")
	}
	tx := new(pgtransport.Transaction)
	if _, err := tx.Query(t.Context(), `SELECT * FROM protected`); !errors.Is(err, pgtransport.ErrUnbound) {
		t.Fatalf("guard: %v", err)
	}
	conn, err := pgx.ConnectConfig(t.Context(), config.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var count int
	if err := conn.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM protected`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbound rows=%d err=%v", count, err)
	}
}
func TestCommitRejectsAbortedTransaction(t *testing.T) {
	_, config := fixture(t)
	pools := openPools(t, config, nil)
	tx := begin(t, pools, pgtransport.Options{})
	if _, err := tx.Exec(t.Context(), `INSERT INTO protected VALUES ('first','discarded')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT 1/0`); pgtransport.SQLState(err) != "22012" {
		t.Fatalf("division: %v", err)
	}
	if err := tx.Commit(t.Context()); !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("aborted COMMIT=%v", err)
	}
	tx = begin(t, pools, pgtransport.Options{ReadOnly: true})
	var count int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM protected WHERE id='discarded'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("discarded rows=%d err=%v", count, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	reuse(t, pools)
}
func TestTransactionCleanup(t *testing.T) {
	for _, name := range []string{"bind failure", "rollback", "cancelled rollback", "failed batch", "failed initial batch", "open batch", "open rows", "commit open rows", "reader rejects writes", "cancelled query"} {
		t.Run(name, func(t *testing.T) {
			_, config := fixture(t)
			pools := openPools(t, config, nil)
			if name == "bind failure" || name == "failed initial batch" {
				work := &pgx.Batch{}
				work.Queue(`INSERT INTO protected VALUES ('first','must-not-run')`)
				scope := pgtransport.Scope{Organization: "first\x00"}
				if name == "failed initial batch" {
					scope.Organization = "first"
					work = &pgx.Batch{}
					work.Queue(`SELECT 1/0`)
				}
				tx, results, err := pools.BeginBatch(t.Context(), scope, pgtransport.Options{}, work)
				if name == "bind failure" {
					if pgtransport.SQLState(err) != "22021" {
						t.Fatalf("bind failure=%v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if err := results.Close(); pgtransport.SQLState(err) != "22012" {
						t.Fatalf("initial batch=%v", err)
					}
					if err := tx.Commit(t.Context()); !errors.Is(err, pgx.ErrTxCommitRollback) {
						t.Fatalf("initial batch commit=%v", err)
					}
				}
			} else {
				tx := begin(t, pools, pgtransport.Options{ReadOnly: name == "reader rejects writes"})
				switch name {
				case "rollback", "cancelled rollback":
					if _, err := tx.Exec(t.Context(), `INSERT INTO protected VALUES ('first','discarded')`); err != nil {
						t.Fatal(err)
					}
					ctx := t.Context()
					if name == "cancelled rollback" {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					}
					if err := tx.Rollback(ctx); err != nil {
						t.Fatal(err)
					}
				case "failed batch":
					batch := &pgx.Batch{}
					batch.Queue(`SELECT 1/0`)
					batch.Queue(`SELECT * FROM protected`)
					results := tx.SendBatch(t.Context(), batch)
					if err := results.Close(); err == nil {
						t.Fatal("failed batch succeeded")
					}
					if err := tx.Commit(t.Context()); !errors.Is(err, pgx.ErrTxCommitRollback) {
						t.Fatalf("commit=%v", err)
					}
				case "open batch":
					batch := &pgx.Batch{}
					batch.Queue(`SELECT * FROM protected`)
					results := tx.SendBatch(t.Context(), batch)
					if err := tx.Rollback(t.Context()); err != nil {
						t.Fatal(err)
					}
					if err := results.Close(); err == nil {
						t.Fatal("open batch survived rollback")
					}
				case "open rows", "commit open rows":
					rows, err := tx.Query(t.Context(), `SELECT * FROM protected`)
					if err != nil {
						t.Fatal(err)
					}
					defer rows.Close()
					if name == "commit open rows" {
						if err := tx.Commit(t.Context()); !errors.Is(err, pgtransport.ErrOpenResults) {
							t.Fatalf("commit=%v", err)
						}
					} else if err := tx.Rollback(t.Context()); err != nil {
						t.Fatal(err)
					}
				case "reader rejects writes":
					if _, err := tx.Exec(t.Context(), `INSERT INTO protected VALUES ('first','forbidden')`); pgtransport.SQLState(err) != "25006" {
						t.Fatalf("read-only write=%v", err)
					}
					if err := tx.Rollback(t.Context()); err != nil {
						t.Fatal(err)
					}
				case "cancelled query":
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					_, err := tx.Exec(ctx, `SELECT * FROM protected`)
					if pgtransport.Classify(err) != pgtransport.Cancel {
						t.Fatalf("cancelled query=%v", err)
					}
					if err := tx.Rollback(ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			reuse(t, pools)
			tx := begin(t, pools, pgtransport.Options{ReadOnly: true})
			var count int
			if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM protected WHERE id<>'same'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unexpected writes=%d err=%v", count, err)
			}
		})
	}
}
func TestPoolRoutingAndIsolation(t *testing.T) {
	_, read := fixture(t)
	write := read.Copy()
	read.ConnConfig.RuntimeParams["application_name"] = "example-reader"
	write.ConnConfig.RuntimeParams["application_name"] = "example-writer"
	read.ConnConfig.RuntimeParams["default_transaction_isolation"] = "serializable"
	write.ConnConfig.RuntimeParams["default_transaction_isolation"] = "serializable"
	pools := openPools(t, read, write)
	for _, readOnly := range []bool{true, false} {
		tx := begin(t, pools, pgtransport.Options{ReadOnly: readOnly})
		var app, isolation string
		if err := tx.QueryRow(t.Context(), `SELECT current_setting('application_name'),current_setting('transaction_isolation')`).Scan(&app, &isolation); err != nil {
			t.Fatal(err)
		}
		want := "example-writer"
		if readOnly {
			want = "example-reader"
		}
		if app != want || isolation != "read committed" {
			t.Fatalf("route=%s isolation=%s", app, isolation)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestGuardDetectsBypass(t *testing.T) {
	_, config := fixture(t)
	violations := 0
	config.ConnConfig.Tracer = pgtest.NewTenantGuard([]string{"protected"}, func(string) { violations++ })
	conn, err := pgx.ConnectConfig(t.Context(), config.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var count int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM protected`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if violations != 1 || count != 0 {
		t.Fatalf("violations=%d rows=%d", violations, count)
	}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT * FROM protected`)
	if err := conn.SendBatch(t.Context(), batch).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO protected VALUES ('first','unguarded')`); pgtransport.SQLState(err) != "42501" {
		t.Fatalf("unguarded insert=%v", err)
	}
	if violations != 3 {
		t.Fatalf("batch violations=%d", violations)
	}
}
