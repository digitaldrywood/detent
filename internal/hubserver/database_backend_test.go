package hubserver

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var postgresStartupTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := os.ReadFile("postgres/roles.sql")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `SET ROLE detent_migration`); err != nil {
		return err
	}
	provider, err := startupMigrationProvider(db)
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func startupMigrationProvider(pool *sql.DB) (*goose.Provider, error) {
	migrations, err := fs.Sub(migrationFiles, "postgres/migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, pool, migrations,
		goose.WithDisableGlobalRegistry(true), goose.WithSlog(discardLogger()))
}

func startupConnection(t *testing.T, admin *sql.DB, group, attributes string) pq.Config {
	t.Helper()
	connection := pq.Config{Host: "127.0.0.1", SSLMode: pq.SSLModeDisable,
		User: "example_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Password: uuid.NewString()}
	var dataDirectory string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database(), current_setting('port')::integer,
		COALESCE(inet_server_addr()::text, current_setting('unix_socket_directories')),
		current_setting('data_directory')`).Scan(&connection.Database, &connection.Port, &connection.Host, &dataDirectory); err != nil {
		t.Fatal(err)
	}
	if connection.Host == "." {
		connection.Host = dataDirectory
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pq.QuoteIdentifier(connection.Database)+" CONNECTION LIMIT 6"); err != nil {
		t.Fatal(err)
	}
	name := pq.QuoteIdentifier(connection.User)
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+name+" LOGIN PASSWORD "+pq.QuoteLiteral(connection.Password)+" "+attributes); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("PostgreSQL tests require a process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+name); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+name); err != nil {
			t.Error(err)
		}
	})
	if group != "" {
		if _, err := admin.ExecContext(t.Context(), "GRANT "+pq.QuoteIdentifier(group)+" TO "+name); err != nil {
			t.Fatal(err)
		}
	}
	return connection
}

func startupConfig(connection pq.Config) Config {
	return Config{DatabaseBackend: DatabasePostgres, Postgres: &postgres.Config{
		Connection: connection,
		Pool:       postgres.PoolConfig{MaxConnections: 2, ServingConnections: 1, BackgroundConnections: 1},
	}, Hosted: &HostedConfig{
		OrganizationID:       "org_startup",
		WorkOSOrganizationID: "provider_startup",
		PublicURL:            "https://hub.example.test",
		Provider:             newHostedSecurityProvider(),
	}, RunDetails: &memoryRunDetails{}}
}

func seedStartupOrganization(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID) {
	t.Helper()
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example-" + strings.TrimPrefix(string(organization), "org_"), CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization), AllocationGeneration: 1}); err != nil {
			return err
		}
		if organization != "org_startup" {
			return nil
		}
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{
			OrganizationID: organization,
			ID:             "test_hub_admin",
			Name:           "Example administrator",
			Hash:           apikey.HashToken(testHubAdminToken),
			Scope:          "admin",
			CreatedAt:      now,
			UpdatedAt:      now,
			KeyScope:       "admin",
			ProjectAccess:  "all",
			Kind:           "service",
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func assertStartupDisconnected(t *testing.T, admin *sql.DB, connection pq.Config) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE usename = $1`, connection.User).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("startup retained %d PostgreSQL connections", count)
		}
		return nil
	})
}

func TestDatabaseBackendRejectsAmbiguousConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
	}{
		{"unknown backend", Config{DatabaseBackend: "unknown"}},
		{"missing postgres", Config{DatabaseBackend: DatabasePostgres}},
		{"implicit postgres", Config{Postgres: &postgres.Config{}}},
		{"sqlite with postgres", Config{DatabaseBackend: DatabaseSQLite, Postgres: &postgres.Config{}}},
		{"two authorities", Config{DatabaseBackend: DatabasePostgres, Postgres: &postgres.Config{}, DatabasePath: "example.db"}},
		{"missing hosted tenant", Config{DatabaseBackend: DatabasePostgres, Postgres: &postgres.Config{}}},
		{"sqlite maintenance", Config{DatabaseBackend: DatabasePostgres, Postgres: &postgres.Config{}, CredentialMaintenance: true}},
		{"sqlite binding migration", Config{DatabaseBackend: DatabasePostgres, Postgres: &postgres.Config{}, hostedBindingMigration: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unopened", "hub.db")
			if test.config.DatabasePath != "" {
				test.config.DatabasePath = path
			}
			store, err := openDatabase(t.Context(), test.config.normalized())
			if !errors.Is(err, ErrDatabaseBackend) || store != nil {
				t.Fatalf("open = %v, %v", store, err)
			}
			if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid backend touched SQLite directory: %v", err)
			}
		})
	}
}

