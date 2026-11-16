package postgres

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PoolConfig struct {
	MaxConnections        int
	ServingConnections    int
	BackgroundConnections int
	SearchBudget          time.Duration
}

const defaultBoardSearchBudget = 750 * time.Millisecond

type Repository struct {
	leaseClock     func() time.Time
	searchBudget   time.Duration
	pool           *pgxpool.Pool
	runtime        *Runtime
	serving        chan struct{}
	background     chan struct{}
	boardCommitMu  sync.RWMutex
	boardCommitted func()
}

var ErrPoolConfig = errors.New("PostgreSQL pool requires positive serving and background budgets plus one reserved health connection within its connection limit")
var ErrWorkload = errors.New("unknown PostgreSQL workload")

var _ corestore.Repository = (*Repository)(nil)

func NewRepository(ctx context.Context, pool *pgxpool.Pool, config PoolConfig, leaseClock ...func() time.Time) (*Repository, error) {
	if len(leaseClock) > 1 {
		return nil, ErrPoolConfig
	}
	if pool == nil || config.MaxConnections < 3 || config.ServingConnections < 1 || config.BackgroundConnections < 1 || config.ServingConnections >= config.MaxConnections || config.BackgroundConnections > config.MaxConnections-config.ServingConnections || config.BackgroundConnections > config.MaxConnections-2 || int(pool.Config().MaxConns) != config.MaxConnections {
		return nil, ErrPoolConfig
	}
	serving := min(config.ServingConnections, config.MaxConnections-config.BackgroundConnections-1)
	runtime, err := NewRuntime(ctx, pool, pool)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	var clock func() time.Time
	if len(leaseClock) == 1 {
		clock = leaseClock[0]
	}
	searchBudget := config.SearchBudget
	if searchBudget <= 0 {
		searchBudget = defaultBoardSearchBudget
	}
	return &Repository{leaseClock: clock, searchBudget: searchBudget, pool: pool, runtime: runtime, serving: make(chan struct{}, serving), background: make(chan struct{}, config.BackgroundConnections)}, nil
}

func (r *Repository) SetBoardCommitNotifier(notify func()) {
	r.boardCommitMu.Lock()
	r.boardCommitted = notify
	r.boardCommitMu.Unlock()
}

func (r *Repository) notifyBoardCommit() {
	r.boardCommitMu.RLock()
	notify := r.boardCommitted
	r.boardCommitMu.RUnlock()
	if notify != nil {
		notify()
	}
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

const internalWriteAttempts = 4

func (r *Repository) internalWrite(ctx context.Context, organization tracker.OrganizationID, run func(*session) error) error {
	for attempt := 1; ; attempt++ {
		err := r.transaction(ctx, organization, corestore.Background, false, run)
		if attempt == internalWriteAttempts || !pgcore.RetryableConflict(err) {
			return err
		}
		delay := time.Duration(10<<(attempt-1)) * time.Millisecond
		delay = delay/2 + rand.N(delay/2+1)
		wait, cancel := context.WithTimeout(ctx, delay)
		<-wait.Done()
		cancel()
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
	}
}

func WithTransactionTiming(ctx context.Context, observe func(string, time.Time), statements ...func(time.Time, string)) context.Context {
	return pgcore.WithTransactionTiming(ctx, observe, statements...)
}

func transactionSpan(ctx context.Context, name string) func() {
	return pgcore.TransactionSpan(ctx, name)
}

func (r *Repository) transactionIsolation(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, readOnly bool, isolation pgx.TxIsoLevel, run func(*session) error) error {
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
	return r.begin(ctx, organization, readOnly, isolation, run)
}

func (r *Repository) leafTransaction(ctx context.Context, organization tracker.OrganizationID, readOnly bool, run func(*session) error) error {
	if request, ok := ctx.Value(readRequestKey{}).(*readRequest); ok && readOnly {
		if request.repository != r || request.session.organization != organization {
			return corestore.ErrTenantMismatch
		}
		return run(request.session)
	}
	return r.begin(ctx, organization, readOnly, pgx.ReadCommitted, run)
}

func (r *Repository) begin(ctx context.Context, organization tracker.OrganizationID, readOnly bool, isolation pgx.TxIsoLevel, run func(*session) error) (resultErr error) {
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
	db := timedQueries{DBTX: tx}
	owner := &session{organization: organization, queries: pgquery.New(db), db: db, readOnly: readOnly, leaseClock: r.leaseClock, searchBudget: r.searchBudget, flushesBoardDeltas: !readOnly}
	err = func() error {
		defer finishCallback()
		return run(owner)
	}()
	if err != nil {
		return mapError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	hasDelivery := len(owner.boardDeltas) != 0
	if !readOnly {
		if err := owner.flushBoardDeltas(ctx); err != nil {
			return err
		}
	}
	finishCommit := transactionSpan(ctx, "pg_commit")
	started := time.Now()
	err = tx.Commit(ctx)
	observeStatement(ctx, started, "COMMIT")
	finishCommit()
	if err == nil && hasDelivery {
		r.notifyBoardCommit()
	}
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
	role      string
}

type session struct {
	boardDeltas             []boardAppend
	flushesBoardDeltas      bool
	archiveAccountingLocked bool
	archiveBillingPresent   bool
	archiveCounterLocked    bool
	insertedArchive         *artifactstore.ArchiveObject
	collaborationLocked     bool
	collaborationUsed       int64

	leaseClock         func() time.Time
	searchBudget       time.Duration
	searchDeadline     time.Time
	searchAdmitted     bool
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

func (s *session) transactionHandle() pgcore.Transaction {
	return pgcore.Transaction{Organization: s.organization, DB: s.db, Queries: s.queries}
}

func (s *session) checkOrganization(organization tracker.OrganizationID) error {
	return s.transactionHandle().CheckOrganization(organization)
}

func checkPage(limit int) error {
	if limit < 1 || limit > corestore.MaxPageSize {
		return corestore.ErrInvalidPage
	}
	return nil
}

func affected(ctx context.Context, rows int64, err error) error {
	return pgcore.Affected(ctx, rows, err)
}

func updated(ctx context.Context, err error) error {
	return pgcore.Updated(ctx, err)
}

func mapError(ctx context.Context, err error) error {
	return pgcore.MapError(ctx, err)
}
