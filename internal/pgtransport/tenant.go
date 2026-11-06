package pgtransport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrTenantRequired = errors.New("authenticated organization is required")
var ErrUnbound = errors.New("protected PostgreSQL query requires a bound tenant")

type Scope struct{ Organization tracker.OrganizationID }

type Reader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Executor interface {
	Reader
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

type Pools struct{ Reader, Writer *pgxpool.Pool }

type Transaction struct {
	conn  *pgxpool.Conn
	scope Scope
	bound bool
	done  bool
}

func Configure(config *pgxpool.Config) {
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.ConnConfig.StatementCacheCapacity = 0
	config.ConnConfig.DescriptionCacheCapacity = 0
}

func (p Pools) Begin(ctx context.Context, scope Scope, options pgx.TxOptions) (*Transaction, error) {
	if strings.TrimSpace(string(scope.Organization)) == "" {
		return nil, ErrTenantRequired
	}
	pool := p.Writer
	if options.AccessMode == pgx.ReadOnly {
		pool = p.Reader
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire PostgreSQL tenant connection: %w", err)
	}
	tx := &Transaction{conn: conn, scope: scope}
	begin := "BEGIN"
	switch options.IsoLevel {
	case "", pgx.ReadCommitted:
	case pgx.RepeatableRead:
		begin += " ISOLATION LEVEL REPEATABLE READ"
	case pgx.Serializable:
		begin += " ISOLATION LEVEL SERIALIZABLE"
	default:
		conn.Release()
		return nil, errors.New("unsupported PostgreSQL isolation")
	}
	if options.AccessMode == pgx.ReadOnly {
		begin += " READ ONLY"
	}
	batch := &pgx.Batch{}
	batch.Queue(begin)
	batch.Queue(`SELECT set_config('search_path', 'public, pg_catalog', true), set_config('detent.organization_id', $1, true)`, string(scope.Organization))
	results := conn.SendBatch(ctx, batch)
	_, beginErr := results.Exec()
	_, bindErr := results.Exec()
	closeErr := results.Close()
	if err := errors.Join(beginErr, bindErr, closeErr); err != nil {
		return nil, errors.Join(fmt.Errorf("bind PostgreSQL tenant transaction: %w", err), tx.Rollback(ctx))
	}
	tx.bound = true
	return tx, nil
}

func (t *Transaction) Scope() Scope { return t.scope }

func (t *Transaction) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if !t.bound {
		return pgconn.CommandTag{}, ErrUnbound
	}
	return t.conn.Exec(ctx, query, args...)
}

func (t *Transaction) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if !t.bound {
		return nil, ErrUnbound
	}
	return t.conn.Query(ctx, query, args...)
}

func (t *Transaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if !t.bound {
		return failedRow{ErrUnbound}
	}
	return t.conn.QueryRow(ctx, query, args...)
}

func (t *Transaction) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if !t.bound {
		return failedBatch{ErrUnbound}
	}
	return t.conn.SendBatch(ctx, batch)
}

func (t *Transaction) Commit(ctx context.Context) error {
	if !t.bound {
		return ErrUnbound
	}
	t.bound = false
	t.done = true
	defer t.conn.Release()
	_, err := t.conn.Exec(ctx, "COMMIT")
	return err
}

func (t *Transaction) Rollback(ctx context.Context) error {
	if t.done || t.conn == nil {
		return nil
	}
	t.done = true
	t.bound = false
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	defer t.conn.Release()
	_, err := t.conn.Exec(cleanup, "ROLLBACK")
	return err
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

type failedBatch struct{ err error }

func (b failedBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b failedBatch) Query() (pgx.Rows, error)         { return nil, b.err }
func (b failedBatch) QueryRow() pgx.Row                { return failedRow{b.err} }
func (b failedBatch) Close() error                     { return b.err }
