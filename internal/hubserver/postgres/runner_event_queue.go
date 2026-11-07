package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
)

type runEventQueue struct{ batch *pgx.Batch }
type runEventQueueRow struct{ query *pgx.QueuedQuery }

func (q runEventQueue) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return runEventQueueRow{q.batch.Queue(sql, args...)}
}
func (r runEventQueueRow) Scan(dest ...any) error {
	r.query.QueryRow(func(row pgx.Row) error { return row.Scan(dest...) })
	return nil
}
func (q runEventQueue) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.batch.Queue(sql, args...)
	return pgconn.NewCommandTag(""), nil
}
func (q runEventQueue) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, corestore.ErrInvalidPage
}

func (f *runnerEventSession) flushRunEvents(ctx context.Context) error {
	if f.pending.Len() == 0 {
		return nil
	}
	db, ok := f.session.db.(timedQueries)
	if !ok {
		return corestore.ErrInvalidPage
	}
	executor, ok := db.DBTX.(pgtransport.Executor)
	if !ok {
		return corestore.ErrInvalidPage
	}
	batch := f.pending
	f.pending = new(pgx.Batch)
	started := time.Now()
	err := executor.SendBatch(ctx, batch).Close()
	for _, q := range batch.QueuedQueries {
		observeStatement(ctx, started, q.SQL)
	}
	return mapError(ctx, err)
}
