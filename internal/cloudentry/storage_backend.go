package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type StorageConfig struct {
	Backend      string
	PostgresDSN  string
	PostgresPool *sql.DB
	Pool         platformstore.PoolConfig
}

func (c StorageConfig) validate() error {
	switch c.Backend {
	case "", "sqlite":
		if c.PostgresDSN != "" || c.PostgresPool != nil || c.Pool != (platformstore.PoolConfig{}) {
			return errors.New("PostgreSQL entry settings require the postgres backend")
		}
	case "postgres":
		if (strings.TrimSpace(c.PostgresDSN) == "") == (c.PostgresPool == nil) || c.Pool.MaxConnections < 3 || c.Pool.ServingConnections < 1 || c.Pool.BackgroundConnections < 1 || c.Pool.ServingConnections >= c.Pool.MaxConnections || c.Pool.BackgroundConnections > c.Pool.MaxConnections-c.Pool.ServingConnections || c.Pool.BackgroundConnections > c.Pool.MaxConnections-2 {
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
	*sql.DB
	postgres *platformstore.Repository
	auth     bool
	clock    func() time.Time
}

func (d *entryDatabase) statement(query string) string {
	if d.postgres == nil {
		return query
	}
	if d.auth {
		query = strings.ReplaceAll(query, "platform_notifications", "auth_notifications")
	}
	var result strings.Builder
	position := 0
	quoted := false
	for _, ch := range query {
		if ch == '\'' {
			quoted = !quoted
		}
		if ch == '?' && !quoted {
			position++
			result.WriteByte('$')
			result.WriteString(strconv.Itoa(position))
		} else {
			result.WriteRune(ch)
		}
	}
	return result.String()
}

type entryTransaction struct {
	*sql.Tx
	owner    *entryDatabase
	postgres *platformstore.EntryTransaction
}

func (t *entryTransaction) cleanup(resultErr *error) {
	if err := t.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		*resultErr = errors.Join(*resultErr, err)
	}
}

func (d *entryDatabase) BeginTx(ctx context.Context, options *sql.TxOptions) (*entryTransaction, error) {
	if d.postgres == nil {
		tx, err := d.DB.BeginTx(ctx, options)
		return &entryTransaction{Tx: tx, owner: d}, err
	}
	var tx *platformstore.EntryTransaction
	var err error
	if health, _ := ctx.Value(entryHealthReadKey{}).(bool); health && options != nil && options.ReadOnly {
		tx, err = d.postgres.BeginHealthRead(ctx)
	} else {
		tx, err = d.postgres.BeginEntry(ctx, entryWorkload(ctx), options != nil && options.ReadOnly)
	}
	if err != nil {
		return nil, err
	}
	return &entryTransaction{Tx: tx.Tx, owner: d, postgres: tx}, nil
}

func (t *entryTransaction) Commit() error {
	if t.postgres != nil {
		return t.postgres.Commit()
	}
	return t.Tx.Commit()
}

func (t *entryTransaction) Rollback() error {
	if t.postgres != nil {
		return t.postgres.Rollback()
	}
	return t.Tx.Rollback()
}

func (t *entryTransaction) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := t.Tx.ExecContext(ctx, t.owner.statement(query), args...)
	if t.postgres != nil {
		err = t.postgres.Classify(ctx, err)
	}
	return result, err
}

func (t *entryTransaction) QueryContext(ctx context.Context, query string, args ...any) (entryRows, error) {
	rows, err := t.Tx.QueryContext(ctx, t.owner.statement(query), args...)
	if t.postgres != nil {
		err = t.postgres.Classify(ctx, err)
	}
	return rows, err
}

func (t *entryTransaction) QueryRowContext(ctx context.Context, query string, args ...any) entryRow {
	row := t.Tx.QueryRowContext(ctx, t.owner.statement(query), args...)
	if t.postgres == nil {
		return row
	}
	return deferredEntryRow{scan: func(dest ...any) error { return t.postgres.Classify(ctx, row.Scan(dest...)) }}
}

func (d *entryDatabase) ExecContext(ctx context.Context, query string, args ...any) (_ sql.Result, resultErr error) {
	if d.postgres == nil {
		return d.DB.ExecContext(ctx, query, args...)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.cleanup(&resultErr)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

type transactionRows struct {
	*sql.Rows
	close      func() error
	cleanupErr error
}

func (r *transactionRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.cleanupErr = r.close()
	return false
}

func (r *transactionRows) Close() error { return r.close() }
func (r *transactionRows) Err() error   { return errors.Join(r.Rows.Err(), r.cleanupErr) }

func (d *entryDatabase) QueryContext(ctx context.Context, query string, args ...any) (entryRows, error) {
	if d.postgres == nil {
		return d.DB.QueryContext(ctx, query, args...)
	}
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	rows, err := tx.Tx.QueryContext(ctx, d.statement(query), args...)
	err = tx.postgres.Classify(ctx, err)
	if err != nil {
		return nil, errors.Join(err, tx.Rollback())
	}
	closeRows := sync.OnceValue(func() error { return errors.Join(rows.Close(), tx.Rollback()) })
	return &transactionRows{Rows: rows, close: closeRows}, nil
}

type deferredEntryRow struct{ scan func(...any) error }

func (r deferredEntryRow) Scan(dest ...any) error { return r.scan(dest...) }

func (d *entryDatabase) QueryRowContext(ctx context.Context, query string, args ...any) entryRow {
	if d.postgres == nil {
		return d.DB.QueryRowContext(ctx, query, args...)
	}
	return deferredEntryRow{scan: func(dest ...any) (resultErr error) {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: strings.HasPrefix(strings.TrimSpace(strings.ToUpper(query)), "SELECT")})
		if err != nil {
			return err
		}
		defer tx.cleanup(&resultErr)
		if err := tx.QueryRowContext(ctx, query, args...).Scan(dest...); err != nil {
			return err
		}
		return tx.Commit()
	}}
}

func openEntryStores(ctx context.Context, cfg Config, keys *hubsecrets.Keyring) (*Registry, *store, error) {
	if cfg.Storage.Backend != "postgres" {
		r, err := OpenRegistry(ctx, cfg.StateDir+"/registry.db")
		if err != nil {
			return nil, nil, err
		}
		a, err := openStore(ctx, cfg.StateDir+"/auth.db", authApplicationID, "auth")
		if err != nil {
			return nil, nil, errors.Join(err, r.Close())
		}
		return r, a, nil
	}
	pool := cfg.Storage.PostgresPool
	var err error
	if pool == nil {
		pool, err = sql.Open("postgres", cfg.Storage.PostgresDSN)
	}
	if err != nil {
		return nil, nil, errors.New("open PostgreSQL entry pool")
	}
	r, err := platformstore.New(ctx, pool, platformstore.Config{Pool: cfg.Storage.Pool, SigningKey: cfg.SigningKey, Keys: keys, Now: cfg.now})
	if err != nil {
		return nil, nil, errors.Join(err, pool.Close())
	}
	closePool := sync.OnceValue(r.Close)
	registryDB := &entryDatabase{postgres: r, clock: cfg.now}
	authDB := &entryDatabase{postgres: r, auth: true, clock: cfg.now}
	return &Registry{store: &store{db: registryDB, reader: registryDB, close: closePool}, now: cfg.now}, &store{db: authDB, reader: authDB, close: closePool}, nil
}

func (d *entryDatabase) choose(sqlite, postgres string) string {
	if d.postgres != nil {
		return postgres
	}
	return sqlite
}

func (d *entryDatabase) PingContext(ctx context.Context) error {
	if d.postgres == nil {
		return d.DB.PingContext(ctx)
	}
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
