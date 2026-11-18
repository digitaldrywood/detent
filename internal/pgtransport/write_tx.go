package pgtransport

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func WriteArgs(args []any) []any {
	options := make([]any, 0, len(args)+1)
	options = append(options, pgx.QueryExecModeExec)
	for i, arg := range args {
		switch arg.(type) {
		case pgx.QueryExecMode:
		case pgx.QueryResultFormats, pgx.QueryResultFormatsByOID, pgx.QueryRewriter:
			options = append(options, arg)
		default:
			return append(options, args[i:]...)
		}
	}
	return options
}

type writeTx struct {
	pgx.Tx
	closed bool
	parent *writeTx
}

func UncachedWrites(tx pgx.Tx) pgx.Tx { return &writeTx{Tx: tx} }

func (t *writeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &writeTx{Tx: tx, parent: t}, nil
}

func (t *writeTx) Commit(ctx context.Context) error {
	t.closed = true
	return t.Tx.Commit(ctx)
}

func (t *writeTx) Rollback(ctx context.Context) error {
	t.closed = true
	return t.Tx.Rollback(ctx)
}

func (t *writeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.Tx.Exec(ctx, sql, WriteArgs(args)...)
}

func (t *writeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.Tx.Query(ctx, sql, WriteArgs(args)...)
}

func (t *writeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.Tx.QueryRow(ctx, sql, WriteArgs(args)...)
}

func (t *writeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	for tx := t; tx != nil; tx = tx.parent {
		if tx.closed {
			return failedBatch{pgx.ErrTxClosed}
		}
	}
	return sendWriteBatch(ctx, t.Tx.Conn(), b)
}
