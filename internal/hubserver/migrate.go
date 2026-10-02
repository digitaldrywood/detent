package hubserver

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/pressly/goose/v3"
	goosedb "github.com/pressly/goose/v3/database"
)

const (
	hubSchemaTable         = "hub_schema_version"
	supportedSchemaVersion = int64(62)
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func runMigrations(ctx context.Context, db *sql.DB, logger *slog.Logger) (int64, error) {
	migrations, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return 0, fmt.Errorf("open hub migrations: %w", err)
	}
	store, err := goosedb.NewStore(goosedb.DialectSQLite3, hubSchemaTable)
	if err != nil {
		return 0, fmt.Errorf("create hub migration store: %w", err)
	}
	provider, err := goose.NewProvider(
		goose.DialectCustom,
		db,
		migrations,
		goose.WithDisableGlobalRegistry(true),
		goose.WithStore(hubMigrationStore{Store: store}),
		goose.WithSlog(logger),
		goose.WithGoMigrations(hubGoMigrations()...),
	)
	if err != nil {
		return 0, fmt.Errorf("create hub migration provider: %w", err)
	}
	return applyHubMigrations(ctx, db, provider)
}

func applyHubMigrations(ctx context.Context, db *sql.DB, provider *goose.Provider) (version int64, resultErr error) {
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return 0, fmt.Errorf("read hub schema versions: %w", err)
	}
	if target != supportedSchemaVersion {
		return 0, fmt.Errorf("embedded hub schema version is %d, want %d", target, supportedSchemaVersion)
	}
	if current > target {
		return 0, fmt.Errorf("%w: database=%d supported=%d", ErrUnsupportedSchema, current, target)
	}
	pending, err := provider.HasPending(ctx)
	if err != nil {
		return 0, fmt.Errorf("read pending hub migrations: %w", err)
	}
	if !pending {
		return current, nil
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return 0, fmt.Errorf("prepare hub schema rebuild: %w", err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore hub foreign key enforcement: %w", err))
		}
	}()
	if _, err := provider.Up(ctx); err != nil {
		return 0, fmt.Errorf("apply hub migrations: %w", err)
	}
	current, target, err = provider.GetVersions(ctx)
	if err != nil {
		return 0, fmt.Errorf("verify hub schema versions: %w", err)
	}
	if current != target {
		return 0, fmt.Errorf("hub schema migration stopped at %d, want %d", current, target)
	}
	return current, nil
}

type hubMigrationStore struct {
	goosedb.Store
}

func (s hubMigrationStore) Insert(ctx context.Context, db goosedb.DBTxConn, request goosedb.InsertRequest) error {
	if request.Version == supportedSchemaVersion {
		var violations int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
			return fmt.Errorf("validate hub foreign keys: %w", err)
		}
		if violations != 0 {
			return fmt.Errorf("hub migration has %d foreign key violations", violations)
		}
	}
	return s.Store.Insert(ctx, db, request)
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var version int64
	query := fmt.Sprintf("SELECT COALESCE(MAX(version_id), 0) FROM %s WHERE is_applied = 1", hubSchemaTable)
	if err := db.QueryRowContext(ctx, query).Scan(&version); err != nil {
		return 0, fmt.Errorf("read hub schema version: %w", err)
	}
	return version, nil
}

// hubGoMigrations are the schema steps SQL cannot express: 40 computes the
// content-addressed identity of each backfilled review policy.
func hubGoMigrations() []*goose.Migration {
	return []*goose.Migration{
		goose.NewGoMigration(40,
			&goose.GoFunc{RunTx: backfillChangeReviewPolicies},
			&goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error { return nil }},
		),
	}
}
