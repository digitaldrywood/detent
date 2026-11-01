package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"slices"
	"strings"

	"github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

type Config struct {
	Connection pq.Config
	Pool       PoolConfig
}

var ErrConnectionConfig = errors.New("PostgreSQL requires an explicit host, port, database, user and TLS mode without session overrides or credential discovery")
var ErrMigrationRole = errors.New("PostgreSQL migration role must have separate unprivileged authority")
var ErrSchema = errors.New("PostgreSQL schema does not match this Detent version")

func Open(ctx context.Context, cfg Config, migrations fs.FS) (*Repository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pool, err := openPool(cfg.Connection, "")
	if err != nil {
		return nil, err
	}
	repository, err := NewRepository(ctx, pool, cfg.Pool)
	if err == nil {
		err = validateSchema(ctx, pool, migrations)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, errors.Join(mapError(ctx, err), pool.Close())
	}
	return repository, nil
}

func openPool(connection pq.Config, role string) (*sql.DB, error) {
	if strings.TrimSpace(connection.Host) == "" || connection.Port == 0 || strings.TrimSpace(connection.Database) == "" || strings.TrimSpace(connection.User) == "" || connection.SSLMode == "" || connection.Passfile != "" || connection.Service != "" || connection.ServiceFile != "" || connection.Options != "" || len(connection.Runtime) != 0 || len(connection.Multi) != 0 {
		return nil, ErrConnectionConfig
	}
	connection = connection.Clone()
	connection.Runtime = map[string]string{"search_path": "public, pg_catalog"}
	if role != "" {
		connection.Runtime["role"] = role
	}
	connector, err := pq.NewConnectorConfig(connection)
	if err != nil {
		return nil, ErrConnectionConfig
	}
	return sql.OpenDB(connector), nil
}

func migrationProvider(pool *sql.DB, migrations fs.FS) (*goose.Provider, error) {
	if migrations == nil {
		return nil, ErrSchema
	}
	return goose.NewProvider(goose.DialectPostgres, pool, migrations,
		goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
}

func validateSchema(ctx context.Context, pool *sql.DB, migrations fs.FS) error {
	provider, err := migrationProvider(pool, migrations)
	if err != nil {
		return err
	}
	sources := provider.ListSources()
	if len(sources) == 0 {
		return ErrSchema
	}
	var owned bool
	if err := pool.QueryRowContext(ctx, `SELECT pg_get_userbyid(relowner) = 'detent_migration'
		FROM pg_class WHERE oid = 'public.goose_db_version'::regclass`).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrSchema
	}
	rows, err := pool.QueryContext(ctx, `SELECT version_id FROM public.goose_db_version WHERE is_applied AND version_id > 0 ORDER BY version_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var actual []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return err
		}
		actual = append(actual, version)
		if len(actual) > len(sources) {
			return ErrSchema
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	expected := make([]int64, len(sources))
	for i, source := range sources {
		expected[i] = source.Version
	}
	if !slices.Equal(actual, expected) {
		return ErrSchema
	}
	return nil
}

func Migrate(ctx context.Context, connection pq.Config, migrations fs.FS) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	pool, err := openPool(connection, "detent_migration")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, pool.Close()) }()
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	var allowed bool
	err = pool.QueryRowContext(ctx, `SELECT pg_has_role(session_user, 'detent_migration', 'MEMBER')
		AND NOT pg_has_role(session_user, 'detent_runtime', 'MEMBER')
		AND NOT pg_has_role(session_user, 'detent_platform', 'MEMBER')
		AND NOT EXISTS (SELECT 1 FROM pg_roles r
			WHERE (r.rolname = session_user OR pg_has_role(session_user, r.oid, 'MEMBER'))
			AND (r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication))`).Scan(&allowed)
	if err != nil {
		return mapError(ctx, err)
	}
	if !allowed {
		return ErrMigrationRole
	}
	provider, err := migrationProvider(pool, migrations)
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return mapError(ctx, err)
}
