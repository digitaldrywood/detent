package pgcore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func ObserveStatement(ctx context.Context, started time.Time, query string) {
	if timing, ok := ctx.Value(transactionTimingKey{}).(transactionTiming); ok && timing.statement != nil {
		timing.statement(started, query)
	}
}

type TimedQueries struct{ pgquery.DBTX }

func (q TimedQueries) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	started := time.Now()
	defer ObserveStatement(ctx, started, query)
	return q.DBTX.Exec(ctx, query, args...)
}

func (q TimedQueries) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	started := time.Now()
	rows, err := q.DBTX.Query(ctx, query, args...)
	ObserveStatement(ctx, started, query)
	return rows, err
}

func (q TimedQueries) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	started := time.Now()
	defer ObserveStatement(ctx, started, query)
	return q.DBTX.QueryRow(ctx, query, args...)
}
