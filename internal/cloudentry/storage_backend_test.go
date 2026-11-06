package cloudentry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/pgtransport"

	"github.com/google/uuid"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var entryPostgresTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := os.ReadFile("../hubserver/postgres/roles.sql")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET ROLE "+postgres.MigrationRoleName); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(os.DirFS("../hubserver/postgres/migrations")), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func entryPostgresDatabase(t *testing.T) *sql.DB {
	t.Helper()
	admin := entryPostgresTemplate.Database(t)
	var name string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" CONNECTION LIMIT 6"); err != nil {
		t.Fatal(err)
	}
	return admin
}

func entryPostgresConfig(t *testing.T, databases ...*sql.DB) func(*Config) {
	t.Helper()
	return entryPostgresStorageConfig(t, false, databases...)
}

func entryPostgresRestartConfig(t *testing.T) func(*Config) {
	t.Helper()
	return entryPostgresStorageConfig(t, true)
}

func entryPostgresStorageConfig(t *testing.T, restart bool, databases ...*sql.DB) func(*Config) {
	t.Helper()
	var admin *sql.DB
	if len(databases) == 0 {
		admin = entryPostgresDatabase(t)
	} else {
		admin = databases[0]
	}
	name := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" IN ROLE "+postgres.PlatformRoleName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("missing process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	objects := newSpacesFixture(t, false)
	return func(cfg *Config) {
		cfg.Storage = StorageConfig{Backend: "postgres", Pool: platformstore.PoolConfig{MaxConnections: 5, ServingConnections: 3, BackgroundConnections: 1}}
		if restart {
			native := entryPostgresTemplate.NativeConfig(t, admin, name, password)
			cfg.Storage.PostgresDSN = native.ConnString()
		} else {
			native := entryPostgresTemplate.NativeConfig(t, admin, name, password)
			native.MaxConns = int32(cfg.Storage.Pool.MaxConnections)
			var err error
			cfg.Storage.PostgresPool, err = pgxpool.NewWithConfig(t.Context(), native)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cfg.Storage.PostgresPool.Close)
		}
		if cfg.Attachments == nil {
			config := objects.config()
			config.Endpoint = "https://storage.example.test"
			cfg.Attachments, cfg.attachmentTransport = &config, objects.transport
		}
	}
}

func openTestRegistry(t *testing.T) *Registry {
	t.Helper()
	var logs lockedBuffer
	cfg := secretEntryConfig(t, &logs)
	cfg.now = time.Now
	entryPostgresConfig(t)(&cfg)
	keys, err := hubsecrets.FromEnvironment(cfg.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	registry, storage, err := openEntryStores(t.Context(), cfg, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(registry.Close(), storage.Close()); err != nil {
			t.Error(err)
		}
	})
	return registry
}

func newPostgresTenantEntryFixture(t *testing.T, configureEntry ...func(*Config)) entryFixture {
	t.Helper()
	return newEntryFixtureWithConfig(t, slog.New(slog.DiscardHandler), configureEntry)
}

func configurePostgresTenant(t *testing.T, cfg *hubserver.Config, databases ...*sql.DB) *sql.DB {
	t.Helper()
	keys, err := hubsecrets.FromEnvironment(entrySecretEnvironment(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys = keys
	var admin *sql.DB
	if len(databases) == 0 {
		admin = entryPostgresDatabase(t)
	} else {
		admin = databases[0]
	}
	name, password := "example_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" IN ROLE "+postgres.RuntimeRoleName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	native := entryPostgresTemplate.NativeConfig(t, admin, name, password)
	native.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	now := time.Now().UTC()
	cursorEnvelope, err := keys.Seal(bytes.Repeat([]byte{1}, 32), accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_" + strings.ReplaceAll(uuid.NewString(), "-", ""), CursorKey: bytes.Repeat([]byte{1}, 32)}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, AllocationGeneration: 1}); err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: cursorEnvelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursorEnvelope.Version, RecordedAt: now}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	connection := entryPostgresTemplate.Connection(t, admin, name, password)
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&connection.Database); err != nil {
		t.Fatal(err)
	}
	connection = pgtransport.Connection{Host: connection.Host, Port: connection.Port, Database: connection.Database, SSLMode: "disable", User: name, Password: password}
	if connection.Host == "localhost" {
		connection.Host = "127.0.0.1"
	}
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.DatabasePath = ""
	cfg.Postgres = &postgres.Config{Connection: connection, Pool: postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1}}
	cfg.RunDetails = tenantDetailFixture{&conversationArchiveFixture{}}
	return admin
}

func tenantPostgresRepository(t *testing.T, fixture tenantFixture) *postgres.Repository {
	t.Helper()
	native, err := fixture.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	native.MaxConns = int32(fixture.config.Postgres.Pool.MaxConnections)
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, fixture.config.Postgres.Pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository
}

func configurePostgresTenants(t *testing.T, databases map[string]*sql.DB) func(*hubserver.Config) {
	t.Helper()
	configurations := make(map[string]hubserver.Config)
	return func(cfg *hubserver.Config) {
		previous, ok := configurations[cfg.Hosted.OrganizationID]
		if !ok {
			databases[cfg.Hosted.OrganizationID] = configurePostgresTenant(t, cfg)
			configurations[cfg.Hosted.OrganizationID] = *cfg
			return
		}
		cfg.DatabaseBackend, cfg.Postgres = previous.DatabaseBackend, previous.Postgres
		cfg.SecretKeys, cfg.RunDetails = previous.SecretKeys, previous.RunDetails
	}
}

type tenantDetailFixture struct {
	*conversationArchiveFixture
}

func (f tenantDetailFixture) StoreDetail(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	return f.PublishContent(ctx, organization, content)
}

func (f tenantDetailFixture) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return f.ReadContent(ctx, organization, ref)
}

