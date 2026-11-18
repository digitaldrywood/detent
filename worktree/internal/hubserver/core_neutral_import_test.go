package hubserver

import (
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newCoreNeutralFixture(t *testing.T, organization tracker.OrganizationID, names ...string) *neutralimporttest.Fixture {
	t.Helper()
	return neutralimporttest.New(t, os.DirFS("../.."), organization, names...)
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

func coreWorkItemNeutralFixture(t *testing.T, org tracker.OrganizationID, project tracker.ProjectID, malformedQueue bool) *neutralimporttest.Fixture {
	t.Helper()
	return coreimporttest.WorkItems(t, os.DirFS("../.."), org, project, malformedQueue, coreimporttest.WorkItemBody())
}

func coreWorkItemNeutralFixtureBody(t *testing.T, org tracker.OrganizationID, project tracker.ProjectID, malformedQueue bool, body string) *neutralimporttest.Fixture {
	t.Helper()
	return coreimporttest.WorkItems(t, os.DirFS("../.."), org, project, malformedQueue, body)
}

func coreImportBody() string {
	return coreimporttest.WorkItemBody()
}

func coreChangeNeutralFixture(t *testing.T, organization tracker.OrganizationID, project tracker.ProjectID, createdAt time.Time, descriptor policy.Descriptor, policyJSON []byte) *neutralimporttest.Fixture {
	t.Helper()
	return coreimporttest.Changes(t, os.DirFS("../.."), organization, project, createdAt, descriptor, policyJSON)
}

func parseTestTime(t *testing.T, raw string) time.Time {
	t.Helper()
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
