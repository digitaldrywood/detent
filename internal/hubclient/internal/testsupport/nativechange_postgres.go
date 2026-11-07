package testsupport

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
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
	_, source, _, _ := runtime.Caller(0)
	return os.DirFS(filepath.Join(filepath.Dir(source), "../../../hubserver/postgres"))
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

func NativeChangePostgresConfig(t *testing.T, cfg hubserver.Config) hubserver.Config {
	t.Helper()
	if cfg.Postgres != nil {
		return cfg
	}
	admin := nativeChangeTemplate.Database(t)
	connection := pgtransport.Connection{User: "example_" + uuid.NewString()[:8], Password: uuid.NewString(), SSLMode: "disable"}
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database(), current_setting('port')::integer, current_setting('unix_socket_directories')`).Scan(&connection.Database, &connection.Port, &connection.Host); err != nil {
		t.Fatal(err)
	}
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
		cfg.Hosted = &hubserver.HostedConfig{OrganizationID: "org_example", WorkOSOrganizationID: "provider_example", PublicURL: "https://hub.example.test", Provider: provider}
	}
	migrations, err := fs.Sub(nativeChangeSchema(), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, migrations)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if cfg.Clock != nil {
		now = cfg.Clock().UTC()
	}
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, AllocationGeneration: 1}); err != nil {
			return err
		}
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "test_admin", Name: "Example administrator", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "admin", CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "service"})
	})
	if err := errors.Join(err, repository.Close()); err != nil {
		t.Fatal(err)
	}
	return cfg
}
