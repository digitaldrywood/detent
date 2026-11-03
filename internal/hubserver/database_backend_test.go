package hubserver

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
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
	if _, err := db.ExecContext(ctx, `SET ROLE detent_migration_v2`); err != nil {
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
	return goose.NewProvider(goose.DialectPostgres, pool, postgres.RoleMigrations(migrations),
		goose.WithDisableGlobalRegistry(true), goose.WithSlog(discardLogger()))
}

func startupConnection(t *testing.T, admin *sql.DB, group, attributes string) pq.Config {
	t.Helper()
	connection := pq.Config{Host: "127.0.0.1", SSLMode: pq.SSLModeDisable,
		User: "example_" + strings.ReplaceAll(uuid.NewString(), "-", ""), Password: uuid.NewString()}
	var externalPort uint16
	externalHost := ""
	selection := os.Getenv(pgtest.Environment)
	if selection != "" && selection != "binary" {
		parsed, err := url.Parse(selection)
		if err != nil {
			t.Fatal("invalid PostgreSQL test fixture selection")
		}
		port, err := strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil {
			t.Fatal("invalid PostgreSQL test fixture port")
		}
		externalPort = uint16(port)
		externalHost = parsed.Hostname()
		if externalHost == "localhost" {
			externalHost = "127.0.0.1"
		}
		if socket := parsed.Query().Get("host"); socket != "" {
			externalHost = socket
		}
	}
	var serverAddress, dataDirectory string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database(), current_setting('port')::integer,
		COALESCE(inet_server_addr()::text, current_setting('unix_socket_directories')),
		current_setting('data_directory')`).Scan(&connection.Database, &connection.Port, &serverAddress, &dataDirectory); err != nil {
		t.Fatal(err)
	}
	if externalPort != 0 {
		connection.Host = externalHost
		connection.Port = externalPort
	} else if serverAddress == "." {
		connection.Host = dataDirectory
	} else {
		connection.Host = serverAddress
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
		Pool:       postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1},
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
		{"options", func(c *pq.Config) { c.Options = "-c role=detent_migration_v2" }},
		{"tenant setting", func(c *pq.Config) { c.Runtime = map[string]string{"detent.organization_id": "example"} }},
		{"role setting", func(c *pq.Config) { c.Runtime = map[string]string{"role": "detent_platform_v2"} }},
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
		{"migration", "detent_migration_v2", ""},
		{"platform", "detent_platform_v2", ""},
		{"ungranted", "", ""},
		{"superuser", "detent_runtime_v2", "SUPERUSER"},
		{"bypass RLS", "detent_runtime_v2", "BYPASSRLS"},
		{"database administrator", "detent_runtime_v2", "CREATEDB"},
		{"replication", "detent_runtime_v2", "REPLICATION"},
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
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
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
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	provider := newHostedSecurityProvider()
	key := hostedSharedKey(61)
	cfg := startupConfig(connection)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
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
	t.Run("browser board routes", func(t *testing.T) {
		if service.database.db != nil || service.database.reader != nil {
			t.Fatal("PostgreSQL browser routes retained legacy database handles")
		}
		now := service.config.now().UTC()
		envelope, err := cfg.SecretKeys.Seal([]byte(strings.Repeat("k", 32)), accessstore.SecretAAD("org_startup", "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: "org_startup", Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
				return err
			}
			for _, id := range []tracker.ProjectID{"project_first", "project_second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_startup", ID: id, Name: string(id), Slug: strings.ReplaceAll(string(id), "_", "-"), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_startup", ProjectID: id, NativeID: tracker.NativeWorkItemID("wi_" + string(id)), Number: 1, Title: "Example issue", WorkflowStateName: new("Todo")}); err != nil {
					return err
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: member.UserID, ProjectID: id}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: member.PrincipalID, ProjectID: id}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		browserRequest := func(path string) (*http.Request, cloudassert.Claims) {
			t.Helper()
			id, err := cloudassert.NewID()
			if err != nil {
				t.Fatal(err)
			}
			now := service.config.now().UTC()
			claims := cloudassert.Claims{Issuer: "entry", Audience: "org_startup", Generation: 1, Kind: cloudassert.KindBrowser, Method: http.MethodGet, Path: path, BodyDigest: cloudassert.BodyDigest(nil), IssuedAt: now, ExpiresAt: now.Add(cloudassert.MaxLifetime), ID: id, Subject: "user_owner", Email: "owner@example.test", ProviderOrganization: "provider_org_startup", ProviderSession: "session_owner", SessionCreatedAt: now, SessionExpiresAt: now.Add(time.Hour), Role: "owner", AccessExpiresAt: now.Add(time.Hour), Binding: strings.Repeat("b", 64), CSRF: strings.Repeat("c", 64)}
			assertion, err := cloudassert.Sign(key, claims)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set(cloudassert.Header, assertion)
			return request, claims
		}
		browserRead := func(path string) *httptest.ResponseRecorder {
			t.Helper()
			request, _ := browserRequest(path)
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			return response
		}
		const base = "/api/v2/organizations/org_startup"
		for _, id := range []string{"project-first", "project-second"} {
			for _, suffix := range []string{"", "/issues/1"} {
				response := browserRead(base + "/readable/" + id + suffix)
				requireNativeStatus(t, response, http.StatusOK)
				var resolved map[string]string
				decodeHubResponse(t, response, &resolved)
				if resolved["project_id"] != strings.ReplaceAll(id, "-", "_") || suffix != "" && resolved["work_item_id"] != "wi_"+resolved["project_id"] {
					t.Fatalf("readable browser response = %+v", resolved)
				}
			}
		}
		for _, path := range []string{"/work-views", "/work-view-preference", "/projects/project_first/work-view-preference", "/model-selection", "/slug", "/projects/project_first/slug", "/projects/project_second/slug"} {
			requireNativeStatus(t, browserRead(base+path), http.StatusOK)
		}
		params := url.Values{"project": {"project_first", "project_second"}, "state": {`{"view":"board","tab":"active","q":"","sort":"default","completedWindow":"24h"}`}}
		response := browserRead(base + "/work-board?" + params.Encode())
		requireNativeStatus(t, response, http.StatusOK)
		var boards []workBoardProject
		decodeHubResponse(t, response, &boards)
		if len(boards) != 2 || boards[0].Project.ID != "project_first" || boards[1].Project.ID != "project_second" {
			t.Fatalf("browser board projects = %+v", boards)
		}
		requireNativeStatus(t, browserRead(base+"/readable/missing-project"), http.StatusNotFound)
		requireNativeStatus(t, browserRead("/api/v2/organizations/org_other/readable/project-first"), http.StatusNotFound)
		t.Run("project event stream", func(t *testing.T) {
			if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_startup", ProjectID: "project_first", NativeID: "wi_owned", Number: 2, Title: "Owned example", WorkflowStateName: new("Todo"), ActorPrincipalID: &member.PrincipalID})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, row := range []struct {
				project tracker.ProjectID
				item    tracker.NativeWorkItemID
				number  int
				actor   string
			}{{"project_first", "wi_project_first", 1, ""}, {"project_second", "wi_project_second", 1, ""}, {"project_first", "wi_owned", 2, member.PrincipalID}} {
				card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_startup", ProjectID: row.project, WorkItemID: row.item, Number: row.number, Revision: 1}, Title: "Example issue", State: "Todo", Actor: tracker.Actor{PrincipalID: row.actor}}}
				ref, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), card)
				if err != nil {
					t.Fatal(err)
				}
				record := viewstore.BoardRecord{OrganizationID: "org_startup", ProjectID: row.project, WorkItemID: row.item, SourceRevision: 1, ActorPrincipalID: row.actor, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#%d", row.project, row.number)}, Payload: ref}
				if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: row.project, WorkItemID: row.item, ExpectedRevision: 1, Current: &record})
				}); err != nil {
					t.Fatal(err)
				}
			}
			var retained, reporter *dashboardSubscription
			for _, role := range []string{"owner", "reporter"} {
				request, claims := browserRequest("/organizations/org_startup/projects/project_first/events")
				claims.Role = role
				session, err := service.accessWebSession(t.Context(), claims.Binding, service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				credential, _, err := service.hostedSharedCredential(t.Context(), session, claims.Binding, role)
				if err != nil {
					t.Fatal(err)
				}
				sub := &dashboardSubscription{request: request, claims: claims, scope: nativeScope{organization: "org_startup", project: "project_first", credential: credential}, initial: true, params: url.Values{}, frames: make(chan dashboardFrame, 4), done: make(chan struct{})}
				delivery := newDashboardDelivery(service)
				delivery.config.pageSize = 1
				firstPage := 0
				if role == "reporter" {
					if more, err := service.dashboardPage(t.Context(), delivery, sub); err != nil || more {
						t.Fatalf("reporter initial visibility reset: more=%t error=%v", more, err)
					}
					select {
					case frame := <-sub.frames:
						if frame.event != "reset" || frame.sequence != 2 || strings.Contains(frame.data, "wi_project_first") {
							t.Fatalf("reporter initial visibility reset=%+v", frame)
						}
					default:
						t.Fatal("reporter initial visibility produced no reset")
					}
					sub = &dashboardSubscription{request: request, claims: claims, scope: sub.scope, sequence: 1, params: url.Values{}, frames: make(chan dashboardFrame, 4), done: make(chan struct{})}
					firstPage = 1
				}
				for page := firstPage; page < 2; page++ {
					more, err := service.dashboardPage(t.Context(), delivery, sub)
					if err != nil || more != (page == 0) {
						t.Fatalf("%s replay page %d: more=%t error=%v", role, page, more, err)
					}
					select {
					case frame := <-sub.frames:
						if frame.sequence != int64(page+1) || strings.Contains(frame.data, "wi_project_second") {
							t.Fatalf("%s replay frame=%+v", role, frame)
						}
						if frame.event != "work_item" || page == 1 && !strings.Contains(frame.data, "wi_owned") {
							t.Fatalf("%s issue invalidation=%+v", role, frame)
						}
					default:
						t.Fatal("project replay produced no frame")
					}
				}
				if more, err := service.dashboardPage(t.Context(), delivery, sub); err != nil || more || len(sub.frames) != 0 {
					t.Fatalf("%s replay duplicated a frame: more=%t pending=%d error=%v", role, more, len(sub.frames), err)
				}
				resumed := &dashboardSubscription{request: request, claims: claims, scope: sub.scope, sequence: 2, frames: make(chan dashboardFrame, 4), done: make(chan struct{})}
				if more, err := service.dashboardPage(t.Context(), delivery, resumed); err != nil || more || len(resumed.frames) != 0 || resumed.grantVersion == "" {
					t.Fatalf("%s current-head resume reset or failed to pin access: more=%t pending=%d grants=%q error=%v", role, more, len(resumed.frames), resumed.grantVersion, err)
				}
				boardResume := &dashboardSubscription{request: request, claims: claims, scope: sub.scope, board: true, sequence: 2, params: url.Values{}, frames: make(chan dashboardFrame, 4), done: make(chan struct{})}
				if more, err := service.dashboardPage(t.Context(), delivery, boardResume); err != nil || more {
					t.Fatalf("%s board current-head resume: more=%t error=%v", role, more, err)
				}
				select {
				case raw := <-boardResume.frames:
					var frame tracker.NativeBoardFrame
					if err := json.Unmarshal([]byte(raw.data), &frame); err != nil {
						t.Fatal(err)
					}
					if frame.Gap || frame.Sequence != 2 || frame.GrantVersion == "" {
						t.Fatalf("%s current-head board resume reset=%+v", role, frame)
					}
				default:
					t.Fatal("board resume produced no frame")
				}
				for _, boundary := range []struct {
					name     string
					sequence int64
					grants   string
				}{{"ahead", 3, sub.grantVersion}, {"changed grants", 2, "previous-grants"}} {
					reset := &dashboardSubscription{request: request, claims: claims, scope: sub.scope, sequence: boundary.sequence, grantVersion: boundary.grants, params: url.Values{}, frames: make(chan dashboardFrame, 4), done: make(chan struct{})}
					if more, err := service.dashboardPage(t.Context(), delivery, reset); err != nil || more {
						t.Fatalf("%s %s reset: more=%t error=%v", role, boundary.name, more, err)
					}
					select {
					case frame := <-reset.frames:
						if frame.event != "reset" || frame.sequence != 2 || !frame.terminal {
							t.Fatalf("%s %s reset frame=%+v", role, boundary.name, frame)
						}
					default:
						t.Fatalf("%s %s produced no reset", role, boundary.name)
					}
				}
				if role == "owner" {
					retained = sub
				} else {
					reporter = sub
				}
			}
			requireNativeStatus(t, browserRead("/organizations/org_startup/projects/project_first/events?work_item=wi_missing"), http.StatusNotFound)
			requireNativeStatus(t, browserRead("/organizations/org_startup/projects/project_first/events?workspace=missing"), http.StatusServiceUnavailable)
			server := httptest.NewServer(service.Handler())
			t.Cleanup(server.Close)
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			request, _ := browserRequest("/organizations/org_startup/projects/project_first/events")
			address, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			request.URL.Scheme, request.URL.Host, request.RequestURI = address.Scheme, address.Host, ""
			response, err := (&http.Client{Transport: transport}).Do(request.WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = response.Body.Close() })
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("project stream status=%d content type=%q", response.StatusCode, response.Header.Get("Content-Type"))
			}
			scanner := bufio.NewScanner(response.Body)
			var event string
			received := false
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "event: ") {
					event = strings.TrimPrefix(line, "event: ")
				}
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var change struct {
					EntityID string `json:"entity_id"`
					Kind     string `json:"kind"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &change); err != nil {
					t.Fatal(err)
				}
				if event != "work_item" || change.EntityID != "wi_project_first" || change.Kind != "upsert" {
					t.Fatalf("project stream event=%q change=%+v", event, change)
				}
				cancel()
				received = true
				break
			}
			if !received {
				t.Fatalf("project stream ended before the issue invalidation: %v", scanner.Err())
			}
			setOwner := func(actor string) {
				t.Helper()
				var item corestore.WorkItem
				if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					item, err = w.WorkItem(t.Context(), "project_first", "wi_owned")
					if err != nil {
						return err
					}
					item.ActorPrincipalID = &actor
					item, err = w.UpdateWorkItem(t.Context(), item)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_startup", ProjectID: "project_first", WorkItemID: "wi_owned", Number: 2, Revision: tracker.Revision(item.Revision)}, Title: "Owned example", State: "Todo", Actor: tracker.Actor{PrincipalID: actor}}}
				ref, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), card)
				if err != nil {
					t.Fatal(err)
				}
				record := viewstore.BoardRecord{OrganizationID: "org_startup", ProjectID: "project_first", WorkItemID: "wi_owned", SourceRevision: item.Revision, ActorPrincipalID: actor, Position: viewstore.BoardPosition{State: "Todo", Identifier: "project_first#2"}, Payload: ref}
				if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: "project_first", WorkItemID: "wi_owned", ExpectedRevision: item.Revision, Current: &record})
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertVisibilityReset := func(sub *dashboardSubscription, sequence int64) {
				t.Helper()
				if more, err := service.dashboardPage(t.Context(), newDashboardDelivery(service), sub); err != nil || more {
					t.Fatalf("visibility loss: more=%t error=%v", more, err)
				}
				select {
				case frame := <-sub.frames:
					if frame.event != "reset" || frame.sequence != sequence || !frame.terminal || strings.Contains(frame.data, "wi_owned") {
						t.Fatalf("visibility loss did not invalidate cached data privately: %+v", frame)
					}
				default:
					t.Fatal("visibility loss produced no reset")
				}
			}
			setOwner("another-principal")
			assertVisibilityReset(reporter, 3)
			setOwner(member.PrincipalID)
			deleted := &dashboardSubscription{request: reporter.request, claims: reporter.claims, scope: reporter.scope, sequence: 4, grantVersion: reporter.grantVersion, frames: make(chan dashboardFrame, 4), done: make(chan struct{})}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.DeleteWorkItem(t.Context(), "project_first", "wi_owned"); err != nil {
					return err
				}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: "project_first", WorkItemID: "wi_owned"})
			}); err != nil {
				t.Fatal(err)
			}
			assertVisibilityReset(deleted, 5)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteAccessProjectGrant(t.Context(), member.UserID, "project_first")
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.dashboardPage(t.Context(), newDashboardDelivery(service), retained); !errors.Is(err, accessstore.ErrDenied) || len(retained.frames) != 0 {
				t.Fatalf("revoked project stream published data: pending=%d error=%v", len(retained.frames), err)
			}
		})
	})
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
		{"missing runtime grant", `REVOKE SELECT ON public.projects FROM detent_runtime_v2`},
		{"schema creation", `GRANT CREATE ON SCHEMA public TO detent_runtime_v2`},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := postgresStartupTemplate.Database(t)
			connection := startupConnection(t, admin, "detent_runtime_v2", "")
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
			connection := startupConnection(t, admin, "detent_runtime_v2", "")
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
	runtime := startupConnection(t, admin, "detent_runtime_v2", "")
	migration := startupConnection(t, admin, "detent_migration_v2", "")
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
	if _, err := admin.ExecContext(t.Context(), "GRANT detent_runtime_v2 TO "+pq.QuoteIdentifier(migration.User)); err != nil {
		t.Fatal(err)
	}
	if err := MigratePostgres(t.Context(), migration); !errors.Is(err, postgres.ErrMigrationRole) {
		t.Fatalf("combined migration authority = %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), "REVOKE detent_runtime_v2 FROM "+pq.QuoteIdentifier(migration.User)); err != nil {
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
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
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
