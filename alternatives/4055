package sqlstmt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"modernc.org/sqlite"
)

type cachedConnector struct{}

func (cachedConnector) Driver() driver.Driver { return &sqlite.Driver{} }

func (c cachedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(":memory:")
	if err != nil {
		return nil, err
	}
	return &cachedConn{Conn: conn}, nil
}

type cachedConn struct {
	driver.Conn
	statements Cache
}

func (c *cachedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.statements.ExecContext(ctx, c.Conn, query, args)
}

func (c *cachedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.statements.QueryContext(ctx, c.Conn, query, args)
}

func (c *cachedConn) Close() error {
	return errors.Join(c.statements.Close(), c.Conn.Close())
}

func TestActiveRowsSurviveStatementChurn(t *testing.T) {
	t.Parallel()
	for _, transactional := range []bool{false, true} {
		t.Run(fmt.Sprintf("transactional=%t", transactional), func(t *testing.T) {
			t.Parallel()
			db := sql.OpenDB(cachedConnector{})
			db.SetMaxOpenConns(1)
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := db.ExecContext(t.Context(), "CREATE TABLE records(organization TEXT NOT NULL,id INTEGER NOT NULL,value TEXT NOT NULL,PRIMARY KEY(organization,id))"); err != nil {
				t.Fatal(err)
			}
			for _, org := range []string{"org_alpha", "org_beta"} {
				for id := range 2 {
					if _, err := db.ExecContext(t.Context(), "INSERT INTO records VALUES(?,?,?)", org, id, fmt.Sprintf("%s-%d", org, id)); err != nil {
						t.Fatal(err)
					}
				}
			}
			conn, err := db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var session interface {
				QueryContext(context.Context, string, ...any) (*sql.Rows, error)
				QueryRowContext(context.Context, string, ...any) *sql.Row
			} = conn
			if transactional {
				tx, err := conn.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				session = tx
			}
			query := "SELECT value FROM records WHERE organization=? ORDER BY id"
			rows, err := session.QueryContext(t.Context(), query, "org_alpha")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			columns, err := rows.ColumnTypes()
			if err != nil || len(columns) != 1 || columns[0].DatabaseTypeName() != "TEXT" {
				t.Fatalf("column metadata = %v, error=%v", columns, err)
			}
			if !rows.Next() {
				t.Fatal("missing first organization row")
			}
			var got string
			if err := rows.Scan(&got); err != nil || got != "org_alpha-0" {
				t.Fatalf("first row = %q, error=%v", got, err)
			}
			for i := range 2 * statementSlots {
				var result int
				if err := session.QueryRowContext(t.Context(), fmt.Sprintf("SELECT %d", i)).Scan(&result); err != nil || result != i {
					t.Fatalf("statement %d = %d, error=%v", i, result, err)
				}
			}
			if err := session.QueryRowContext(t.Context(), query, "org_beta").Scan(&got); err != nil || got != "org_beta-0" {
				t.Fatalf("nested scope = %q, error=%v", got, err)
			}
			if !rows.Next() {
				t.Fatal("statement churn consumed active rows")
			}
			if err := rows.Scan(&got); err != nil || got != "org_alpha-1" {
				t.Fatalf("remaining row = %q, error=%v", got, err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if err := session.QueryRowContext(t.Context(), query).Scan(&got); err == nil {
				t.Fatal("missing bindings reused a prior scope")
			}
			if err := session.QueryRowContext(t.Context(), query, "org_beta").Scan(&got); err != nil || got != "org_beta-0" {
				t.Fatalf("query after binding failure = %q, error=%v", got, err)
			}
		})
	}
}
