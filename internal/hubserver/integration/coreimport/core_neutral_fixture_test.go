package coreimport_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newCoreNeutralFixture(t *testing.T, organization tracker.OrganizationID, names ...string) *neutralimporttest.Fixture {
	t.Helper()
	return neutralimporttest.New(t, os.DirFS("../../../.."), organization, names...)
}

func coreImportPostgres(t *testing.T) *postgres.Repository {
	t.Helper()
	repository, _ := coreImportPostgresWithAdmin(t, 3)
	return repository
}
func coreImportPostgresWithAdmin(t *testing.T, maxConnections int, configure ...func(*pgxpool.Config)) (*postgres.Repository, *sql.DB) {
	t.Helper()
	return hubfixture.RepositoryWithAdmin(t, maxConnections, configure...)
}
