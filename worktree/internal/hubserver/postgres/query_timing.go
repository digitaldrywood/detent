package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func observeStatement(ctx context.Context, started time.Time, query string) {
	if timing, ok := ctx.Value(transactionTimingKey{}).(transactionTiming); ok && timing.statement != nil {
		timing.statement(started, query)
	}
}

type timedQueries struct{ pgquery.DBTX }

func (q timedQueries) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	started := time.Now()
	defer observeStatement(ctx, started, query)
	return q.DBTX.ExecContext(ctx, query, args...)
}

func (q timedQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	started := time.Now()
	rows, err := q.DBTX.QueryContext(ctx, query, args...)
	observeStatement(ctx, started, query)
	return rows, err
}

func (q timedQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	started := time.Now()
	defer observeStatement(ctx, started, query)
	return q.DBTX.QueryRowContext(ctx, query, args...)
}
