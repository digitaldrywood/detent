package hubserver

import (
	"database/sql"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func openHostedPostgresService(t *testing.T, cfg Config) *Service {
	t.Helper()
	cfg = hostedPostgresConfig(t, cfg)
	service := openTestService(t, cfg)
	if service.config.Postgres == nil {
		t.Fatal("hosted fixture PostgreSQL configuration is unavailable")
	}
	admin, ok := projectRecordDatabases.Load(service.config.Postgres.Connection.Database)
	if ok {
		projectRecordDatabases.Store(service.config.TenantRepository, admin)
		t.Cleanup(func() { projectRecordDatabases.Delete(service.config.TenantRepository) })
	}
	return service
}

func hostedPostgresAdmin(t *testing.T, service *Service) *sql.DB {
	t.Helper()
	if value, ok := projectRecordDatabases.Load(service.config.TenantRepository); ok {
		return value.(*sql.DB)
	}
	if service.config.Postgres != nil {
		if value, ok := projectRecordDatabases.Load(service.config.Postgres.Connection.Database); ok {
			return value.(*sql.DB)
		}
	}
	t.Fatal("hosted fixture PostgreSQL database is unavailable")
	return nil
}

func seedHostedPostgresProject(t *testing.T, service *Service, id tracker.ProjectID) {
	t.Helper()
	organization := service.accessOrganization()
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: "private-project-sentinel", Slug: "private-project-sentinel", Profile: "native"}, States: states})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seedNativeFixturePolicy(t, service, string(organization)+"/"+string(id))
}
