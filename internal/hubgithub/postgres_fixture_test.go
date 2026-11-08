package hubgithub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

var githubTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := os.ReadFile("../hubserver/postgres/roles.sql")
	if err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(os.DirFS("../hubserver/postgres/migrations")), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	status := m.Run()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := githubTemplate.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		status = 1
	}
	os.Exit(status)
}

type githubHostedProvider struct{ auth.HostedProvider }

func (p *githubHostedProvider) Memberships(context.Context, string, string) ([]auth.Membership, error) {
	m := auth.Membership{ID: "member_example", UserID: "user_example", OrganizationID: "provider_example", Status: "active"}
	m.Role.Slug = "owner"
	return []auth.Membership{m}, nil
}

type githubDetails struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (d *githubDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.data == nil {
		d.data = map[string][]byte{}
	}
	d.data[ref.Key] = bytes.Clone(data)
	return ref, nil
}

func (d *githubDetails) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	data, ok := d.data[ref.Key]
	if !ok {
		return nil, errors.New("detail unavailable")
	}
	return bytes.Clone(data), nil
}

func openGitHubService(t *testing.T, cfg hubserver.Config) (*hubserver.Service, error) {
	cfg, err := gitHubServiceConfig(t, cfg)
	if err != nil {
		return nil, err
	}
	return hubserver.Open(t.Context(), cfg)
}

func gitHubServiceConfig(t *testing.T, cfg hubserver.Config) (hubserver.Config, error) {
	t.Helper()
	db := githubTemplate.Database(t)
	role := "example_" + uuid.NewString()
	if _, err := db.ExecContext(t.Context(), `CREATE ROLE "`+role+`" LOGIN IN ROLE detent_runtime_v2`); err != nil {
		t.Fatal(err)
	}
	connection := githubTemplate.Connection(t, db, role, "")
	cfg.DatabaseBackend, cfg.DatabasePath = hubserver.DatabasePostgres, ""
	cfg.Postgres = &postgres.Config{Connection: connection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
	cfg.Hosted = &hubserver.HostedConfig{OrganizationID: "org_example", WorkOSOrganizationID: "provider_example", PublicURL: "http://127.0.0.1:7777", Provider: &githubHostedProvider{}}
	cfg.RunDetails = &githubDetails{}
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		}
		if name == "DETENT_HUB_SECRET_KEY_VERSION" {
			return "1"
		}
		return ""
	})
	if err != nil {
		return hubserver.Config{}, err
	}
	cfg.SecretKeys = keys
	envelope, err := keys.Seal(bytes.Repeat([]byte{0x5a}, 32), accessstore.SecretAAD("org_example", "", accessstore.NativeCursorKeyKind))
	if err != nil {
		return hubserver.Config{}, err
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, os.DirFS("../hubserver/postgres/migrations"))
	if err != nil {
		return hubserver.Config{}, err
	}
	now := time.Now().UTC()
	err = repository.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_example", Name: "Example", Slug: "example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: "org_example", ID: "hub_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_example", ProviderID: "provider_example", AllocationGeneration: 1}); err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_example", Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: "org_example", Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_example", ID: "example_admin", Name: "Example", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), UserID: "user_example", MembershipID: "member_example", Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_example", UserID: "user_example", MembershipID: "member_example", Role: "owner", Active: true, PrincipalID: "example_admin", CreatedAt: now, UpdatedAt: now})
	})
	if err := errors.Join(err, repository.Close()); err != nil {
		return hubserver.Config{}, err
	}
	return cfg, nil
}
