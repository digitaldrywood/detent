package hostedplans

import (
	"database/sql"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func postgresWithAdmin(t *testing.T, maxConnections int) (*postgres.Repository, *sql.DB) {
	t.Helper()
	admin := hubtest.Database(t)
	return hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(maxConnections)), admin
}
