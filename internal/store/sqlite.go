package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/symphony-go/internal/store/sqlc"
)

type sqliteStore struct {
	db      *sql.DB
	queries *sqlc.Queries
}

func openSQLite(ctx context.Context, cfg Config) (*sqliteStore, error) {
	if cfg.Path == "" {
		return nil, errors.New("sqlite path is required")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("creating sqlite directory: %w", err)
	}

	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := configureSQLite(ctx, db, busyTimeoutMillis(cfg.BusyTimeout)); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &sqliteStore{
		db:      db,
		queries: sqlc.New(db),
	}, nil
}

func (s *sqliteStore) Queries() *sqlc.Queries {
	return s.queries
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

func configureSQLite(ctx context.Context, db *sql.DB, busyTimeoutMillis int64) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMillis)); err != nil {
		return fmt.Errorf("setting sqlite busy_timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enabling sqlite foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enabling sqlite WAL: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging sqlite database: %w", err)
	}
	return nil
}
