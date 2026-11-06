package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

type backgroundCPUQuery struct {
	statement string
	args      []any
}

type backgroundCPUReader struct {
	nativeQueryer
	queries []backgroundCPUQuery
}

func (r *backgroundCPUReader) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	r.queries = append(r.queries, backgroundCPUQuery{statement, args})
	return r.nativeQueryer.QueryContext(ctx, statement, args...)
}

func (r *backgroundCPUReader) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	r.queries = append(r.queries, backgroundCPUQuery{statement, args})
	return r.nativeQueryer.QueryRowContext(ctx, statement, args...)
}

func (r *backgroundCPUReader) plans(t *testing.T) string {
	t.Helper()
	var plans []string
	for _, query := range r.queries {
		rows, err := r.nativeQueryer.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.statement, query.args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plans = append(plans, detail)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return strings.Join(plans, "\n")
}
