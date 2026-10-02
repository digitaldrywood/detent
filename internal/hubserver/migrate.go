package hubserver

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/pressly/goose/v3"
	goosedb "github.com/pressly/goose/v3/database"
)

const (
	hubSchemaTable         = "hub_schema_version"
	supportedSchemaVersion = int64(69)
)

//go:embed migrations/*.sql migration_steps/*.sql
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

func hubGoMigrations() []*goose.Migration {
	return []*goose.Migration{
		goose.NewGoMigration(40,
			&goose.GoFunc{RunTx: backfillChangeReviewPolicies},
			&goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error { return nil }},
		),
		goose.NewGoMigration(64, &goose.GoFunc{RunTx: migrateAttachmentReferences}, nil),
		goose.NewGoMigration(65, &goose.GoFunc{RunTx: migrateConversationOrigin}, nil),
	}
}

func migrateAttachmentReferences(ctx context.Context, tx *sql.Tx) error {
	if err := migrateAttachmentAndUpdaterSchemas(ctx, tx); err != nil {
		return err
	}
	data, err := migrationFiles.ReadFile("migration_steps/00064_cloud_attachment_references.sql")
	if err != nil {
		return err
	}
	up, _, _ := strings.Cut(string(data), "-- +goose Down")
	_, err = tx.ExecContext(ctx, up)
	return err
}

func migrateAttachmentAndUpdaterSchemas(ctx context.Context, tx *sql.Tx) error {
	var attachments int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'attachments'").Scan(&attachments); err != nil {
		return err
	}
	if attachments == 0 {
		data, err := migrationFiles.ReadFile("migrations/00062_cloud_attachments.sql")
		if err != nil {
			return err
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := tx.ExecContext(ctx, up); err != nil {
			return fmt.Errorf("restore attachment schema: %w", err)
		}
	}
	var observation int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('runner_identities') WHERE name = 'update_observation_json'").Scan(&observation); err != nil {
		return err
	}
	if observation == 0 {
		data, err := migrationFiles.ReadFile("migrations/00063_runner_update_observation.sql")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(data)); err != nil {
			return fmt.Errorf("restore runner update observation schema: %w", err)
		}
	}
	return nil
}

func migrateConversationOrigin(ctx context.Context, tx *sql.Tx) error {
	if err := migrateAttachmentAndUpdaterSchemas(ctx, tx); err != nil {
		return err
	}
	var origin int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('conversations') WHERE name = 'origin'").Scan(&origin); err != nil {
		return err
	}
	if origin != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE conversations ADD COLUMN origin TEXT NOT NULL DEFAULT 'user' CHECK (origin IN ('user', 'worker'))"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE conversations SET origin = 'worker'
WHERE work_item_id IS NOT NULL AND visibility = 'shared'
  AND NOT EXISTS (
    SELECT 1 FROM conversation_audience_events
    WHERE conversation_id = conversations.id AND from_visibility = 'private'
  )`)
	return err
}