func TestEntryBackendPlatformOwners(t *testing.T) {
	for _, backend := range []string{"postgres"} {
		t.Run(backend, func(t *testing.T) {
			configure := entryBackendConfig(t, backend)
			objects := newSpacesFixture(t, false)
			f := newEntryFixture(t, configure, func(cfg *Config) {
				cfg.LookupEnv = entrySecretEnvironment(1, 1)
				storage := objects.config()
				storage.Endpoint = "https://storage.example.test"
				cfg.Attachments, cfg.attachmentTransport = &storage, objects.transport
			})
			s := f.service
			for _, id := range []string{"org_alpha", "org_beta"} {
				organization, err := s.registry.Organization(t.Context(), id)
				if err != nil || organization.Slug == "" {
					t.Fatalf("registered organization %s: %+v %v", id, organization, err)
				}
				identity := auth.HostedIdentity{Subject: "user_alice", OrganizationID: organization.ProviderID, SessionID: "session_" + id, CreatedAt: s.config.now(), ExpiresAt: s.config.now().Add(time.Hour)}
				if err := s.auth.createSession(t.Context(), "hash_"+id, "csrf_"+id, auth.Identity{Subject: identity.Subject, Email: "alice@example.test", Hosted: &identity}); err != nil {
					t.Fatal(err)
				}
				session, err := s.auth.session(t.Context(), "hash_"+id)
				if err != nil {
					t.Fatal(err)
				}
				tokens := auth.HostedTokens{AccessToken: "access_" + id, RefreshToken: "refresh_" + id}
				if _, _, err := s.auth.authorize(t.Context(), session, id, identity, tokens, session.Email); err != nil {
					t.Fatal(err)
				}
				for _, source := range []*store{s.auth.store, s.registry.store} {
					tx, err := source.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := enqueuePlatformNotification(t.Context(), tx, "organization", id, session.Email, organization.Name, "Free", formatTime(s.config.now())); err != nil {
						tx.Rollback(t.Context())
						t.Fatal(err)
					}
					if err := tx.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
					var count int
					queueQuery := "SELECT count(*) FROM platform.platform_notifications WHERE entity_id=$1"
					if source.db.auth {
						queueQuery = "SELECT count(*) FROM platform.auth_notifications WHERE entity_id=$1"
					}
					if err := source.reader.QueryRowContext(t.Context(), queueQuery, id).Scan(&count); err != nil || count != 1 {
						t.Fatalf("notification queue: %d %v", count, err)
					}
				}
			}
			if status := s.AuthSecretStatus(t.Context()); status.Problem != "" || len(status.Problems) != 0 {
				t.Fatalf("secret status: %+v", status)
			}
			session, err := s.auth.session(t.Context(), "hash_org_alpha")
			if err != nil {
				t.Fatal(err)
			}
			metadata := mutation.Metadata{OrganizationID: "org_alpha", RetryIdentity: "example_retry", InputHash: "example_hash"}
			out := operatoradmin.Output{ResourceID: "example_resource", URL: "https://example.test/resource", Data: json.RawMessage(`{"role":"viewer"}`)}
			if err := s.recordAdministrationReceipt(t.Context(), session, metadata, "succeeded", out); err != nil {
				t.Fatal(err)
			}
			replayed, found, err := s.administrationReceipt(t.Context(), session, metadata)
			if err != nil || !found || replayed.ResourceID != out.ResourceID || replayed.URL != out.URL || string(replayed.Data) != string(out.Data) {
				t.Fatalf("receipt replay: %+v %t %v", replayed, found, err)
			}
			metadata.InputHash = "changed_hash"
			if _, _, err := s.administrationReceipt(t.Context(), session, metadata); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("changed receipt input: %v", err)
			}
			if _, err := s.readPlatformAudit(t.Context(), platformAuditFilter{Limit: 20}); err != nil {
				t.Fatal(err)
			}
			config := s.config
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			configure(&config)
			reopened := openSecretEntry(t, config)
			for _, id := range []string{"org_alpha", "org_beta"} {
				session, err := reopened.auth.session(t.Context(), "hash_"+id)
				if err != nil {
					t.Fatal(err)
				}
				item, err := reopened.auth.authorization(t.Context(), session, id)
				if err != nil {
					t.Fatal(err)
				}
				tokens, err := reopened.auth.tokens(item)
				if err != nil || tokens.AccessToken != "access_"+id || tokens.RefreshToken != "refresh_"+id {
					t.Fatalf("restarted tokens: %+v %v", tokens, err)
				}
				if _, err := reopened.auth.store.db.ExecContext(t.Context(), "UPDATE platform.sessions SET expires_at=$1 WHERE token_hash=$2", formatTime(reopened.config.now().Add(-time.Hour)), session.Hash); err != nil {
					t.Fatal(err)
				}
				if _, err := reopened.auth.authorizationByBinding(t.Context(), item.Binding); backend == "postgres" && !errors.Is(err, errNoSession) {
					t.Fatalf("expired account session retained authorization: %v", err)
				}

			}
		})
	}
}

