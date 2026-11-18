package pgtransport

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type writeBatch struct {
	ctx     context.Context
	conn    *pgx.Conn
	tracer  pgx.BatchTracer
	queue   []pgx.QueuedQuery
	results *pgconn.MultiResultReader
	rows    *writeBatchRows
	err     error
	closed  bool
}

func sendWriteBatch(ctx context.Context, conn *pgx.Conn, batch *pgx.Batch) pgx.BatchResults {
	config := conn.Config()
	if batch.Len() == 0 || config.DefaultQueryExecMode == pgx.QueryExecModeExec {
		return conn.SendBatch(ctx, batch)
	}
	b := &writeBatch{ctx: ctx, conn: conn, queue: make([]pgx.QueuedQuery, 0, batch.Len())}
	b.tracer, _ = config.Tracer.(pgx.BatchTracer)
	if b.tracer != nil {
		b.ctx = b.tracer.TraceBatchStart(ctx, conn, pgx.TraceBatchStartData{Batch: batch})
	}
	var wire pgconn.Batch
	var encoder pgx.ExtendedQueryBuilder
	for _, queued := range batch.QueuedQueries {
		q := *queued
		var rewriter pgx.QueryRewriter
		for len(q.Arguments) > 0 {
			option, ok := q.Arguments[0].(pgx.QueryRewriter)
			if !ok {
				break
			}
			rewriter = option
			q.Arguments = q.Arguments[1:]
		}
		if rewriter != nil {
			q.SQL, q.Arguments, b.err = rewriter.RewriteQuery(b.ctx, conn, q.SQL, q.Arguments)
			if b.err != nil {
				return b
			}
		}
		if b.err = encoder.Build(conn.TypeMap(), nil, q.Arguments); b.err != nil {
			return b
		}
		wire.ExecParams(q.SQL, encoder.ParamValues, nil, encoder.ParamFormats, encoder.ResultFormats)
		b.queue = append(b.queue, q)
	}
	b.results = conn.PgConn().ExecBatch(b.ctx, &wire)
	return b
}

func (b *writeBatch) next() (*pgx.QueuedQuery, error) {
	if b.rows != nil {
		b.rows.Close()
	}
	if b.err != nil {
		return nil, b.err
	}
	if b.closed || len(b.queue) == 0 {
		return nil, errors.New("no more PostgreSQL batch results")
	}
	q := &b.queue[0]
	b.queue = b.queue[1:]
	if !b.results.NextResult() {
		b.err = b.results.Close()
		if b.err == nil {
			b.err = errors.New("missing PostgreSQL batch result")
		}
		b.trace(q, pgconn.CommandTag{}, b.err)
		return nil, b.err
	}
	return q, nil
}

func (b *writeBatch) trace(q *pgx.QueuedQuery, tag pgconn.CommandTag, err error) {
	if b.tracer != nil {
		b.tracer.TraceBatchQuery(b.ctx, b.conn, pgx.TraceBatchQueryData{SQL: q.SQL, Args: q.Arguments, CommandTag: tag, Err: err})
	}
}

func (b *writeBatch) Exec() (pgconn.CommandTag, error) {
	q, err := b.next()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := b.results.ResultReader().Close()
	b.err = err
	b.trace(q, tag, err)
	return tag, err
}

func (b *writeBatch) Query() (pgx.Rows, error) {
	q, err := b.next()
	if err != nil {
		return failedBatchRows{err}, err
	}
	b.rows = &writeBatchRows{Rows: pgx.RowsFromResultReader(b.conn.TypeMap(), b.results.ResultReader()), batch: b, query: q}
	return b.rows, nil
}

func (b *writeBatch) QueryRow() pgx.Row {
	rows, err := b.Query()
	if err != nil {
		return failedRow{err}
	}
	return transactionRow{rows}
}

func (b *writeBatch) Close() error {
	if b.closed {
		return b.err
	}
	if b.rows != nil {
		b.rows.Close()
	}
	for b.err == nil && len(b.queue) > 0 {
		if callback := b.queue[0].Fn; callback != nil {
			if err := callback(b); err != nil {
				b.err = err
			}
		} else {
			_, b.err = b.Exec()
		}
	}
	if b.rows != nil {
		b.rows.Close()
	}
	if b.results != nil {
		if err := b.results.Close(); b.err == nil {
			b.err = err
		}
	}
	b.closed = true
	if b.tracer != nil {
		b.tracer.TraceBatchEnd(b.ctx, b.conn, pgx.TraceBatchEndData{Err: b.err})
	}
	return b.err
}

type writeBatchRows struct {
	pgx.Rows
	batch *writeBatch
	query *pgx.QueuedQuery
}

func (r *writeBatchRows) Close() {
	if r.batch == nil {
		return
	}
	r.Rows.Close()
	b := r.batch
	r.batch = nil
	b.rows = nil
	if err := r.Rows.Err(); err != nil && b.err == nil {
		b.err = err
	}
	b.trace(r.query, r.Rows.CommandTag(), r.Rows.Err())
}

func (r *writeBatchRows) Next() bool {
	if r.batch == nil {
		return false
	}
	if !r.Rows.Next() {
		r.Close()
		return false
	}
	return true
}

type failedBatchRows struct{ err error }

func (r failedBatchRows) Close() {}

func (r failedBatchRows) Next() bool { return false }

func (r failedBatchRows) Err() error { return r.err }

func (r failedBatchRows) Scan(...any) error { return r.err }

func (r failedBatchRows) Values() ([]any, error) { return nil, r.err }

func (r failedBatchRows) RawValues() [][]byte { return nil }

func (r failedBatchRows) Conn() *pgx.Conn { return nil }

func (r failedBatchRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

func (r failedBatchRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
