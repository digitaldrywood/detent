package cloudentry

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestRegistryMigrationPreservesOrganizationEvents(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := fs.Sub(migrationFiles, "migrations/registry")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations, goose.WithDisableGlobalRegistry(true), goose.WithTableName("entry_schema_version"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"PRAGMA application_id = 1146376775",
		"INSERT INTO organizations(id,provider_id,name,state,endpoint,generation,created_at,updated_at) VALUES ('org_a','porg_a','A','ready','unix:/run/a.sock',1,'2026-09-26T00:00:00Z','2026-09-26T00:00:00Z')",
		"INSERT INTO organization_events(organization_id,event,generation,recorded_at) VALUES ('org_a','registered_ready',1,'2026-09-26T00:00:00Z')",
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(t.Context(), path)
	if err != nil {
		t.Fatalf("upgrade with existing events failed: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	organization, err := registry.Organization(t.Context(), "org_a")
	if err != nil || organization.State != "ready" || organization.ProviderID != "porg_a" {
		t.Fatalf("migrated organization = %+v, %v", organization, err)
	}
	var events, violations int
	if err := registry.store.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM organization_events), (SELECT count(*) FROM pragma_foreign_key_check)").Scan(&events, &violations); err != nil || events != 1 || violations != 0 {
		t.Fatalf("events = %d violations = %d err = %v", events, violations, err)
	}
	if _, err := registry.store.db.ExecContext(t.Context(), "INSERT INTO organization_events(organization_id,event,generation,recorded_at) VALUES ('org_missing','x',1,'2026-09-26T00:00:00Z')"); err == nil {
		t.Fatal("event foreign key was lost")
	}
}
