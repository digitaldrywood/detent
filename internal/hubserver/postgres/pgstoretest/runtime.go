package pgstoretest

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func RolePool(t *testing.T, admin *sql.DB, group, attributes string) *pgxpool.Pool {
	t.Helper()
	return RolePoolFromTemplate(t, Template, admin, group, attributes)
}

func RolePoolFromTemplate(t *testing.T, owner *pgtest.Template, admin *sql.DB, group, attributes string) *pgxpool.Pool {
	t.Helper()
	admin.SetMaxIdleConns(1)
	var database string
	var limit int
	if err := admin.QueryRowContext(t.Context(), `SELECT datname,datconnlimit FROM pg_database WHERE datname=current_database()`).Scan(&database, &limit); err != nil {
		t.Fatal(err)
	}
	if limit >= 0 && limit < 3 {
		if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 3"); err != nil {
			t.Fatal(err)
		}
	}
	name := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" "+attributes); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("PostgreSQL tests require a process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	if group != "" {
		if _, err := admin.ExecContext(t.Context(), "GRANT "+pgx.Identifier{group}.Sanitize()+" TO "+quoted); err != nil {
			t.Fatal(err)
		}
	}
	config := owner.NativeConfig(t, admin, name, password)
	config.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func RuntimeFor(t *testing.T, reader, writer *pgxpool.Pool) *postgres.Runtime {
	t.Helper()
	runtime, err := postgres.NewRuntime(t.Context(), reader, writer)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

type TenantTransaction interface {
	pgtransport.Executor
	Commit(context.Context) error
	Rollback(context.Context) error
}

type unboundTransaction struct{ pgx.Tx }

func (t unboundTransaction) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	return t.Tx.Exec(pgtest.UnboundProbe(ctx), query, args...)
}

func (t unboundTransaction) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	return t.Tx.Query(pgtest.UnboundProbe(ctx), query, args...)
}

func (t unboundTransaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return t.Tx.QueryRow(pgtest.UnboundProbe(ctx), query, args...)
}

func TenantTx(t *testing.T, runtime *postgres.Runtime, pool *pgxpool.Pool, organization string) TenantTransaction {
	t.Helper()
	var tx TenantTransaction
	var err error
	if organization == "" {
		var raw pgx.Tx
		raw, err = pool.BeginTx(t.Context(), pgx.TxOptions{})
		tx = unboundTransaction{raw}
	} else {
		tx, err = runtime.Begin(t.Context(), tracker.OrganizationID(organization), pgtransport.Options{})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(t.Context()) })
	return tx
}