func TestEntryPostgresLogin(t *testing.T) {
	f := newEntryFixture(t, entryPostgresConfig(t))
	b := newBrowser(t, f.service.Handler())
	if location := b.login("/organizations/org_alpha/organization", "user_alice:porg_alpha"); location != "/organizations/org_alpha/organization" {
		t.Fatalf("login location = %q", location)
	}
	for _, name := range []string{"registry.db", "auth.db"} {
		if _, err := os.Stat(f.service.config.StateDir + "/" + name); !os.IsNotExist(err) {
			t.Fatalf("PostgreSQL entry created local store %s: %v", name, err)
		}
	}
}

func entryBackendConfig(t *testing.T, backend string) func(*Config) {
	t.Helper()
	return entryPostgresConfig(t)
}

func TestEntryStorageSelectionFailsClosed(t *testing.T) {
	for _, selection := range []StorageConfig{{}, {Backend: "sqlite"}, {Backend: "other"}, {Backend: "postgres"}, {PostgresDSN: "postgres://example.invalid/example"}, {Backend: "postgres", PostgresDSN: "postgres://example.invalid/example", Pool: platformstore.PoolConfig{MaxConnections: 2, ServingConnections: 2, BackgroundConnections: 1}}} {
		t.Run(selection.Backend, func(t *testing.T) {
			var logs lockedBuffer
			config := secretEntryConfig(t, &logs)
			config.Storage = selection
			if service, err := Open(t.Context(), config); err == nil || service != nil {
				t.Fatal("invalid storage selection opened an entry service")
			}
			for _, name := range []string{"auth.db", "registry.db"} {
				if _, err := os.Stat(config.StateDir + "/" + name); !os.IsNotExist(err) {
					t.Fatalf("invalid selection opened %s: %v", name, err)
				}
			}
		})
	}
}

func TestEntryDSNRejectsAmbientCredentialDiscovery(t *testing.T) {
	cfg := Config{SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))}
	entryPostgresRestartConfig(t)(&cfg)
	t.Setenv("PGPASSWORD", "example-untrusted")
	registry, authStore, err := openEntryStores(t.Context(), cfg, nil)
	if registry != nil {
		if closeErr := registry.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if authStore != nil {
		if closeErr := authStore.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, pgtransport.ErrConnectionConfig) {
		t.Fatalf("ambient DSN authority=%v", err)
	}
}

func TestEntryAuthStoreFailures(t *testing.T) {
	f := newEntryFixture(t)
	b := newBrowser(t, f.service.Handler())
	b.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	a := f.service.auth
	var hash, binding string
	if err := a.store.reader.QueryRowContext(t.Context(), "SELECT session_hash,binding FROM platform.authorizations WHERE organization_id='org_alpha'").Scan(&hash, &binding); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		read func(string, string) error
	}{
		{"session", func(hash, _ string) error { _, err := a.session(t.Context(), hash); return err }},
		{"authorization", func(hash, _ string) error {
			_, err := a.authorization(t.Context(), accountSession{Hash: hash, Subject: "user_alice"}, "org_alpha")
			return err
		}},
		{"binding", func(_, binding string) error { _, err := a.authorizationByBinding(t.Context(), binding); return err }},
		{"token rotation", func(_, binding string) error { return a.storeTokens(t.Context(), binding, auth.HostedTokens{}) }},
		{"support transaction", func(hash, _ string) error { _, err := a.consumeSupportTransaction(t.Context(), hash); return err }},
	}
	for _, test := range cases {
		t.Run(test.name+" missing", func(t *testing.T) {
			if err := test.read("missing_session", "missing_binding"); !errors.Is(err, errNoSession) {
				t.Fatalf("missing credential = %v, want no session", err)
			}
		})
	}
	var err error
	a.keys, err = hubsecrets.FromEnvironment(entrySecretEnvironment(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.sealToken("provider-access", binding, "example_access")
	if err != nil {
		t.Fatal(err)
	}
	a.keys, err = hubsecrets.FromEnvironment(entrySecretEnvironment(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.openToken("provider-access", binding, sealed); !errors.Is(err, hubsecrets.ErrUnavailable) {
		t.Errorf("missing decryption key = %v, want unavailable", err)
	}
	f.service.config.Storage.PostgresPool.Close()
	for _, test := range cases {
		t.Run(test.name+" outage", func(t *testing.T) {
			if err := test.read(hash, binding); err == nil || errors.Is(err, errNoSession) {
				t.Fatalf("storage outage = %v, want operational error", err)
			}
		})
	}
}
