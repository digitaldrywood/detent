package hubserver

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func seedRequestRetentionRow(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, table string, at time.Time, route, kind, caller string, count int64) {
	t.Helper()
	var query string
	args := []any{at.Unix(), route}
	switch table {
	case "request_minutes":
		query = "INSERT INTO request_minutes VALUES(?,?,?,?,2,?,?,3,3,3,'[]')"
		args = append(args, kind, caller, count, float64(count)*3)
	case "request_hours":
		query = "INSERT INTO request_hours VALUES(?,?,2,?,?,3,3,3,'[]')"
		args = append(args, count, float64(count)*3)
	case "request_days":
		query = "INSERT INTO request_days VALUES(?,?,?,2,?,?,3)"
		args = append(args, kind, count, float64(count)*3)
	default:
		t.Fatalf("unknown metrics table %q", table)
	}
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}
