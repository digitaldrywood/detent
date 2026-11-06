package pgtransport

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type transactionRows struct {
	pgx.Rows
	tx *Transaction
}

func (r *transactionRows) Close() {
	r.Rows.Close()
	if r.tx.rows == r {
		r.tx.rows = nil
	}
}
func (r *transactionRows) Next() bool {
	next := r.Rows.Next()
	if !next {
		r.Close()
	}
	return next
}

type transactionRow struct{ rows pgx.Rows }

func (r transactionRow) Scan(dest ...any) error {
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

type transactionBatch struct {
	pgx.BatchResults
	tx *Transaction
}

func (b *transactionBatch) Close() error {
	err := b.BatchResults.Close()
	if b.tx.batch == b {
		b.tx.batch = nil
	}
	return err
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

type failedBatch struct{ err error }

func (b failedBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b failedBatch) Query() (pgx.Rows, error)         { return nil, b.err }
func (b failedBatch) QueryRow() pgx.Row                { return failedRow(b) }
func (b failedBatch) Close() error                     { return b.err }

type hiddenRows struct{ pgx.Rows }

func (hiddenRows) Conn() *pgx.Conn       { return nil }
func (*transactionRows) Conn() *pgx.Conn { return nil }
func (b *transactionBatch) Query() (pgx.Rows, error) {
	rows, err := b.BatchResults.Query()
	if rows == nil {
		return nil, err
	}
	return hiddenRows{rows}, err
}

type callbackBatch struct{ pgx.BatchResults }

func (b callbackBatch) Query() (pgx.Rows, error) {
	rows, err := b.BatchResults.Query()
	if rows == nil {
		return nil, err
	}
	return hiddenRows{rows}, err
}
func guardedBatch(batch *pgx.Batch) *pgx.Batch {
	safe := &pgx.Batch{}
	for _, queued := range batch.QueuedQueries {
		copy := *queued
		if callback := copy.Fn; callback != nil {
			copy.Fn = func(results pgx.BatchResults) error { return callback(callbackBatch{results}) }
		}
		safe.QueuedQueries = append(safe.QueuedQueries, &copy)
	}
	return safe
}
