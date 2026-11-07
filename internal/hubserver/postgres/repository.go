package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
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
	leaseClock func() time.Time
	pool       *pgxpool.Pool
	runtime    *Runtime
	serving    chan struct{}
	background chan struct{}
}

var ErrPoolConfig = errors.New("PostgreSQL pool requires positive serving and background budgets plus one reserved health connection within its connection limit")
var ErrWorkload = errors.New("unknown PostgreSQL workload")

var _ corestore.Repository = (*Repository)(nil)

func NewRepository(ctx context.Context, pool *pgxpool.Pool, config PoolConfig) (*Repository, error) {
	if pool == nil || config.MaxConnections < 3 || config.ServingConnections < 1 || config.BackgroundConnections < 1 || config.ServingConnections >= config.MaxConnections || config.BackgroundConnections > config.MaxConnections-config.ServingConnections || config.BackgroundConnections > config.MaxConnections-2 || int(pool.Config().MaxConns) != config.MaxConnections {
		return nil, ErrPoolConfig
	}
	serving := min(config.ServingConnections, config.MaxConnections-config.BackgroundConnections-1)
	runtime, err := NewRuntime(ctx, pool, pool)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return &Repository{pool: pool, runtime: runtime, serving: make(chan struct{}, serving), background: make(chan struct{}, config.BackgroundConnections)}, nil
}

func (r *Repository) Close() error {
	r.pool.Close()
	return nil
}

func (r *Repository) Ping(ctx context.Context) error {
	return r.pool.Ping(ctx)
}

func (r *Repository) Stats() *pgxpool.Stat {
	return r.pool.Stat()
}

func (r *Repository) WorkloadCapacities() (serving, background int) {
	return cap(r.serving), cap(r.background)
}

func (r *Repository) Read(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(corestore.Reader) error) error {
	return r.transaction(ctx, organization, workload, true, func(session *session) error { return read(session) })
}

func (r *Repository) Write(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(corestore.Writer) error) error {
	return r.transaction(ctx, organization, workload, false, func(session *session) error { return write(session) })
}

func (r *Repository) transaction(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, readOnly bool, run func(*session) error) error {
	return r.transactionIsolation(ctx, organization, workload, readOnly, pgx.ReadCommitted, run)
}

type transactionTimingKey struct{}

type transactionTiming struct {
	span      func(string, time.Time)
	statement func(time.Time, string)
}

func WithTransactionTiming(ctx context.Context, observe func(string, time.Time), statements ...func(time.Time, string)) context.Context {
	timing := transactionTiming{span: observe}
	if len(statements) > 0 {
		timing.statement = statements[0]
	}
	return context.WithValue(ctx, transactionTimingKey{}, timing)
}

func transactionSpan(ctx context.Context, name string) func() {
	timing, ok := ctx.Value(transactionTimingKey{}).(transactionTiming)
	observe := timing.span
	if !ok || observe == nil {
		return func() {}
	}
	started := time.Now()
	return func() { observe(name, started) }
}

func (r *Repository) transactionIsolation(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, readOnly bool, isolation pgx.TxIsoLevel, run func(*session) error) (resultErr error) {
	if request, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		if request.repository != r || request.session.organization != organization || workload != corestore.Serving || !readOnly {
			return corestore.ErrTenantMismatch
		}
		return run(request.session)
	}
	finishAdmission := transactionSpan(ctx, "pg_admission")
	release, err := r.acquire(ctx, workload)
	finishAdmission()
	if err != nil {
		return err
	}
	defer release()
	finishBegin := transactionSpan(ctx, "pg_begin")
	tx, err := r.runtime.Begin(ctx, organization, pgtransport.Options{Isolation: isolation, ReadOnly: readOnly})
	finishBegin()
	if err != nil {
		return mapError(ctx, err)
	}
	defer func() {
		if cleanupErr := tx.Rollback(ctx); cleanupErr != nil {
			resultErr = errors.Join(resultErr, mapError(ctx, cleanupErr))
		}
	}()
	finishCallback := transactionSpan(ctx, "pg_callback")
	err = func() error {
		defer finishCallback()
		db := timedQueries{DBTX: tx}
		return run(&session{organization: organization, queries: pgquery.New(db), db: db, readOnly: readOnly, leaseClock: r.leaseClock})
	}()
	if err != nil {
		return mapError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finishCommit := transactionSpan(ctx, "pg_commit")
	started := time.Now()
	err = tx.Commit(ctx)
	observeStatement(ctx, started, "COMMIT")
	finishCommit()
	return mapError(ctx, err)
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

type projectAccessKey struct {
	principal string
	project   tracker.ProjectID
}

type session struct {
	leaseClock         func() time.Time
	searchDeadline     time.Time
	searchAdmitted     bool
	searchFallback     map[tracker.ProjectID]bool
	cacheMu            sync.Mutex
	projectGrants      map[projectAccessKey]accessstore.ProjectGrant
	tokenGrants        map[projectAccessKey]accessstore.TokenGrant
	projectRows        map[tracker.ProjectID][]pgquery.BoundProjectRecordRow
	credentials        map[string]accessstore.Credential
	members            map[string]accessstore.Member
	organizationRecord *accessstore.Organization
	db                 pgquery.DBTX
	readOnly           bool
	organization       tracker.OrganizationID
	queries            *pgquery.Queries
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
	if errors.Is(err, pgx.ErrNoRows) {
		return corestore.ErrRevisionConflict
	}
	return mapError(ctx, err)
}

func mapError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil && (errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
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
	if errors.Is(err, pgx.ErrNoRows) {
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
