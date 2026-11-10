package hubserver

import (
	"io/fs"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func tfConvReopenConversationService(t *testing.T, original *Service, configure ...func(*Config)) *Service {
	t.Helper()
	cfg := original.config
	admin := conversationTestAdmin(t, original)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	organization, local := original.database.hostedOrganization, original.localOrganization
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	migrations, err := fs.Sub(migrationFiles, "postgres/migrations")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), postgres.Config{Connection: connection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}, migrations)
	if err != nil {
		t.Fatal(err)
	}
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() {
		projectRecordDatabases.Delete(repository)
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	cfg.Postgres = nil
	cfg.DatabaseBackend = ""
	cfg.TenantRepository = repository
	cfg.AccessRepository = nil
	cfg.BillingRepository = nil
	for _, apply := range configure {
		apply(&cfg)
	}
	var service *Service
	if cfg.Hosted == nil {
		logger := cfg.Logger
		cfg.Logger = discardLogger()
		service = openLocalPostgresHubTestService(t, repository, cfg)
		service.config.Logger = logger
		service.database.logger = logger
		if service.conversations != nil {
			service.conversations.logger = logger.With("component", "conversation")
			service.registerConversationRoutes(service.echo)
		}
	} else {
		service = openPostgresHubTestService(t, repository, cfg)
	}
	service.database.hostedOrganization = organization
	service.localOrganization = local
	service.requestMetrics.organization = organization
	if cfg.Hosted == nil && service.conversations != nil {
		if err := service.conversations.start(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	return service
}
