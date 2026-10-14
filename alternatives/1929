package hubserver

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

func migrateRequestMetrics(ctx context.Context, db *sql.DB) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, nil, goose.WithDisableGlobalRegistry(true), goose.WithGoMigrations(goose.NewGoMigration(20261009071000, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE tenant(organization TEXT PRIMARY KEY);
CREATE TABLE request_minutes(
 minute INTEGER NOT NULL, route TEXT NOT NULL, caller_kind TEXT NOT NULL, caller_id TEXT NOT NULL, status_class INTEGER NOT NULL,
 count INTEGER NOT NULL, sum_ms REAL NOT NULL, p50_ms REAL NOT NULL, p95_ms REAL NOT NULL, max_ms REAL NOT NULL, histogram TEXT NOT NULL,
 PRIMARY KEY(minute,route,caller_kind,caller_id,status_class));`)
		return err
	}}, nil)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}
