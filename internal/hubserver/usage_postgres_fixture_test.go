package hubserver

import (
	"database/sql"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func usagePostgresAdmin(t *testing.T, service *Service) *sql.DB {
	t.Helper()
	repository, ok := service.config.TenantRepository.(*postgres.Repository)
	if !ok {
		t.Fatal("usage fixture requires a PostgreSQL repository")
	}
	value, ok := projectRecordDatabases.Load(repository)
	if !ok {
		t.Fatal("usage fixture PostgreSQL database is unavailable")
	}
	return value.(*sql.DB)
}
