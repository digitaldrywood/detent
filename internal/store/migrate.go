package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sync"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationMu sync.Mutex

func runMigrations(ctx context.Context, db *sql.DB) error {
	return runMigrationsFromFS(ctx, db, migrationsFS)
}

func runMigrationsFromFS(ctx context.Context, db *sql.DB, migrations fs.FS) error {
	migrationMu.Lock()
	defer migrationMu.Unlock()

	goose.SetBaseFS(migrations)
	defer goose.SetBaseFS(nil)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations", goose.WithAllowMissing()); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}
