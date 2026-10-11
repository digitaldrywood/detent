package cloudentry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/instancelock"
)

func TestDevelopMigrationUsesDisposableReleaseState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state, tenants := filepath.Join(root, "entry"), filepath.Join(root, "tenants")
	registry, err := OpenRegistry(t.Context(), filepath.Join(state, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"org_alpha", "org_beta"} {
		_, err := registry.Register(t.Context(), Organization{ID: id, ProviderID: "provider_" + id, Name: "Released tenant", Generation: 1, Endpoint: "unix:" + filepath.Join(root, "sockets", id+".sock")})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET managed = 1"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	auth, err := openStore(t.Context(), filepath.Join(state, "auth.db"), authApplicationID, "auth")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.db.ExecContext(t.Context(), "CREATE TABLE preview_fixture(value TEXT); INSERT INTO preview_fixture VALUES ('released')"); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	base := fstest.MapFS{"20261006202000_release.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE change_versions(id TEXT PRIMARY KEY);\nINSERT INTO change_versions VALUES ('released-version');\n")}}
	developMigration, err := os.ReadFile("../hubserver/migrations/20261006212000_change_sources.sql")
	if err != nil {
		t.Fatal(err)
	}
	develop := fstest.MapFS{"20261006202000_release.sql": base["20261006202000_release.sql"], "20261006212000_change_sources.sql": &fstest.MapFile{Data: developMigration}}
	withDatabase := func(path string, visit func(*sql.DB)) {
		t.Helper()
		db, err := sql.Open("sqlite", storeDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}()
		visit(db)
	}
	provider := func(db *sql.DB, files fs.FS) *goose.Provider {
		t.Helper()
		provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithDisableGlobalRegistry(true), goose.WithTableName("hub_schema_version"), goose.WithSlog(slog.New(slog.DiscardHandler)))
		if err != nil {
			t.Fatal(err)
		}
		return provider
	}
	for _, id := range []string{"org_alpha", "org_beta"} {
		directory := filepath.Join(tenants, id)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"tenant.yaml", "admin-token"} {
			if err := os.WriteFile(filepath.Join(directory, name), []byte("released "+name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		withDatabase(filepath.Join(directory, "hub.db"), func(db *sql.DB) {
			if _, err := provider(db, base).Up(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	config := Config{Build: buildinfo.Info{Version: "develop-c6d3f432e"}, Platform: PlatformConfig{BootstrapAdminEmail: "operator@example.test"}, PublicURL: "https://staging.cloud.detent.build", StateDir: state, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), Provider: newFakeProvider(), Logger: slog.New(slog.DiscardHandler), clientFS: fstest.MapFS{}, Allocation: &AllocationConfig{TenantRoot: tenants, SocketRoot: filepath.Join(root, "sockets"), MaxTenants: 2, MaxConcurrent: 1, RetryLimit: 1}}
	for _, version := range []string{"develop-c6d3f432e", "develop-9a5915f53", "0.117.45"} {
		config.Build.Version = version
		allocation := *config.Allocation
		allocation.Launcher = &silentLauncher{running: map[string]bool{}}
		config.Allocation = &allocation
		if version == "develop-9a5915f53" {
			if err := os.MkdirAll(filepath.Join(state, ".develop"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, ".develop", "previous-deploy"), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		service, err := Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = service.Close() })
		if _, err := os.Stat(filepath.Join(state, ".develop", "previous-deploy")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("previous deploy state was retained: %v", err)
		}
		preview := strings.HasPrefix(version, "develop-")
		if (service.config.StateDir != state) != preview || (service.config.Allocation.TenantRoot != tenants) != preview {
			t.Fatalf("%s state routing = %s / %s", version, service.config.StateDir, service.config.Allocation.TenantRoot)
		}
		organization, err := service.registry.Organization(t.Context(), "org_alpha")
		if err != nil || organization.Name != "Released tenant" {
			t.Fatalf("preview retained previous develop registry writes: %+v %v", organization, err)
		}
		for _, id := range []string{"org_alpha", "org_beta"} {
			organization, err := service.registry.Organization(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			spec := service.tenantSpec(organization)
			if spec.Socket != filepath.Join(service.config.Allocation.SocketRoot, id+".sock") || spec.Directory != filepath.Join(service.config.Allocation.TenantRoot, id) {
				t.Fatalf("tenant escaped preview routing: %+v", spec)
			}
			for _, name := range []string{"tenant.yaml", "admin-token"} {
				value, err := os.ReadFile(filepath.Join(spec.Directory, name))
				if err != nil || string(value) != "released "+name {
					t.Fatalf("tenant state not reseeded: %q %v", value, err)
				}
				info, err := os.Stat(filepath.Join(spec.Directory, name))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("copied credential permissions: %v %v", info, err)
				}
			}
			withDatabase(filepath.Join(spec.Directory, "hub.db"), func(db *sql.DB) {
				current, supported, err := provider(db, base).GetVersions(t.Context())
				if err != nil || current != 20261006202000 || supported != 20261006202000 {
					t.Fatalf("%s blocked by develop-only migration: database=%d supported=%d err=%v", version, current, supported, err)
				}
				if version == "develop-c6d3f432e" {
					if _, err := provider(db, develop).Up(t.Context()); err != nil {
						t.Fatal(err)
					}
					current, supported, err = provider(db, base).GetVersions(t.Context())
					if err != nil || current != 20261006212000 || supported != 20261006202000 {
						t.Fatalf("reported refusal was not reproduced in preview: database=%d supported=%d err=%v", current, supported, err)
					}
				}
			})
		}
		var value string
		if err := service.auth.store.db.QueryRowContext(t.Context(), "SELECT value FROM preview_fixture").Scan(&value); err != nil || value != "released" {
			t.Fatalf("preview retained previous auth writes: %q %v", value, err)
		}
		if preview {
			if _, err := service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET name = 'Develop only'"); err != nil {
				t.Fatal(err)
			}
			if _, err := service.auth.store.db.ExecContext(t.Context(), "UPDATE preview_fixture SET value = 'develop only'"); err != nil {
				t.Fatal(err)
			}
			lock, err := instancelock.Acquire(filepath.Join(state, "registry.db.lock"))
			if lock != nil {
				_ = lock.Close()
			}
			if !errors.Is(err, instancelock.ErrHeld) {
				t.Fatalf("release state lost single-owner protection: %v", err)
			}
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if preview {
			if _, err := os.Stat(filepath.Join(state, ".develop")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("disposable state survived shutdown: %v", err)
			}
		}
	}
}

func TestDevelopSnapshotIncludesCommittedWAL(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db, err := sql.Open("sqlite", storeDSN(filepath.Join(root, "writer.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE committed(value TEXT); INSERT INTO committed VALUES ('released')"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		value, err := os.ReadFile(filepath.Join(root, "writer.db") + suffix)
		if err != nil || len(value) == 0 {
			t.Fatalf("WAL fixture %s: %v", suffix, err)
		}
		if err := os.WriteFile(filepath.Join(root, "release.db")+suffix, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "release.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshotDevelopDatabase(t.Context(), filepath.Join(root, "release.db"), filepath.Join(root, "preview.db"), false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "release.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("snapshot wrote to release database: %v", err)
	}
	preview, err := sql.Open("sqlite", storeDSN(filepath.Join(root, "preview.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer preview.Close()
	var value string
	if err := preview.QueryRowContext(t.Context(), "SELECT value FROM committed").Scan(&value); err != nil || value != "released" {
		t.Fatalf("snapshot lost committed WAL data: %q %v", value, err)
	}
}

func TestDevelopStateSelectionAndFailure(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, version, origin, fault string
	}{
		{name: "staging release", version: "0.117.45", origin: "https://staging.cloud.detent.build"},
		{name: "local development", version: "dev", origin: "https://staging.cloud.detent.build"},
		{name: "production", version: "develop-aabbccdde", origin: "https://cloud.detent.build"},
		{name: "other deployment", version: "develop-aabbccdde", origin: "https://cloud.example.test"},
		{name: "corrupt database", version: "develop-aabbccdde", origin: "https://staging.cloud.detent.build", fault: "corrupt"},
		{name: "cancelled copy", version: "develop-aabbccdde", origin: "https://staging.cloud.detent.build", fault: "cancelled"},
		{name: "live release owner", version: "develop-aabbccdde", origin: "https://staging.cloud.detent.build", fault: "held"},
		{name: "unmanaged tenant roots", version: "develop-aabbccdde", origin: "https://staging.cloud.detent.build", fault: "overlap"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "entry")
			config := Config{Build: buildinfo.Info{Version: test.version}, PublicURL: test.origin, StateDir: state, Logger: slog.New(slog.DiscardHandler), Allocation: &AllocationConfig{TenantRoot: filepath.Join(root, "tenants"), SocketRoot: filepath.Join(root, "sockets")}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var held *instancelock.Lock
			if test.fault != "" {
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "registry.db"), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch test.fault {
			case "cancelled":
				cancel()
			case "held":
				var err error
				held, err = instancelock.Acquire(filepath.Join(state, "registry.db.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
			case "overlap":
				config.Allocation.TenantRoot = state
			}
			prepared, closeState, err := prepareDevelopState(ctx, config)
			if test.fault == "" {
				if err != nil || closeState != nil || prepared.StateDir != state || prepared.Allocation.TenantRoot != config.Allocation.TenantRoot {
					t.Fatalf("release or unrelated deployment changed state: %+v %v", prepared, err)
				}
				if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unrelated startup wrote state: %v", err)
				}
				return
			}
			if err == nil || closeState != nil {
				t.Fatalf("failed preview fell back to release state: %v", err)
			}
			if test.fault == "held" && !errors.Is(err, instancelock.ErrHeld) || test.fault == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("preview lost source failure: %v", err)
			}
			if held != nil {
				if err := held.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(state, ".develop")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed startup retained preview state: %v", err)
			}
			lock, err := instancelock.Acquire(filepath.Join(state, "registry.db.lock"))
			if err != nil {
				t.Fatalf("failed startup retained release ownership: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRegistryMigrationPreservesOrganizationEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
