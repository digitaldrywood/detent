package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeColumnRecordsPreservesCloseErrors(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("close rows")
	for _, tt := range []struct {
		name     string
		raw      string
		closeErr error
		invalid  bool
	}{
		{name: "successful read", raw: `{}`},
		{name: "close failure after exhaustion", raw: `{}`, closeErr: closeErr},
		{name: "decode failure", raw: `{`, invalid: true},
		{name: "decode failure retains close failure", raw: `{`, closeErr: closeErr, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := &changeCloseErrorRows{raw: tt.raw, closeErr: tt.closeErr}
			db := sql.OpenDB(changeCloseErrorConnector{rows: rows})
			t.Cleanup(func() { db.Close() })
			values, err := changeColumnRecords[tracker.ChangeVersion](t.Context(), db, "SELECT record_json FROM change_versions")
			if tt.closeErr != nil && !errors.Is(err, tt.closeErr) {
				t.Fatalf("close failure lost: %v", err)
			}
			var syntaxErr *json.SyntaxError
			if errors.As(err, &syntaxErr) != tt.invalid {
				t.Fatalf("decode failure = %v, want %v: %v", syntaxErr, tt.invalid, err)
			}
			if !tt.invalid && tt.closeErr == nil && (err != nil || len(values) != 1) {
				t.Fatalf("read records = %v, %v", values, err)
			}
			if rows.closes != 1 {
				t.Fatalf("closed driver rows %d times, want 1", rows.closes)
			}
		})
	}
}

type changeCloseErrorConnector struct {
	rows *changeCloseErrorRows
}

func (changeCloseErrorConnector) Driver() driver.Driver { return nil }

func (c changeCloseErrorConnector) Connect(context.Context) (driver.Conn, error) {
	return changeCloseErrorConn{rows: c.rows}, nil
}

type changeCloseErrorConn struct {
	driver.Conn
	rows *changeCloseErrorRows
}

func (c changeCloseErrorConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return c.rows, nil
}

func (changeCloseErrorConn) Close() error { return nil }

type changeCloseErrorRows struct {
	raw      string
	closeErr error
	read     bool
	closes   int
}

func (*changeCloseErrorRows) Columns() []string { return []string{"record_json"} }
func (r *changeCloseErrorRows) Close() error {
	r.closes++
	return r.closeErr
}
func (r *changeCloseErrorRows) Next(values []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	values[0] = r.raw
	return nil
}
