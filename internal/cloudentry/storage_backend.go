package cloudentry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type StorageConfig struct {
	Backend      string
	PostgresDSN  string
	PostgresPool *pgxpool.Pool
	Pool         platformstore.PoolConfig
}

func (c StorageConfig) validate() error {
	switch c.Backend {
	case "postgres":
		if (strings.TrimSpace(c.PostgresDSN) == "") == (c.PostgresPool == nil) || c.Pool.MaxConnections < 3 || c.Pool.ServingConnections < 1 || c.Pool.BackgroundConnections < 1 || c.Pool.ServingConnections >= c.Pool.MaxConnections || c.Pool.BackgroundConnections >= c.Pool.MaxConnections-c.Pool.ServingConnections || c.Pool.BackgroundConnections > c.Pool.MaxConnections-2 {
			return errors.New("PostgreSQL entry storage requires a DSN and positive workload budgets with room for a reserved health connection")
		}
	default:
		return errors.New("unknown entry storage backend")
	}
	return nil
}

type entryRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

type entryRow interface{ Scan(...any) error }

func closeEntryRows(rows entryRows, resultErr *error) {
	if err := rows.Close(); err != nil {
		*resultErr = errors.Join(*resultErr, err)
	}
}

type entryDatabase struct {
	postgres *platformstore.Repository
	auth     bool
	clock    func() time.Time
}

type entryTransaction struct {
	pgx.Tx
	owner    *entryDatabase
	postgres *platformstore.EntryTransaction
}

func (t *entryTransaction) cleanup(ctx context.Context, resultErr *error) {
	if err := t.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		*resultErr = errors.Join(*resultErr, err)
	}
}

func (d *entryDatabase) BeginTx(ctx context.Context, options *pgx.TxOptions) (*entryTransaction, error) {
	var tx *platformstore.EntryTransaction
	var err error
	if health, ok := ctx.Value(entryHealthReadKey{}).(bool); ok && health && options != nil && options.AccessMode == pgx.ReadOnly {
		tx, err = d.postgres.BeginHealthRead(ctx)
	} else {
		tx, err = d.postgres.BeginEntry(ctx, entryWorkload(ctx), options != nil && options.AccessMode == pgx.ReadOnly)
	}
	if err != nil {
		return nil, err
	}
	return &entryTransaction{Tx: tx.Tx, owner: d, postgres: tx}, nil
}

func (t *entryTransaction) Commit(ctx context.Context) error {
	return t.postgres.Commit(ctx)
}

func (t *entryTransaction) Rollback(ctx context.Context) error {
	return t.postgres.Rollback(ctx)
}

func (t *entryTransaction) ExecContext(ctx context.Context, query string, args ...any) (entryResult, error) {
	result, err := t.Exec(ctx, query, args...)
	err = t.postgres.Classify(ctx, err)
	return entryCommandTag{result}, err
}

func (t *entryTransaction) QueryContext(ctx context.Context, query string, args ...any) (entryRows, error) {
	rows, err := t.Query(ctx, query, args...)
	err = t.postgres.Classify(ctx, err)
	if err != nil {
		return nil, err
	}
	return nativeEntryRows{Rows: rows, classify: func(err error) error { return t.postgres.Classify(ctx, err) }}, nil
}

func (t *entryTransaction) QueryRowContext(ctx context.Context, query string, args ...any) entryRow {
	rows, err := t.QueryContext(ctx, query, args...)
	return entryScanRow(rows, err)
}

func (d *entryDatabase) ExecContext(ctx context.Context, query string, args ...any) (entryResult, error) {
	tag, err := d.postgres.ExecEntry(ctx, entryWorkload(ctx), query, args...)
	return entryCommandTag{tag}, err
}
func (d *entryDatabase) QueryContext(ctx context.Context, query string, args ...any) (entryRows, error) {
	var rows pgx.Rows
	var err error
	if health, ok := ctx.Value(entryHealthReadKey{}).(bool); ok && health {
		rows, err = d.postgres.QueryHealth(ctx, query, args...)
	} else {
		rows, err = d.postgres.QueryEntry(ctx, entryWorkload(ctx), query, args...)
	}
	if err != nil {
		return nil, err
	}
	return nativeEntryRows{Rows: rows}, nil
}

