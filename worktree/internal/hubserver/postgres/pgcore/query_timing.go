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

type timedStatement struct {
	observe  func()
	observed bool
}

func startStatement(ctx context.Context, query string) *timedStatement {
	started := time.Now()
	return &timedStatement{observe: func() { ObserveStatement(ctx, started, query) }}
}

func (s *timedStatement) finish() {
	if !s.observed {
		s.observed = true
		s.observe()
	}
}

type timedRows struct {
	pgx.Rows
	statement *timedStatement
}

func (r timedRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.statement.finish()
	return false
}

func (r timedRows) Close() {
	r.Rows.Close()
	r.statement.finish()
}

type timedRow struct {
	pgx.Row
	statement *timedStatement
}

func (r timedRow) Scan(dest ...any) error {
	defer r.statement.finish()
	return r.Row.Scan(dest...)
}

func (q TimedQueries) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	statement := startStatement(ctx, query)
	rows, err := q.DBTX.Query(ctx, query, args...)
	if err != nil {
		statement.finish()
		return rows, err
	}
	return timedRows{Rows: rows, statement: statement}, nil
}

func (q TimedQueries) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	statement := startStatement(ctx, query)
	return timedRow{Row: q.DBTX.QueryRow(ctx, query, args...), statement: statement}
}
