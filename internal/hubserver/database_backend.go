package hubserver

import (
	"context"
	"errors"
	"io/fs"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/digitaldrywood/detent/internal/hubserver/hubconfig"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type DatabaseBackend = hubconfig.DatabaseBackend

const DatabasePostgres = hubconfig.DatabasePostgres

var ErrDatabaseBackend = hubconfig.ErrDatabaseBackend
var ErrPostgresServingUnavailable = errors.New("PostgreSQL Hub serving requires the remaining storage owners to be ported")

func validateDatabaseBackend(cfg Config) error {
	return cfg.ValidateDatabaseBackend()
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
	version, err := postgres.LatestSchemaVersion()
	if err != nil {
		return nil, errors.Join(err, repository.Close())
	}
	fingerprint, err := postgres.LatestSchemaFingerprint()
	if err != nil {
		return nil, errors.Join(err, repository.Close())
	}
	return &database{postgres: repository, schemaVersion: version, schemaFingerprint: fingerprint, dashboardCommitted: boardCommitChannel(repository)}, nil
}

func boardCommitChannel(repository *postgres.Repository) chan struct{} {
	committed := make(chan struct{}, 1)
	repository.SetBoardCommitNotifier(func() {
		select {
		case committed <- struct{}{}:
		default:
		}
	})
	return committed
}

func MigratePostgres(ctx context.Context, connection pgtransport.Connection) error {
	return MigratePostgresWithConversationSources(ctx, connection)
}

func MigratePostgresWithConversationSources(ctx context.Context, connection pgtransport.Connection, sources ...postgres.ConversationHistoryImportSource) error {
	migrations, err := fs.Sub(migrationFiles, "postgres/migrations")
	if err != nil {
		return err
	}
	return postgres.Migrate(ctx, connection, migrations, sources...)
}
