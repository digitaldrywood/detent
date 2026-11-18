package platformstore

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type EntryTransaction struct {
	pgx.Tx
	release func()
}

func (r *Repository) BeginEntry(ctx context.Context, workload corestore.Workload, readOnly bool) (*EntryTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, err := r.acquireEntry(ctx, workload)
	if err != nil {
		return nil, err
	}
	return r.beginEntry(ctx, readOnly, release)
}

func (r *Repository) BeginHealthRead(ctx context.Context) (*EntryTransaction, error) {
	return r.beginEntry(ctx, true, func() {})
}

func (r *Repository) beginEntry(ctx context.Context, readOnly bool, release func()) (*EntryTransaction, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: entryAccessMode(readOnly)})
	if err != nil {
		release()
		return nil, mapError(ctx, err)
	}
	return &EntryTransaction{Tx: tx, release: release}, nil
}

func (t *EntryTransaction) Commit(ctx context.Context) error {
	defer t.release()
	return t.Tx.Commit(ctx)
}

func (t *EntryTransaction) Rollback(ctx context.Context) error {
	defer t.release()
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return t.Tx.Rollback(cleanup)
}

func (t *EntryTransaction) Classify(ctx context.Context, err error) error { return mapError(ctx, err) }

func (t *EntryTransaction) cleanup(ctx context.Context, resultErr *error) {
	if err := t.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		*resultErr = errors.Join(*resultErr, err)
	}
}

func entryAccessMode(readOnly bool) pgx.TxAccessMode {
	if readOnly {
		return pgx.ReadOnly
	}
	return pgx.ReadWrite
}

func (r *Repository) acquireEntry(ctx context.Context, workload corestore.Workload) (func(), error) {
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

	return release, nil
}