func TestPostgresStartupRejectsImplicitConnectionSettings(t *testing.T) {
	base := pq.Config{Host: "127.0.0.1", Port: 1, Database: "example", User: "example", SSLMode: pq.SSLModeDisable}
	for _, test := range []struct {
		name   string
		change func(*pq.Config)
	}{
		{"host", func(c *pq.Config) { c.Host = "" }},
		{"port", func(c *pq.Config) { c.Port = 0 }},
		{"database", func(c *pq.Config) { c.Database = "" }},
		{"user", func(c *pq.Config) { c.User = "" }},
		{"TLS", func(c *pq.Config) { c.SSLMode = "" }},
		{"passfile", func(c *pq.Config) { c.Passfile = "example" }},
		{"service", func(c *pq.Config) { c.Service = "example" }},
		{"service file", func(c *pq.Config) { c.ServiceFile = "example" }},
		{"options", func(c *pq.Config) { c.Options = "-c role=detent_migration" }},
		{"tenant setting", func(c *pq.Config) { c.Runtime = map[string]string{"detent.organization_id": "example"} }},
		{"role setting", func(c *pq.Config) { c.Runtime = map[string]string{"role": "detent_platform"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := base.Clone()
			test.change(&connection)
			store, err := openDatabase(t.Context(), startupConfig(connection))
			if store != nil || !errors.Is(err, postgres.ErrConnectionConfig) {
				t.Fatalf("open = %v, %v", store, err)
			}
		})
	}
}

func TestPostgresStartupRejectsUnsafeRoles(t *testing.T) {
	for _, test := range []struct {
		name       string
		group      string
		attributes string
	}{
		{"migration", "detent_migration", ""},
		{"platform", "detent_platform", ""},
		{"ungranted", "", ""},
		{"superuser", "detent_runtime", "SUPERUSER"},
		{"bypass RLS", "detent_runtime", "BYPASSRLS"},
		{"database administrator", "detent_runtime", "CREATEDB"},
		{"replication", "detent_runtime", "REPLICATION"},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := postgresStartupTemplate.Database(t)
			connection := startupConnection(t, admin, test.group, test.attributes)
			service, err := Open(t.Context(), startupConfig(connection))
			if service != nil || !errors.Is(err, postgres.ErrRuntimeRole) {
				t.Fatalf("open = %v, %v", service, err)
			}
			assertStartupDisconnected(t, admin, connection)
		})
	}
}

