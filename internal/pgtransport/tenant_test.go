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
)

var template = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE protected (tenant text, id text, PRIMARY KEY(tenant,id)); ALTER TABLE protected ENABLE ROW LEVEL SECURITY; ALTER TABLE protected FORCE ROW LEVEL SECURITY; CREATE POLICY tenant ON protected USING(tenant = current_setting('detent.organization_id',true)); GRANT SELECT,INSERT ON protected TO PUBLIC; INSERT INTO protected VALUES ('first','same'),('second','same')`)
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

func TestTenantBinding(t *testing.T) {
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
	pool := template.OpenNativeRole(t, admin, role, "fixture-only")
	pools := pgtransport.Pools{Reader: pool, Writer: pool}
	for _, tenant := range []string{"first", "second", "first"} {
		tx, err := pools.Begin(t.Context(), pgtransport.Scope{Organization: tracker.OrganizationID(tenant)}, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		var got string
		if err := tx.QueryRow(t.Context(), `SELECT tenant FROM protected WHERE id=$1`, "same").Scan(&got); err != nil || got != tenant {
			t.Fatalf("tenant=%s result=%s err=%v", tenant, got, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM protected`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbound rows=%d err=%v", count, err)
	}
	if _, err := pools.Begin(t.Context(), pgtransport.Scope{}, pgx.TxOptions{}); !errors.Is(err, pgtransport.ErrTenantRequired) {
		t.Fatalf("missing tenant: %v", err)
	}
	tx := new(pgtransport.Transaction)
	if _, err := tx.Query(t.Context(), `SELECT * FROM protected`); !errors.Is(err, pgtransport.ErrUnbound) {
		t.Fatalf("guard: %v", err)
	}
}

func TestGuardDetectsBypass(t *testing.T) {
	admin := template.Database(t)
	var role string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	config := template.NativeConfig(t, admin, role, "fixture-only")
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
	if violations != 1 {
		t.Fatalf("violations=%d", violations)
	}
	batch := &pgx.Batch{}
	batch.Queue(`SELECT * FROM protected`)
	if err := conn.SendBatch(t.Context(), batch).Close(); err != nil {
		t.Fatal(err)
	}
	if violations != 2 {
		t.Fatalf("batch violations=%d", violations)
	}
}
