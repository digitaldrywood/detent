package hubserver

import (
	"context"
	"errors"
	"io/fs"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type DatabaseBackend string

const (
	DatabaseSQLite   DatabaseBackend = "sqlite"
	DatabasePostgres DatabaseBackend = "postgres"
)

var ErrDatabaseBackend = errors.New("hub database backend configuration is invalid")
var ErrPostgresServingUnavailable = errors.New("PostgreSQL Hub serving requires the remaining storage owners to be ported")

func validateDatabaseBackend(cfg Config) error {
	switch cfg.DatabaseBackend {
	case "", DatabaseSQLite:
		if cfg.Postgres != nil {
			return ErrDatabaseBackend
		}
	case DatabasePostgres:
		if cfg.Postgres == nil || cfg.DatabasePath != "" || cfg.CredentialMaintenance || cfg.hostedBindingMigration {
			return ErrDatabaseBackend
		}
	default:
		return ErrDatabaseBackend
	}
	return nil
}

func openPostgresDatabase(ctx context.Context, cfg postgres.Config) (*database, error) {
	migrations, err := fs.Sub(migrationFiles, "postgres/migrations")
	if err != nil {
		return nil, err
	}
	repository, err := postgres.Open(ctx, cfg, migrations)
	if err != nil {
		return nil, err
	}
	return &database{postgres: repository}, nil
}

func MigratePostgres(ctx context.Context, connection pq.Config) error {
	migrations, err := fs.Sub(migrationFiles, "postgres/migrations")
	if err != nil {
		return err
	}
	return postgres.Migrate(ctx, connection, migrations)
}
