package cloudentry

import (
	"database/sql"
	"log/slog"
	"testing"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func runtimePublicationFixture(t *testing.T) (entryFixture, *sql.DB) {
	t.Helper()
	admin := entryPostgresDatabase(t)
	var database string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pq.QuoteIdentifier(database)+" CONNECTION LIMIT 32"); err != nil {
		t.Fatal(err)
	}
	return newEntryFixtureWithConfig(t, slog.New(slog.DiscardHandler), []func(*Config){entryPostgresConfig(t, admin)}, func(cfg *hubserver.Config) { configurePostgresTenant(t, cfg, admin) }), admin
}

func TestRuntimePublicationTenantIsolation(t *testing.T) {
	f, _ := runtimePublicationFixture(t)
	organizations := []tracker.OrganizationID{"org_alpha", "org_beta"}
	repos := make(map[tracker.OrganizationID]tenantstore.Repository)
	stage := func(org tracker.OrganizationID, id string, published bool) {
		t.Helper()
		if err := repos[org].WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StageRuntime(t.Context(), billingstore.RuntimeRevision{ID: id, Published: published, Reference: billingstore.AuditReference{Key: "example/" + string(org) + "/" + id}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	current := func(org tracker.OrganizationID, want string) {
		t.Helper()
		if err := repos[org].ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.CurrentRuntime(t.Context())
			if err == nil && got.ID != want {
				t.Fatalf("%s runtime=%s want=%s", org, got.ID, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range organizations {
		repos[org] = tenantPostgresRepository(t, f.fixtures[string(org)])
		stage(org, "initial-"+string(org), true)
		stage(org, "shared-publication", false)
		current(org, "initial-"+string(org))
	}
	if err := f.service.registry.store.db.postgres.PublishRuntime(t.Context(), "shared-publication", "entry", "example/payload", organizations); err != nil {
		t.Fatal(err)
	}
	for _, org := range organizations {
		current(org, "shared-publication")
		stage(org, "failed", false)
		current(org, "shared-publication")
	}
	stage(organizations[1], "newer", false)
	if err := f.service.registry.store.db.postgres.PublishRuntime(t.Context(), "failed", "entry", "example/failed", organizations); err == nil {
		t.Fatal("published overwritten preparation")
	}
	for _, org := range organizations {
		current(org, "shared-publication")
	}
	if err := repos[organizations[0]].ReadTenant(t.Context(), organizations[0], corestore.Serving, func(r tenantstore.Reader) error {
		published, err := r.RuntimePublished(t.Context(), "initial-org_beta")
		if published {
			t.Fatal("foreign tenant runtime visible")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE public.hosted_runtime_heads SET organization_id=organization_id WHERE organization_id=?", "org_alpha"); err == nil {
		t.Fatal("platform changed tenant runtime head")
	}
	connector, err := pq.NewConnectorConfig(f.fixtures["org_alpha"].config.Postgres.Connection)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDB := sql.OpenDB(connector)
	defer runtimeDB.Close()
	if _, err := runtimeDB.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES('forged')"); err == nil {
		t.Fatal("tenant published a platform revision")
	}
}