func TestPostgresStartupOwnsPoolWithoutSQLiteOrWorkers(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime", "")
	cfg := startupConfig(connection)
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if store.db != nil || store.reader != nil || store.authReader != nil || store.claimReader != nil || store.lock != nil || store.path != "" {
		t.Fatal("PostgreSQL startup created SQLite resources")
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedStartupOrganization(t, store.postgres, organization)
		err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "example", Name: "Example", Slug: "example", Profile: "native", WorkflowSource: "example"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	for range 2 {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertStartupDisconnected(t, admin, connection)
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open PostgreSQL Hub = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.db != nil || service.database.reader != nil || service.config.TenantRepository != service.database.postgres || service.config.AccessRepository != service.database.postgres || service.config.BillingRepository != service.database.postgres {
		t.Fatal("PostgreSQL service did not bind all selected storage owners or created SQLite resources")
	}
	if response, status := service.readInstanceHealth(t.Context()); status != 200 || !response.SchemaFactsComplete || response.SchemaVersion == 0 || response.SchemaFingerprint == "" {
		t.Fatalf("PostgreSQL health = %+v, %d", response, status)
	}
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
	response := httptest.NewRecorder()
	service.echo.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated PostgreSQL health status = %d body=%s", response.Code, response.Body.String())
	}
	legacy := httptest.NewRecorder()
	service.echo.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/api/v1/work-items", nil))
	if legacy.Code != http.StatusServiceUnavailable {
		t.Fatalf("legacy SQLite route status = %d, want explicit unavailable response", legacy.Code)
	}
	var leases, attempts int
	if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM leases), (SELECT count(*) FROM native_attempts)`).Scan(&leases, &attempts); err != nil || leases != 0 || attempts != 0 {
		t.Fatalf("failed startup dispatched: leases=%d attempts=%d error=%v", leases, attempts, err)
	}
}

func TestPostgresSharedEntryHealthAndOwnerBootstrap(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime", "")
	provider := newHostedSecurityProvider()
	key := hostedSharedKey(61)
	cfg := startupConfig(connection)
	cfg.Hosted.Provider = provider
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.CreateMembership(t.Context(), "user_owner", "provider_org_startup", "owner"); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open PostgreSQL shared-entry Hub = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	signedCall := func(path, body, subject, email string) *httptest.ResponseRecorder {
		t.Helper()
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		claims := cloudassert.Claims{Issuer: "entry", Audience: "org_startup", Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest([]byte(body)), IssuedAt: now, ExpiresAt: now.Add(30 * time.Second), ID: id, Subject: subject, Email: email}
		assertion, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set(cloudassert.Header, assertion)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		service.echo.ServeHTTP(response, request)
		return response
	}
	health := signedCall("/internal/v1/health", "{}", "", "")
	var healthResponse MigrationHealth
	if err := json.NewDecoder(health.Body).Decode(&healthResponse); err != nil {
		t.Fatal(err)
	}
	if health.Code != http.StatusOK || !healthResponse.SchemaFactsComplete || healthResponse.SchemaVersion == 0 || healthResponse.SchemaFingerprint == "" {
		t.Fatalf("signed PostgreSQL shared health = %d %+v", health.Code, healthResponse)
	}
	bootstrapBody := `{"organization_name":"Example owner organization"}`
	bootstrap := signedCall("/internal/v1/owner/bootstrap", bootstrapBody, "user_owner", "owner@example.test")
	if bootstrap.Code != http.StatusNoContent {
		t.Fatalf("signed PostgreSQL owner bootstrap = %d %s", bootstrap.Code, bootstrap.Body.String())
	}
	replay := signedCall("/internal/v1/owner/bootstrap", bootstrapBody, "user_owner", "owner@example.test")
	if replay.Code != http.StatusNoContent {
		t.Fatalf("signed PostgreSQL owner bootstrap replay = %d %s", replay.Code, replay.Body.String())
	}
	var member accessstore.Member
	err = service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), "user_owner")
		if err != nil {
			return err
		}
		organization, err := r.AccessOrganization(t.Context())
		if err != nil {
			return err
		}
		if organization.Name != "Example owner organization" {
			return fmt.Errorf("organization name = %q", organization.Name)
		}
		return nil
	})
	if err != nil || !member.Active || member.Role != "owner" || member.MembershipID != "membership_user_owner" {
		t.Fatalf("PostgreSQL owner member = %+v, error = %v", member, err)
	}
	if _, err := provider.CreateMembership(t.Context(), "user_member", "provider_org_startup", "member"); err != nil {
		t.Fatal(err)
	}
	denied := signedCall("/internal/v1/owner/bootstrap", bootstrapBody, "user_member", "member@example.test")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("non-owner PostgreSQL bootstrap = %d %s", denied.Code, denied.Body.String())
	}
}

func TestPostgresStartupRejectsSchemaDrift(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
	}{
		{"missing migration", `DELETE FROM public.goose_db_version WHERE version_id = (SELECT max(version_id) FROM public.goose_db_version)`},
		{"newer schema", `INSERT INTO public.goose_db_version(version_id, is_applied) VALUES (99999999999999, true)`},
		{"disabled RLS", `ALTER TABLE public.projects DISABLE ROW LEVEL SECURITY`},
		{"unforced RLS", `ALTER TABLE public.projects NO FORCE ROW LEVEL SECURITY`},
		{"missing runtime grant", `REVOKE SELECT ON public.projects FROM detent_runtime`},
		{"schema creation", `GRANT CREATE ON SCHEMA public TO detent_runtime`},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := postgresStartupTemplate.Database(t)
			connection := startupConnection(t, admin, "detent_runtime", "")
			if _, err := admin.ExecContext(t.Context(), test.statement); err != nil {
				t.Fatal(err)
			}
			store, err := openDatabase(t.Context(), startupConfig(connection))
			if store != nil || err == nil {
				t.Fatalf("open drifted schema = %v, %v", store, err)
			}
			assertStartupDisconnected(t, admin, connection)
		})
	}
}

func TestPostgresStartupRejectsSessionDefaults(t *testing.T) {
	for _, setting := range []string{
		`row_security = off`,
		`"detent.organization_id" = 'org_first'`,
	} {
		t.Run(setting, func(t *testing.T) {
			admin := postgresStartupTemplate.Database(t)
			connection := startupConnection(t, admin, "detent_runtime", "")
			if _, err := admin.ExecContext(t.Context(), "ALTER ROLE "+pq.QuoteIdentifier(connection.User)+" SET "+setting); err != nil {
				t.Fatal(err)
			}
			service, err := Open(t.Context(), startupConfig(connection))
			if service != nil || !errors.Is(err, postgres.ErrRuntimeRole) {
				t.Fatalf("unsafe session default returned service=%v error=%v", service, err)
			}
			assertStartupDisconnected(t, admin, connection)
		})
	}
}

func TestDatabaseStartupCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, cfg := range []Config{
		{DatabasePath: filepath.Join(t.TempDir(), "unopened", "hub.db")},
		startupConfig(pq.Config{}),
	} {
		store, err := openDatabase(ctx, cfg.normalized())
		if store != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled startup = %v, %v", store, err)
		}
	}
}

func TestPostgresMigrationAuthorityIsSeparate(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	runtime := startupConnection(t, admin, "detent_runtime", "")
	migration := startupConnection(t, admin, "detent_migration", "")
	provider, err := startupMigrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	store, err := openDatabase(t.Context(), startupConfig(runtime))
	if store != nil || err == nil {
		t.Fatalf("unmigrated runtime = %v, %v", store, err)
	}
	if err := MigratePostgres(t.Context(), runtime); err == nil {
		t.Fatal("runtime migrated schema")
	}
	if _, err := admin.ExecContext(t.Context(), "GRANT detent_runtime TO "+pq.QuoteIdentifier(migration.User)); err != nil {
		t.Fatal(err)
	}
	if err := MigratePostgres(t.Context(), migration); !errors.Is(err, postgres.ErrMigrationRole) {
		t.Fatalf("combined migration authority = %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), "REVOKE detent_runtime FROM "+pq.QuoteIdentifier(migration.User)); err != nil {
		t.Fatal(err)
	}
	if err := MigratePostgres(t.Context(), migration); err != nil {
		t.Fatal(err)
	}
	if err := MigratePostgres(t.Context(), migration); err != nil {
		t.Fatalf("reapply migrated deployment: %v", err)
	}
	assertStartupDisconnected(t, admin, migration)
	store, err = openDatabase(t.Context(), startupConfig(runtime))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	assertStartupDisconnected(t, admin, runtime)
}

func TestPostgresStartupCancelsSchemaWait(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime", "")
	observer := postgresStartupTemplate.OpenRole(t, admin, connection.User, connection.Password)
	lock, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(t.Context(), `LOCK TABLE public.goose_db_version IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		store, err := openDatabase(ctx, startupConfig(connection))
		if store != nil {
			err = errors.Join(errors.New("canceled startup returned a database"), store.Close())
		}
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		lock.Rollback()
		<-done
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		if err := observer.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE usename = current_user AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("startup has not reached the schema lock")
		}
		return nil
	})
	cancel()
	err = <-done
	done <- err
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel schema wait = %v", err)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertStartupDisconnected(t, admin, connection)
}
