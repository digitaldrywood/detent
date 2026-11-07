package testsupport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeChangeSchema() fs.FS {
	return os.DirFS("../../hubserver/postgres")
}

var nativeChangeTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	schema := nativeChangeSchema()
	roles, err := fs.ReadFile(schema, "roles.sql")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
		return err
	}
	migrations, err := fs.Sub(schema, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(migrations), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func CloseNativeChangePostgres(ctx context.Context) error { return nativeChangeTemplate.Close(ctx) }

func NativeChangeDatabasePath(t testing.TB) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "unused.db")
}

var nativeChangeDatabases sync.Map
var nativeChangeRepositories sync.Map

func NativeChangePostgresConfig(t *testing.T, cfg hubserver.Config) hubserver.Config {
	t.Helper()
	if cfg.Postgres != nil {
		return cfg
	}
	admin := nativeChangeTemplate.Database(t)
	connection := pgtransport.Connection{User: "example_" + uuid.NewString()[:8], Password: uuid.NewString(), SSLMode: "disable"}
	var dataDirectory string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database(), current_setting('port')::integer, current_setting('unix_socket_directories'), current_setting('data_directory')`).Scan(&connection.Database, &connection.Port, &connection.Host, &dataDirectory); err != nil {
		t.Fatal(err)
	}
	if connection.Host == "." {
		connection.Host = dataDirectory
	}
	nativeChangeDatabases.Store(connection.Database, admin)
	t.Cleanup(func() { nativeChangeDatabases.Delete(connection.Database) })
	name := pgx.Identifier{connection.User}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+name+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(connection.Password)+" IN ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT 8"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+name); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+name); err != nil {
			t.Error(err)
		}
	})
	admin.SetMaxIdleConns(0)
	cfg.DatabasePath = ""
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.Postgres = &postgres.Config{Connection: connection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
	if cfg.Hosted == nil {
		provider, err := auth.NewHostedProvider(auth.IdentityProviderWorkOS, auth.WorkOSConfig{APIURL: "https://identity.example.test", ClientID: "client_example", APIKey: "example", RedirectURL: "https://hub.example.test/auth/callback", HTTPClient: &http.Client{Transport: RoundTrip(func(*http.Request) (*http.Response, error) { return nil, auth.ErrHostedIdentity })}})
		if err != nil {
			t.Fatal(err)
		}
		provider = nativeChangeIdentityProvider{HostedProvider: provider}
		cfg.Hosted = &hubserver.HostedConfig{OrganizationID: "org_11111111111111111111111111111111", WorkOSOrganizationID: "provider_example", PublicURL: "https://hub.example.test", Provider: provider}
	}
	migrations, err := fs.Sub(nativeChangeSchema(), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, migrations)
	if err != nil {
		t.Fatal(err)
	}
	nativeChangeRepositories.Store(connection.Database, repository)
	t.Cleanup(func() {
		nativeChangeRepositories.Delete(connection.Database)
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC()
	if cfg.Clock != nil {
		now = cfg.Clock().UTC()
	}
	if cfg.SecretKeys == nil {
		cfg.SecretKeys, err = hubsecrets.FromEnvironment(func(key string) string {
			if key == "DETENT_HUB_SECRET_KEYS" {
				return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)) + `"}`
			}
			return "1"
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{2}, 32), accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, AllocationGeneration: 1}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "test_admin", Name: "Example administrator", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "operator", NativeOnly: true, UserID: "user_example", MembershipID: "membership_example", CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_example", MembershipID: "membership_example", Email: "owner@example.test", Role: "owner", Active: true, PrincipalID: "test_admin", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_" + strings.ReplaceAll(uuid.NewString(), "-", ""), CursorKey: bytes.Repeat([]byte{2}, 32)}); err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func NativeChangeExpireLease(t *testing.T, cfg hubserver.Config, lease tracker.LeaseID) {
	t.Helper()
	value, ok := nativeChangeDatabases.Load(cfg.Postgres.Connection.Database)
	if !ok {
		t.Fatal("fixture database is unavailable")
	}
	database, ok := value.(*sql.DB)
	if !ok {
		t.Fatal("fixture database has an invalid type")
	}
	_, err := database.ExecContext(t.Context(), `UPDATE leases SET acquired_at=clock_timestamp()-interval '3 hours',renewed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE organization_id=$1 AND lease_id=$2`, cfg.Hosted.OrganizationID, lease)
	if err != nil {
		t.Fatal(err)
	}
}

func nativeChangeRepository(cfg hubserver.Config) (*postgres.Repository, error) {
	value, ok := nativeChangeRepositories.Load(cfg.Postgres.Connection.Database)
	if !ok {
		return nil, errors.New("fixture repository is unavailable")
	}
	repository, ok := value.(*postgres.Repository)
	if !ok {
		return nil, errors.New("fixture repository has an invalid type")
	}
	return repository, nil
}
