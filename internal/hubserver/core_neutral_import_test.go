package hubserver

import (
	"database/sql"
	"os"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newCoreNeutralFixture(t *testing.T, organization tracker.OrganizationID, names ...string) *neutralimporttest.Fixture {
	t.Helper()
	return neutralimporttest.New(t, os.DirFS("../.."), organization, names...)
}

func coreImportPostgres(t *testing.T) *postgres.Repository {
	t.Helper()
	repository, _ := coreImportPostgresWithAdmin(t, 3)
	return repository
}
func coreImportPostgresWithAdmin(t *testing.T, maxConnections int, configure ...func(*pgxpool.Config)) (*postgres.Repository, *sql.DB) {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT "+strconv.Itoa(maxConnections+1)); err != nil {
		t.Fatal(err)
	}
	native := postgresStartupTemplate.NativeConfig(t, admin, connection.User, connection.Password)
	native.MaxConns = int32(maxConnections)
	for _, option := range configure {
		option(native)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: maxConnections, ServingConnections: maxConnections - 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository, admin
}
