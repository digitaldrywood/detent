package platformstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (r *Repository) ExecEntry(ctx context.Context, workload corestore.Workload, query string, args ...any) (pgconn.CommandTag, error) {
	release, err := r.acquireEntry(ctx, workload)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer release()
	tag, err := r.pool.Exec(ctx, query, args...)
	return tag, mapError(ctx, err)
}
func (r *Repository) QueryEntry(ctx context.Context, workload corestore.Workload, query string, args ...any) (pgx.Rows, error) {
	release, err := r.acquireEntry(ctx, workload)
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		release()
		return nil, mapError(ctx, err)
	}
	return &entryRows{Rows: rows, release: release, classify: func(err error) error { return mapError(ctx, err) }}, nil
}
func (r *Repository) QueryRowEntry(ctx context.Context, workload corestore.Workload, query string, args ...any) pgx.Row {
	return entryRow{scan: func(dest ...any) error {
		release, err := r.acquireEntry(ctx, workload)
		if err != nil {
			return err
		}
		defer release()
		return mapError(ctx, r.pool.QueryRow(ctx, query, args...).Scan(dest...))
	}}
}

type entryRow struct{ scan func(...any) error }

func (r entryRow) Scan(dest ...any) error { return r.scan(dest...) }

type entryRows struct {
	pgx.Rows
	release  func()
	classify func(error) error
}

func (r *entryRows) Close() { r.Rows.Close(); r.release() }
func (r *entryRows) Next() bool {
	next := r.Rows.Next()
	if !next {
		r.Close()
	}
	return next
}
func (*entryRows) Conn() *pgx.Conn { return nil }

func (r *Repository) QueryHealth(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return &entryRows{Rows: rows, release: func() {}, classify: func(err error) error { return mapError(ctx, err) }}, nil
}
func (r *Repository) QueryHealthRow(ctx context.Context, query string, args ...any) pgx.Row {
	return entryRow{scan: func(dest ...any) error { return mapError(ctx, r.pool.QueryRow(ctx, query, args...).Scan(dest...)) }}
}

func (r *entryRows) Err() error             { return r.classify(r.Rows.Err()) }
func (r *entryRows) Scan(dest ...any) error { return r.classify(r.Rows.Scan(dest...)) }
