package hubserver

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"

	"github.com/digitaldrywood/detent/internal/hubserver/hubconfig"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DatabaseBackend = hubconfig.DatabaseBackend

const DatabasePostgres = hubconfig.DatabasePostgres

var ErrDatabaseBackend = hubconfig.ErrDatabaseBackend
var ErrPostgresServingUnavailable = errors.New("PostgreSQL Hub serving requires the remaining storage owners to be ported")

func openPostgresDatabase(ctx context.Context, cfg postgres.Config) (*database, error) {
	migrations, err := postgres.MigrationFiles()
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

type HostedRuntime struct {
	database      *database
	startedAt     time.Time
	replays       cloudassert.ReplayCache
	runnerWindows requestmetrics.RunnerWindows
}

func OpenHostedRuntime(ctx context.Context, cfg postgres.Config) (*HostedRuntime, error) {
	db, err := openPostgresDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &HostedRuntime{database: db, startedAt: time.Now().UTC()}, nil
}

func (h *HostedRuntime) ProvisionTenant(ctx context.Context, organization tracker.OrganizationID, name, slug string, cfg Config) error {
	if !cfg.Hosted.BindsSharedOrganization(string(organization)) || cfg.SecretKeys == nil {
		return ErrHostedDatabaseBinding
	}
	if err := hostedplans.ValidateHostedConfig(cfg.Hosted); err != nil {
		return err
	}
	cfg = cfg.Normalized()
	view, err := h.database.postgres.ForOrganization(organization, cfg.Hosted.SharedEntry.Generation)
	if err != nil {
		return err
	}
	defer view.Close()
	return view.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		_, err := w.AccessOrganization(ctx)
		if err == nil || !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		cursor := make([]byte, 32)
		if _, err := rand.Read(cursor); err != nil {
			return err
		}
		defer clear(cursor)
		secret, audit, err := newNativeCursorSecret(cfg, organization)
		if err != nil {
			return err
		}
		if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: name, Slug: slug, CreatedAt: cfg.Clock().UTC()}); err != nil {
			return err
		}
		deployment, generation := cfg.Hosted.Deployment()
		if err := w.CreateAccessTenant(ctx, accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, PublicURL: cfg.Hosted.PublicURL, Deployment: deployment, AllocationGeneration: generation}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(ctx, accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: cursor}); err != nil {
			return err
		}
		return w.StoreAccessSecret(ctx, secret, audit)
	})
}

func (h *HostedRuntime) OpenTenant(ctx context.Context, organization tracker.OrganizationID, cfg Config) (*Service, error) {
	if !cfg.Hosted.BindsSharedOrganization(string(organization)) {
		return nil, ErrHostedDatabaseBinding
	}
	hosted := *cfg.Hosted
	entry := *hosted.SharedEntry
	entry.Replays = &h.replays
	hosted.SharedEntry = &entry
	cfg.Hosted = &hosted
	cfg.Postgres = nil
	return openRuntimeService(ctx, cfg, nil, h, organization)
}

func (h *HostedRuntime) tenantDatabase(organization tracker.OrganizationID, generation int64) (*database, error) {
	repo, err := h.database.postgres.ForOrganization(organization, generation)
	if err != nil {
		return nil, err
	}
	return &database{postgres: repo, schemaVersion: h.database.schemaVersion, schemaFingerprint: h.database.schemaFingerprint, dashboardCommitted: boardCommitChannel(repo)}, nil
}

func (h *HostedRuntime) Health(ctx context.Context) (MigrationHealth, error) {
	if err := h.database.health(ctx); err != nil {
		return MigrationHealth{}, err
	}
	return MigrationHealth{SchemaVersion: h.database.schemaVersion, SchemaFingerprint: h.database.schemaFingerprint, SchemaFactsComplete: true}, nil
}

func (h *HostedRuntime) Close() error { return h.database.Close() }

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
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		return err
	}
	return postgres.Migrate(ctx, connection, migrations, sources...)
}
