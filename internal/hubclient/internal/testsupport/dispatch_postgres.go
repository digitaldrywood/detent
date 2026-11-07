package testsupport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func dispatchPostgresFiles() fs.FS {
	return os.DirFS("../../hubserver/postgres")
}

var dispatchPostgresTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := fs.ReadFile(dispatchPostgresFiles(), "roles.sql")
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
	migrations, err := fs.Sub(dispatchPostgresFiles(), "migrations")
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

type dispatchHostedProvider struct{ auth.HostedProvider }

func (dispatchHostedProvider) Memberships(context.Context, string, string) ([]auth.Membership, error) {
	member := auth.Membership{ID: "membership_example", UserID: "user_example", OrganizationID: "provider_example", Status: "active"}
	member.Role.Slug = "owner"
	return []auth.Membership{member}, nil
}

var dispatchPostgresFixtures = make(chan struct{}, 4)

func DispatchPostgresConfig(t *testing.T, cfg hubserver.Config) hubserver.Config {
	t.Helper()
	select {
	case dispatchPostgresFixtures <- struct{}{}:
		t.Cleanup(func() { <-dispatchPostgresFixtures })
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	admin := dispatchPostgresTemplate.Database(t)
	connection := dispatchPostgresTemplate.Connection(t, admin, "", "")
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT 6"); err != nil {
		t.Fatal(err)
	}
	connection.User = "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	connection.Password = uuid.NewString()
	role := pgx.Identifier{connection.User}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+role+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(connection.Password)+" IN ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	})
	cfg.DatabaseBackend, cfg.DatabasePath = hubserver.DatabasePostgres, ""
	cfg.Postgres = &postgres.Config{Connection: connection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}, LeaseClock: cfg.Clock}
	cfg.Hosted = &hubserver.HostedConfig{OrganizationID: "org_example", WorkOSOrganizationID: "provider_example", PublicURL: "https://hub.example.test", Provider: dispatchHostedProvider{}}
	encoded, err := json.Marshal(map[string]string{"1": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x35}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys, err = hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return string(encoded)
		}
		return "1"
	})
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := fs.Sub(dispatchPostgresFiles(), "migrations")
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
	envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x5a}, 32), accessstore.SecretAAD("org_example", "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_example", Name: "Example", Slug: "example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_example", ProviderID: "provider_example", AllocationGeneration: 1}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: "org_example", ID: "hub_" + strings.Repeat("a", 32), CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_example", Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: "org_example", Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_example", ID: "example_admin", Name: "Example administrator", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "operator", KeyScope: "admin", ProjectAccess: "all", Kind: "personal", UserID: "user_example", MembershipID: "membership_example", NativeOnly: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_example", UserID: "user_example", Email: "owner@example.test", MembershipID: "membership_example", Role: "owner", Active: true, PrincipalID: "example_admin", CreatedAt: now, UpdatedAt: now})
	})
	closeErr := repository.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("seed PostgreSQL fixture: %v, close: %v", err, closeErr)
	}
	return cfg
}

func DispatchWriteFixture(t *testing.T, cfg hubserver.Config, write func(tenantstore.Writer) error) {
	t.Helper()
	migrations, err := fs.Sub(dispatchPostgresFiles(), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, migrations)
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(t.Context(), tracker.OrganizationID(cfg.Hosted.OrganizationID), corestore.Serving, write)
	if err := errors.Join(err, repository.Close()); err != nil {
		t.Fatal(err)
	}
}

func CloseDispatchPostgres(ctx context.Context) error { return dispatchPostgresTemplate.Close(ctx) }

func DispatchRefreshProjections(t *testing.T, cfg hubserver.Config, service *hubserver.Service) {
	t.Helper()
	migrations, err := fs.Sub(dispatchPostgresFiles(), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	}()
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	boardComplete := false
	for range 128 {
		required := false
		err := repository.ReadTenant(t.Context(), organization, corestore.Background, func(r tenantstore.Reader) error {
			candidate, err := r.NextBoardCandidate(t.Context(), "")
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			required = candidate.Required
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if !required {
			boardComplete = true
			break
		}
		ctx := (hubmaintenance.Budget{Items: 1, Bytes: hubserver.BoardMaintenanceBytes}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
		if _, _, err := service.MaintainBoardRecord(ctx, organization, ""); err != nil {
			t.Fatal(err)
		}
	}
	if !boardComplete {
		t.Fatal("board projection exceeded fixture work bound")
	}
	pagesComplete := false
	for range 512 {
		ctx := (hubmaintenance.Budget{Items: 1, Bytes: hubmaintenance.ByteLimit}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
		processed, err := service.MaintainWorkItemPage(ctx, organization)
		if err != nil {
			t.Fatal(err)
		}
		if !processed {
			pagesComplete = true
			break
		}
	}
	if !pagesComplete {
		t.Fatal("work item projection exceeded fixture work bound")
	}
	for range 512 {
		processed, err := searchstore.NewContent(cfg.RunDetails).RefreshOne(t.Context(), repository, tracker.OrganizationID(cfg.Hosted.OrganizationID))
		if err != nil {
			t.Fatal(err)
		}
		if !processed {
			return
		}
	}
	t.Fatal("search projection exceeded fixture work bound")
}
