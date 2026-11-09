package sqlstmt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

type cachedConnector struct{}

func (cachedConnector) Driver() driver.Driver { return cachedDriver{} }

func (cachedConnector) Connect(context.Context) (driver.Conn, error) {
	return &cachedConn{Conn: protocolConn{}}, nil
}

type cachedDriver struct{}

func (cachedDriver) Open(string) (driver.Conn, error) { return protocolConn{}, nil }

type protocolConn struct{}

func (protocolConn) Prepare(query string) (driver.Stmt, error) {
	return &protocolStatement{query: query}, nil
}
func (protocolConn) Close() error              { return nil }
func (protocolConn) Begin() (driver.Tx, error) { return protocolTransaction{}, nil }
func (protocolConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	statement := &protocolStatement{query: query}
	return statement.QueryContext(ctx, args)
}

type protocolTransaction struct{}

func (protocolTransaction) Commit() error   { return nil }
func (protocolTransaction) Rollback() error { return nil }

type protocolStatement struct {
	query  string
	closed bool
}

func (s *protocolStatement) Close() error                             { s.closed = true; return nil }
func (s *protocolStatement) NumInput() int                            { return strings.Count(s.query, "?") }
func (*protocolStatement) Exec([]driver.Value) (driver.Result, error) { return nil, driver.ErrSkip }
func (*protocolStatement) Query([]driver.Value) (driver.Rows, error)  { return nil, driver.ErrSkip }
func (s *protocolStatement) QueryContext(_ context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.closed {
		return nil, errors.New("statement is closed")
	}
	if len(args) != s.NumInput() {
		return nil, errors.New("binding count mismatch")
	}
	if s.query == "SELECT value FROM records WHERE organization=? ORDER BY id" {
		organization, ok := args[0].Value.(string)
		if !ok {
			return nil, errors.New("organization binding is not text")
		}
		var values []driver.Value
		switch organization {
		case "org_alpha":
			values = []driver.Value{"org_alpha-0", "org_alpha-1"}
		case "org_beta":
			values = []driver.Value{"org_beta-0", "org_beta-1"}
		}
		return &protocolRows{statement: s, values: values, kind: "TEXT"}, nil
	}
	value, err := strconv.Atoi(strings.TrimPrefix(s.query, "SELECT "))
	if err != nil {
		return nil, err
	}
	return &protocolRows{statement: s, values: []driver.Value{int64(value)}, kind: "INTEGER"}, nil
}

type protocolRows struct {
	statement *protocolStatement
	values    []driver.Value
	kind      string
}

func (*protocolRows) Columns() []string                       { return []string{"value"} }
func (*protocolRows) Close() error                            { return nil }
func (r *protocolRows) ColumnTypeDatabaseTypeName(int) string { return r.kind }
func (r *protocolRows) Next(dest []driver.Value) error {
	if r.statement.closed {
		return errors.New("active statement was closed")
	}
	if len(r.values) == 0 {
		return io.EOF
	}
	dest[0], r.values = r.values[0], r.values[1:]
	return nil
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
