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
	"sync"
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

type dispatchHostedProvider struct {
	auth.HostedProvider
	member auth.Membership
}

func (p dispatchHostedProvider) Memberships(context.Context, string, string) ([]auth.Membership, error) {
	member := p.member
	member.Role.Slug = "owner"
	return []auth.Membership{member}, nil
}

var dispatchPostgresFixtures = make(chan struct{}, 4)
var dispatchPostgresAdmins sync.Map

func DispatchPostgresDatabase(t *testing.T) *sql.DB {
	t.Helper()
	admin := dispatchPostgresTemplate.Database(t)
	connection := dispatchPostgresTemplate.Connection(t, admin, "", "")
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT 16"); err != nil {
		t.Fatal(err)
	}
	dispatchPostgresAdmins.Store(connection.Database, admin)
	t.Cleanup(func() { dispatchPostgresAdmins.Delete(connection.Database) })
	return admin
}

func DispatchPostgresOrganizationConfig(t *testing.T, cfg hubserver.Config, admin *sql.DB) hubserver.Config {
	t.Helper()
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg.InitialAdminToken = []byte(token)
	organization := "org_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	return dispatchPostgresConfig(t, cfg, admin, organization)
}

func DispatchPostgresConfig(t *testing.T, cfg hubserver.Config) hubserver.Config {
	t.Helper()
	return dispatchPostgresConfig(t, cfg, nil, "org_example")
}

func dispatchPostgresConfig(t *testing.T, cfg hubserver.Config, admin *sql.DB, organization string) hubserver.Config {
	t.Helper()
	select {
	case dispatchPostgresFixtures <- struct{}{}:
		t.Cleanup(func() { <-dispatchPostgresFixtures })
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if admin == nil {
		admin = dispatchPostgresTemplate.Database(t)
		connection := dispatchPostgresTemplate.Connection(t, admin, "", "")
		if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT 6"); err != nil {
			t.Fatal(err)
		}
		dispatchPostgresAdmins.Store(connection.Database, admin)
		t.Cleanup(func() { dispatchPostgresAdmins.Delete(connection.Database) })
	}
	connection := dispatchPostgresTemplate.Connection(t, admin, "", "")
	provider, user, membership, principal, slug, hubID := "provider_example", "user_example", "membership_example", "example_admin", "example", "hub_"+strings.Repeat("a", 32)
	email := "owner@example.test"
	if organization != "org_example" {
		provider, user, membership, principal, slug, hubID = "provider_"+organization, "user_"+organization, "membership_"+organization, "admin_"+organization, organization, "hub_"+strings.TrimPrefix(organization, "org_")
		email = "owner+" + organization + "@example.test"
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
	cfg.Hosted = &hubserver.HostedConfig{OrganizationID: organization, WorkOSOrganizationID: provider, PublicURL: "https://hub.example.test", Provider: dispatchHostedProvider{member: auth.Membership{ID: membership, UserID: user, OrganizationID: provider, Status: "active"}}}
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
	envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x5a}, 32), accessstore.SecretAAD(tracker.OrganizationID(organization), "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: tracker.OrganizationID(organization), Name: "Example", Slug: slug, CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: tracker.OrganizationID(organization), ProviderID: provider, AllocationGeneration: 1}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: tracker.OrganizationID(organization), ID: hubID, CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: tracker.OrganizationID(organization), Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: tracker.OrganizationID(organization), Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(organization), ID: principal, Name: "Example administrator", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "operator", KeyScope: "admin", ProjectAccess: "all", Kind: "personal", UserID: user, MembershipID: membership, NativeOnly: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: tracker.OrganizationID(organization), UserID: user, Email: email, MembershipID: membership, Role: "owner", Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now})
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
			admin, ok := dispatchPostgresAdmins.Load(cfg.Postgres.Connection.Database)
			if !ok {
				t.Fatal("PostgreSQL fixture owner is unavailable")
			}
			database, ok := admin.(*sql.DB)
			if !ok {
				t.Fatal("PostgreSQL fixture owner has an invalid type")
			}
			if _, err := database.ExecContext(t.Context(), "ANALYZE issues,search_documents,search_postings,search_scope_documents,native_board_items,native_work_item_pages"); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("search projection exceeded fixture work bound")
}