type deferredEntryRow struct{ scan func(...any) error }

func (r deferredEntryRow) Scan(dest ...any) error { return r.scan(dest...) }
func (d *entryDatabase) QueryRowContext(ctx context.Context, query string, args ...any) entryRow {
	rows, err := d.QueryContext(ctx, query, args...)
	return entryScanRow(rows, err)
}

type nativeEntryRows struct {
	pgx.Rows
	classify func(error) error
}

func (r nativeEntryRows) Close() error { r.Rows.Close(); return nil }

type entryResult interface{ RowsAffected() (int64, error) }
type entryCommandTag struct{ pgconn.CommandTag }

func (r entryCommandTag) RowsAffected() (int64, error) { return r.CommandTag.RowsAffected(), nil }

func openEntryStores(ctx context.Context, cfg Config, keys *hubsecrets.Keyring) (*Registry, *store, error) {
	pool := cfg.Storage.PostgresPool
	var err error
	if pool == nil {
		config, parseErr := pgtransport.ParsePoolConfig(cfg.Storage.PostgresDSN)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("configure PostgreSQL entry pool: %w", parseErr)
		}
		pgtransport.Configure(config)
		config.MaxConns = int32(cfg.Storage.Pool.MaxConnections)
		pool, err = pgxpool.NewWithConfig(ctx, config)
	}
	if err != nil {
		return nil, nil, errors.New("open PostgreSQL entry pool")
	}
	r, err := platformstore.New(ctx, pool, platformstore.Config{Pool: cfg.Storage.Pool, SigningKey: cfg.SigningKey, Keys: keys, Now: cfg.now})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	closePool := sync.OnceValue(r.Close)
	registryDB := &entryDatabase{postgres: r, clock: cfg.now}
	authDB := &entryDatabase{postgres: r, auth: true, clock: cfg.now}
	return &Registry{store: &store{db: registryDB, reader: registryDB, close: closePool}, now: cfg.now}, &store{db: authDB, reader: authDB, close: closePool}, nil
}

func (d *entryDatabase) PingContext(ctx context.Context) error {
	return d.postgres.Ping(ctx)
}

type entryWorkloadKey struct{}
type entryHealthReadKey struct{}

func entryWorkload(ctx context.Context) corestore.Workload {
	if workload, ok := ctx.Value(entryWorkloadKey{}).(corestore.Workload); ok {
		return workload
	}
	return corestore.Serving
}
func entryBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, entryWorkloadKey{}, corestore.Background)
}

func (d *entryDatabase) entryNow() time.Time    { return d.clock() }
func (t *entryTransaction) entryNow() time.Time { return t.owner.entryNow() }

func entryScanRow(rows entryRows, queryErr error) entryRow {
	return deferredEntryRow{scan: func(dest ...any) (resultErr error) {
		if queryErr != nil {
			return queryErr
		}
		defer closeEntryRows(rows, &resultErr)
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return err
			}
			return pgx.ErrNoRows
		}
		return rows.Scan(dest...)
	}}
}

func (r nativeEntryRows) Scan(dest ...any) error {
	fields := r.FieldDescriptions()
	targets := append([]any(nil), dest...)
	times := make(map[int]*time.Time)
	for index, target := range dest {
		if index >= len(fields) {
			break
		}
		if fields[index].DataTypeOID != pgtype.TimestamptzOID {
			continue
		}
		if _, ok := target.(*string); ok {
			value := new(time.Time)
			times[index] = value
			targets[index] = value
		}
	}
	if err := r.Rows.Scan(targets...); err != nil {
		if r.classify != nil {
			return r.classify(err)
		}
		return err
	}
	for index, value := range times {
		target, ok := dest[index].(*string)
		if !ok {
			return errors.New("invalid PostgreSQL timestamp destination")
		}
		*target = value.UTC().Format(time.RFC3339Nano)
	}
	return nil
}

func (r nativeEntryRows) Err() error {
	if r.classify != nil {
		return r.classify(r.Rows.Err())
	}
	return r.Rows.Err()
}
