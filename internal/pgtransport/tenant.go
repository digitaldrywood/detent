package pgtransport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrTenantRequired = errors.New("authenticated organization is required")
var ErrUnbound = errors.New("protected PostgreSQL query requires a bound tenant")
var ErrOpenResults = errors.New("PostgreSQL transaction has open results")

type Scope struct{ Organization tracker.OrganizationID }
type Options struct {
	Isolation pgx.TxIsoLevel
	ReadOnly  bool
}

type Reader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type Executor interface {
	Reader
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

type Transaction struct {
	conn        *pgxpool.Conn
	initialRead bool
	beginSQL    string
	scope       Scope
	bound, done bool
	rows        *transactionRows
	batch       *transactionBatch
}

func (p *Pools) Begin(ctx context.Context, scope Scope, options Options) (*Transaction, error) {
	tx, results, err := p.BeginBatch(ctx, scope, options, nil)
	if err != nil {
		return nil, err
	}
	if err := results.Close(); err != nil {
		return nil, errors.Join(err, tx.Rollback(ctx))
	}
	return tx, nil
}

func (p *Pools) BeginBatch(ctx context.Context, scope Scope, options Options, work *pgx.Batch) (*Transaction, pgx.BatchResults, error) {
	if strings.TrimSpace(string(scope.Organization)) == "" {
		return nil, nil, ErrTenantRequired
	}
	isolation := options.Isolation
	if isolation == "" {
		isolation = pgx.ReadCommitted
	}
	switch isolation {
	case pgx.ReadCommitted, pgx.RepeatableRead, pgx.Serializable:
	default:
		return nil, nil, errors.New("unsupported PostgreSQL isolation")
	}
	pool := p.writer
	mode := " READ WRITE"
	if options.ReadOnly {
		pool = p.reader
		mode = " READ ONLY"
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire PostgreSQL tenant connection: %w", err)
	}
	tx := &Transaction{conn: conn, scope: scope, beginSQL: "BEGIN ISOLATION LEVEL " + string(isolation) + mode, initialRead: options.ReadOnly && work == nil}
	results, err := tx.bind(ctx, work)
	if err != nil {
		return nil, nil, errors.Join(err, tx.Rollback(ctx))
	}
	return tx, results, nil
}

func (tx *Transaction) bind(ctx context.Context, work *pgx.Batch) (pgx.BatchResults, error) {
	batch := new(pgx.Batch)
	batch.Queue(tx.beginSQL)
	batch.Queue(`SELECT set_config('search_path', 'public, pg_catalog', true), set_config('detent.organization_id', $1, true), set_config('jit', 'off', true)`, string(tx.scope.Organization))
	if work != nil {
		batch.QueuedQueries = append(batch.QueuedQueries, guardedBatch(work).QueuedQueries...)
	}
	results := tx.conn.SendBatch(ctx, batch)
	_, beginErr := results.Exec()
	_, bindErr := results.Exec()
	if err := errors.Join(beginErr, bindErr); err != nil {
		closeErr := results.Close()
		return nil, errors.Join(fmt.Errorf("bind PostgreSQL tenant transaction: %w", err), closeErr)
	}
	tx.bound = true
	tracked := &transactionBatch{BatchResults: results, tx: tx}
	tx.batch = tracked
	return tracked, nil
}

func (t *Transaction) Scope() Scope { return t.scope }
func (t *Transaction) ready() error {
	if !t.bound {
		return ErrUnbound
	}
	if t.rows != nil || t.batch != nil {
		return ErrOpenResults
	}
	return nil
}
func (t *Transaction) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if err := t.ready(); err != nil {
		return pgconn.CommandTag{}, err
	}
	t.initialRead = false
	return t.conn.Exec(ctx, query, args...)
}
func (t *Transaction) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if err := t.ready(); err != nil {
		return nil, err
	}
	first := t.initialRead
	t.initialRead = false
	rows, err := t.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	tracked := &transactionRows{Rows: rows, tx: t}
	if first {
		tracked.retry = func() (pgx.Rows, error) {
			t.bound = false
			if err := t.rollbackConnection(ctx); err != nil {
				return nil, err
			}
			results, err := t.bind(ctx, nil)
			if err != nil {
				return nil, err
			}
			if err := results.Close(); err != nil {
				return nil, err
			}
			rows, err := t.conn.Query(ctx, query, args...)
			if err == nil {
				t.rows = tracked
			}
			return rows, err
		}
	}
	t.rows = tracked
	return tracked, nil
}
func (t *Transaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	rows, err := t.Query(ctx, query, args...)
	if err != nil {
		return failedRow{err}
	}
	return transactionRow{rows}
}
func (t *Transaction) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if err := t.ready(); err != nil {
		return failedBatch{err}
	}
	t.initialRead = false
	tracked := &transactionBatch{BatchResults: t.conn.SendBatch(ctx, guardedBatch(batch)), tx: t}
	t.batch = tracked
	return tracked
}
func (t *Transaction) Commit(ctx context.Context) error {
	if err := t.ready(); err != nil {
		if errors.Is(err, ErrOpenResults) {
			return errors.Join(err, t.Rollback(ctx))
		}
		return err
	}
	t.bound = false
	t.done = true
	defer t.conn.Release()
	tag, err := t.conn.Exec(ctx, "COMMIT")
	if err == nil && tag.String() == "ROLLBACK" {
		return pgx.ErrTxCommitRollback
	}
	return err
}
func (t *Transaction) Rollback(ctx context.Context) error {
	if t.done || t.conn == nil {
		return nil
	}
	t.done = true
	t.bound = false
	defer t.conn.Release()
	return t.rollbackConnection(ctx)
}

func (t *Transaction) rollbackConnection(ctx context.Context) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if t.rows != nil || t.batch != nil {
		return t.conn.Conn().Close(cleanup)
	}
	if t.conn.Conn().IsClosed() {
		return nil
	}
	_, err := t.conn.Exec(cleanup, "ROLLBACK")
	if err != nil && t.conn.Conn().IsClosed() {
		return nil
	}
	return err
}
