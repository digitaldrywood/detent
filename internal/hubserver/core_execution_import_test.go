package hubserver

import (
	"database/sql"
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coreExecutionNeutralFixture(t *testing.T, organization tracker.OrganizationID) *neutralimporttest.Fixture {
	t.Helper()
	return coreimporttest.Execution(t, os.DirFS("../.."), organization)
}

func seedCoreExecutionPayloadTarget(t *testing.T, repository *postgres.Repository, admin *sql.DB, organization tracker.OrganizationID) {
	t.Helper()
	coreimporttest.SeedExecutionTarget(t, repository, admin, organization)
}
