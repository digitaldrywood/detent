package platformstore

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type EntryTransaction struct {
	*sql.Tx
	release func()
}

func (r *Repository) BeginEntry(ctx context.Context, workload corestore.Workload, readOnly bool) (*EntryTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slots := r.serving
	switch workload {
	case corestore.Serving:
	case corestore.Background:
		slots = r.background
	default:
		return nil, ErrInvalid
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := sync.OnceFunc(func() { <-slots })
	tx, err := r.pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: readOnly})
	if err != nil {
		release()
		return nil, mapError(ctx, err)
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = platform, pg_catalog"); err != nil {
		tx.Rollback()
		release()
		return nil, mapError(ctx, err)
	}
	return &EntryTransaction{Tx: tx, release: release}, nil
}

func (t *EntryTransaction) Commit() error {
	defer t.release()
	return t.Tx.Commit()
}

func (t *EntryTransaction) Rollback() error {
	defer t.release()
	return t.Tx.Rollback()
}

func (t *EntryTransaction) Classify(ctx context.Context, err error) error { return mapError(ctx, err) }

func (t *EntryTransaction) cleanup(resultErr *error) {
	if err := t.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		*resultErr = errors.Join(*resultErr, err)
	}
}
