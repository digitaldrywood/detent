package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func observeStatement(ctx context.Context, started time.Time, query string) {
	if timing, ok := ctx.Value(transactionTimingKey{}).(transactionTiming); ok && timing.statement != nil {
		timing.statement(started, query)
	}
}

type timedQueries struct{ pgquery.DBTX }

func (q timedQueries) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	started := time.Now()
	defer observeStatement(ctx, started, query)
	return q.DBTX.Exec(ctx, query, args...)
}

func (q timedQueries) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	started := time.Now()
	rows, err := q.DBTX.Query(ctx, query, args...)
	observeStatement(ctx, started, query)
	return rows, err
}

func (q timedQueries) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	started := time.Now()
	defer observeStatement(ctx, started, query)
	return q.DBTX.QueryRow(ctx, query, args...)
}
