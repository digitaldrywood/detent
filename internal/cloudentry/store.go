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

	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/schemafingerprint"
)

const (
	registryApplicationID = platformstore.RegistryCopyApplicationID
	authApplicationID     = platformstore.AuthCopyApplicationID
)

//go:embed migrations/registry/*.sql migrations/auth/*.sql
var migrationFiles embed.FS

type store struct {
	db     *entryDatabase
	reader *entryDatabase
	lock   *instancelock.Lock
	close  func() error
}

func (s *store) schemaVersion(ctx context.Context) (int64, error) {
	if s.db.postgres != nil {
		return s.db.postgres.SchemaVersion(ctx)
	}
	var version int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM (
		SELECT version_id, is_applied, ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY id DESC) AS state_rank
		FROM entry_schema_version
	) WHERE state_rank = 1 AND is_applied = 1`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read entry schema version: %w", err)
	}
	return version, nil
}

func (s *store) schemaFingerprint(ctx context.Context) (string, error) {
	if s.db.postgres != nil {
		return s.db.postgres.SchemaFingerprint(ctx)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version_id FROM (
		SELECT version_id, is_applied, ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY id DESC) AS state_rank
		FROM entry_schema_version
	) WHERE state_rank = 1 AND is_applied = 1 ORDER BY version_id`)
	if err != nil {
		return "", fmt.Errorf("read applied entry migration versions: %w", err)
	}
	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return "", errors.Join(err, rows.Close())
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return "", errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("close entry migration versions: %w", err)
	}
	return schemafingerprint.Versions(versions), nil
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
	db, err := sql.Open("sqlite", storeDSN(path))
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	db.SetMaxOpenConns(1)
	result := &store{db: &entryDatabase{DB: db}, lock: lock}
	if err := result.prepare(ctx, applicationID, directory); err != nil {
		return nil, errors.Join(err, result.Close())
	}
	reader, err := sql.Open("sqlite", storeDSN(path)+"&_pragma=query_only(1)")
	if err != nil {
		return nil, errors.Join(err, result.Close())
	}
	result.reader = &entryDatabase{DB: reader}
	result.reader.SetMaxOpenConns(4)
	if err := result.reader.PingContext(ctx); err != nil {
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
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db.DB, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName("entry_schema_version"), goose.WithAllowOutofOrder(true))
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
	if s.close != nil {
		return s.close()
	}
	var err error
	if s.reader != nil {
		err = s.reader.Close()
	}
	if s.db != nil {
		err = errors.Join(err, s.db.Close())
	}
	return errors.Join(err, s.lock.Close())
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func storeDSN(path string) string {
	databasePath := filepath.ToSlash(path)
	if len(databasePath) >= 3 && databasePath[1] == ':' && databasePath[2] == '/' && (databasePath[0] >= 'A' && databasePath[0] <= 'Z' || databasePath[0] >= 'a' && databasePath[0] <= 'z') {
		databasePath = "/" + databasePath
	}
	dsn := &url.URL{Scheme: "file", Path: databasePath}
	query := dsn.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_txlock", "immediate")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}
