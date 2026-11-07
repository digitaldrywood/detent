package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/pressly/goose/v3"
)

type Config struct {
	Connection pgtransport.Connection
	Pool       PoolConfig
	LeaseClock func() time.Time
}

var ErrConnectionConfig = errors.New("PostgreSQL requires an explicit host, port, database, user and TLS mode without session overrides or credential discovery")
var ErrMigrationRole = errors.New("PostgreSQL migration role must have separate unprivileged authority")
var ErrSchema = errors.New("PostgreSQL schema does not match this Detent version")

func Open(ctx context.Context, cfg Config, migrations fs.FS) (*Repository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := nativeConfig(cfg.Connection, "")
	if err != nil {
		return nil, err
	}
	config.MaxConns = int32(cfg.Pool.MaxConnections)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	repository, err := NewRepository(ctx, pool, cfg.Pool)
	if err == nil {
		repository.leaseClock = cfg.LeaseClock
		err = validateNativeSchema(ctx, pool, migrations)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		pool.Close()
		return nil, mapError(ctx, err)
	}
	return repository, nil
}

func nativeConfig(connection pgtransport.Connection, role string) (*pgxpool.Config, error) {
	config, err := connection.PoolConfig()
	if err != nil {
		return nil, ErrConnectionConfig
	}
	if role != "" {
		config.ConnConfig.RuntimeParams["search_path"] = "public, pg_catalog"
		config.ConnConfig.RuntimeParams["role"] = role
	}
	return config, nil
}

func openPool(connection pgtransport.Connection, role string) (*sql.DB, error) {
	config, err := nativeConfig(connection, role)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*config.ConnConfig), nil
}

func validateNativeSchema(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) error {
	if migrations == nil {
		return ErrSchema
	}
	var expected []int64
	entries, err := fs.ReadDir(RoleMigrations(migrations), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) < 15 || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := strconv.ParseInt(strings.SplitN(name, "_", 2)[0], 10, 64)
		if err != nil {
			return ErrSchema
		}
		expected = append(expected, version)
	}
	if len(expected) == 0 {
		return ErrSchema
	}
	slices.Sort(expected)
	var owned bool
	if err := pool.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) = 'detent_migration_v2' FROM pg_class WHERE oid = 'public.goose_db_version'::regclass`).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrSchema
	}
	rows, err := pool.Query(ctx, `SELECT version_id FROM (SELECT version_id,is_applied,ROW_NUMBER() OVER(PARTITION BY version_id ORDER BY id DESC) AS state_rank FROM public.goose_db_version WHERE version_id>0) AS migration_state WHERE state_rank=1 AND is_applied ORDER BY version_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	if !slices.Equal(actual, expected) {
		return ErrSchema
	}
	return nil
}

func migrationProvider(pool *sql.DB, migrations fs.FS) (*goose.Provider, error) {
	if migrations == nil {
		return nil, ErrSchema
	}
	return goose.NewProvider(goose.DialectPostgres, pool, RoleMigrations(migrations),
		goose.WithDisableGlobalRegistry(true), goose.WithAllowOutofOrder(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
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
	if err := pool.QueryRowContext(ctx, `SELECT pg_get_userbyid(relowner) = 'detent_migration_v2'
		FROM pg_class WHERE oid = 'public.goose_db_version'::regclass`).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrSchema
	}
	rows, err := pool.QueryContext(ctx, `SELECT version_id FROM (
		SELECT version_id, is_applied, ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY id DESC) AS state_rank
		FROM public.goose_db_version WHERE version_id > 0
	) AS migration_state WHERE state_rank = 1 AND is_applied ORDER BY version_id`)
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

func Migrate(ctx context.Context, connection pgtransport.Connection, migrations fs.FS) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	pool, err := openPool(connection, MigrationRoleName)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, pool.Close()) }()
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	var allowed bool
	err = pool.QueryRowContext(ctx, `SELECT pg_has_role(session_user, $1, 'MEMBER')
		AND NOT pg_has_role(session_user, $2, 'MEMBER')
		AND NOT pg_has_role(session_user, $3, 'MEMBER')
		AND NOT EXISTS (SELECT 1 FROM pg_roles r
			WHERE (r.rolname = session_user OR pg_has_role(session_user, r.oid, 'MEMBER'))
			AND (r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication))`, MigrationRoleName, RuntimeRoleName, PlatformRoleName).Scan(&allowed)
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
