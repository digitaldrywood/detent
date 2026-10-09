package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationMu sync.Mutex

func runMigrations(ctx context.Context, db *sql.DB, lockPath string) (err error) {
	if lockPath != "" {
		release, lockErr := lockMigrations(ctx, lockPath)
		if lockErr != nil {
			return fmt.Errorf("locking migrations %s: %w", lockPath, lockErr)
		}
		defer func() {
			if releaseErr := release(); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("unlocking migrations %s: %w", lockPath, releaseErr))
			}
		}()
	}
	return runMigrationsFromFS(ctx, db, migrationsFS)
}

func migrationLockPath(path string) string {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.Contains(path, "?") {
		return ""
	}
	return path + ".migrate.lock"
}

func sqliteDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_txlock=immediate"
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
