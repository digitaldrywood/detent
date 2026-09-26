package cloudentry

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

const (
	registryApplicationID = 0x44545247
	authApplicationID     = 0x44544155
)

//go:embed migrations/registry/*.sql migrations/auth/*.sql
var migrationFiles embed.FS

type store struct {
	db   *sql.DB
	lock *instancelock.Lock
}

func openStore(ctx context.Context, path string, applicationID int64, directory string) (*store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve store path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	lock, err := instancelock.Acquire(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("acquire store ownership: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create private store: %w", err), lock.Close())
	}
	if err := file.Close(); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	dsn := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := dsn.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "locking_mode(EXCLUSIVE)")
	query.Add("_pragma", "synchronous(FULL)")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	db.SetMaxOpenConns(1)
	result := &store{db: db, lock: lock}
	if err := result.prepare(ctx, applicationID, directory); err != nil {
		return nil, errors.Join(err, result.Close())
	}
	return result, nil
}

func (s *store) prepare(ctx context.Context, applicationID int64, directory string) error {
	var current int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&current); err != nil {
		return err
	}
	if current != applicationID {
		var tables int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table'").Scan(&tables); err != nil {
			return err
		}
		if current != 0 || tables != 0 {
			return errors.New("store file belongs to another application")
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", applicationID)); err != nil {
			return err
		}
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil || mode != "wal" {
		return errors.Join(errors.New("store requires WAL journaling"), err)
	}
	migrations, err := fs.Sub(migrationFiles, "migrations/"+directory)
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName("entry_schema_version"))
	if err != nil {
		return err
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply %s migrations: %w", directory, err)
	}
	return nil
}

func (s *store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	return errors.Join(err, s.lock.Close())
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
