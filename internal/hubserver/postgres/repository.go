package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PoolConfig struct {
	MaxConnections        int
	ServingConnections    int
	BackgroundConnections int
}

type Repository struct {
	pool       *sql.DB
	runtime    *Runtime
	serving    chan struct{}
	background chan struct{}
}

var ErrPoolConfig = errors.New("PostgreSQL pool requires positive serving and background budgets within its connection limit")
var ErrWorkload = errors.New("unknown PostgreSQL workload")

var _ corestore.Repository = (*Repository)(nil)

func NewRepository(ctx context.Context, pool *sql.DB, config PoolConfig) (*Repository, error) {
	if pool == nil || config.MaxConnections < 2 || config.ServingConnections < 1 || config.BackgroundConnections < 1 || config.ServingConnections >= config.MaxConnections || config.BackgroundConnections > config.MaxConnections-config.ServingConnections {
		return nil, ErrPoolConfig
	}
	pool.SetMaxOpenConns(config.MaxConnections)
	pool.SetMaxIdleConns(config.MaxConnections)
	runtime, err := NewRuntime(ctx, pool, pool)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return &Repository{pool: pool, runtime: runtime, serving: make(chan struct{}, config.ServingConnections), background: make(chan struct{}, config.BackgroundConnections)}, nil
}

func (r *Repository) Close() error {
	return r.pool.Close()
}

func (r *Repository) Read(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(corestore.Reader) error) error {
	return r.transaction(ctx, organization, workload, true, func(session *session) error { return read(session) })
}

func (r *Repository) Write(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(corestore.Writer) error) error {
	return r.transaction(ctx, organization, workload, false, func(session *session) error { return write(session) })
}

func (r *Repository) transaction(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, readOnly bool, run func(*session) error) error {
	release, err := r.acquire(ctx, workload)
	if err != nil {
		return err
	}
	defer release()
	tx, err := r.runtime.Begin(ctx, organization, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: readOnly})
	if err != nil {
		return mapError(ctx, err)
	}
	defer tx.Rollback()
	if err := run(&session{organization: organization, queries: pgquery.New(tx), db: tx, readOnly: readOnly}); err != nil {
		return mapError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mapError(ctx, tx.Commit())
}

func (r *Repository) acquire(ctx context.Context, workload corestore.Workload) (func(), error) {
	slots := r.serving
	switch workload {
	case corestore.Serving:
	case corestore.Background:
		slots = r.background
	default:
		return nil, ErrWorkload
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type session struct {
	db           pgquery.DBTX
	readOnly     bool
	organization tracker.OrganizationID
	queries      *pgquery.Queries
}

func (s *session) checkOrganization(organization tracker.OrganizationID) error {
	if organization != s.organization {
		return corestore.ErrTenantMismatch
	}
	return nil
}

func checkPage(limit int) error {
	if limit < 1 || limit > corestore.MaxPageSize {
		return corestore.ErrInvalidPage
	}
	return nil
}

func affected(ctx context.Context, rows int64, err error) error {
	if err != nil {
		return mapError(ctx, err)
	}
	if rows == 0 {
		return corestore.ErrNotFound
	}
	return nil
}

func updated(ctx context.Context, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return corestore.ErrRevisionConflict
	}
	return mapError(ctx, err)
}

func mapError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil && (errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return fmt.Errorf("PostgreSQL operation: %w", ctx.Err())
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		var domain error
		switch state.SQLState() {
		case "23505":
			domain = corestore.ErrUniqueViolation
		case "40001", "40P01":
			domain = corestore.ErrSerializationFailure
		case "57014":
			domain = ctx.Err()
			if domain == nil {
				domain = context.Canceled
			}
		}
		return &storageError{cause: err, domain: domain, state: state.SQLState()}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &storageError{cause: err, domain: corestore.ErrNotFound}
	}
	return err
}

type storageError struct {
	cause  error
	domain error
	state  string
}

func (e *storageError) Error() string {
	if e.domain != nil {
		return e.domain.Error()
	}
	return "PostgreSQL operation failed with SQLSTATE " + e.state
}

func (e *storageError) Unwrap() error {
	return e.cause
}

func (e *storageError) Is(target error) bool {
	return errors.Is(e.domain, target)
}
