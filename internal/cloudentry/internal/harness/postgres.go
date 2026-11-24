package harness

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/attachment"
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

func SecretEnvironment(active int, versions ...int) func(string) string {
	values := map[string]string{}
	for _, version := range versions {
		values[strconv.Itoa(version)] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(version)}, 32))
	}
	raw, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return func(name string) string {
		if len(versions) == 0 {
			return ""
		}
		if name == "DETENT_HUB_SECRET_KEYS" {
			return string(raw)
		}
		if name == "DETENT_HUB_SECRET_KEY_VERSION" {
			return strconv.Itoa(active)
		}
		return ""
	}
}

var Postgres = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, postgres.RolesSQL()); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET ROLE "+postgres.MigrationRoleName); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(migrations), goose.WithGoMigrations(postgres.ConversationMigrations(migrations)...), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func PostgresDatabase(t *testing.T) *sql.DB {
	t.Helper()
	return postgresDatabase(t, 6)
}

func SharedPostgresDatabase(t *testing.T) *sql.DB {
	t.Helper()
	return postgresDatabase(t, 32)
}

func postgresDatabase(t *testing.T, connections int) *sql.DB {
	t.Helper()
	admin := Postgres.Database(t)
	var name string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" CONNECTION LIMIT "+strconv.Itoa(connections)); err != nil {
		t.Fatal(err)
	}
	return admin
}

func ConfigurePostgresTenant(t *testing.T, cfg *hubserver.Config, databases ...*sql.DB) *sql.DB {
	t.Helper()
	keys, err := hubsecrets.FromEnvironment(SecretEnvironment(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys = keys
	var admin *sql.DB
	if len(databases) == 0 {
		admin = PostgresDatabase(t)
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
	native := Postgres.NativeConfig(t, admin, name, password)
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
	deployment, generation := "origin", int64(0)
	if cfg.Hosted.SharedEntry != nil {
		deployment, generation = "shared", cfg.Hosted.SharedEntry.Generation
	}
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
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
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
	connection := Postgres.Connection(t, admin, name, password)
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&connection.Database); err != nil {
		t.Fatal(err)
	}
	connection = pgtransport.Connection{Host: connection.Host, Port: connection.Port, Database: connection.Database, SSLMode: "disable", User: name, Password: password}
	if connection.Host == "localhost" {
		connection.Host = "127.0.0.1"
	}
	cfg.DatabaseBackend = hubserver.DatabasePostgres

	cfg.Postgres = &postgres.Config{Connection: connection, Pool: postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1}}
	if cfg.RunDetails == nil {
		cfg.RunDetails = TenantDetails{&ConversationArchive{}}
	}
	return admin
}

type TenantDetails struct {
	attachment.ArchiveContentStore
}

func (f TenantDetails) StoreDetail(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	return f.PublishContent(ctx, organization, content)
}

func (f TenantDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return f.ReadContent(ctx, organization, ref)
}
