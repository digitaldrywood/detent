package scenariotest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var hubclientPostgresTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `SET synchronous_commit = off`); err != nil {
		return err
	}
	roles, err := os.ReadFile("../../hubserver/postgres/roles.sql")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE ROLE hubclient_test LOGIN; GRANT detent_runtime_v2 TO hubclient_test`); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `SET ROLE detent_migration_v2`); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(os.DirFS("../../hubserver/postgres/migrations")), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

type hubclientIdentityProvider struct{ auth.HostedProvider }

func (hubclientIdentityProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	member := auth.Membership{ID: "membership_example", UserID: user, OrganizationID: organization, Status: "active"}
	member.Role.Slug = "owner"
	return []auth.Membership{member}, nil
}

func ClosePostgres() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return hubclientPostgresTemplate.Close(ctx)
}

func hubclientPostgresConfig(t *testing.T, cfg hubserver.Config, states []tracker.NativeState, requireDependencies bool) hubserver.Config {
	t.Helper()
	admin := hubclientPostgresTemplate.Database(t)
	connection := pgtransport.Connection{User: "hubclient_test", Password: "test-only", SSLMode: "disable"}
	var socket, data string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database(), current_setting('port')::integer, current_setting('unix_socket_directories'), current_setting('data_directory')`).Scan(&connection.Database, &connection.Port, &socket, &data); err != nil {
		t.Fatal(err)
	}
	connection.Host = socket
	if socket == "." {
		connection.Host = data
	}
	if _, err := admin.ExecContext(t.Context(), `ALTER DATABASE `+`"`+connection.Database+`" CONNECTION LIMIT 6`); err != nil {
		t.Fatal(err)
	}
	cfg.DatabasePath = ""
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.Postgres = &postgres.Config{Connection: connection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
	if cfg.Hosted == nil {
		cfg.Hosted = &hubserver.HostedConfig{OrganizationID: "org_example", WorkOSOrganizationID: "provider_example", PublicURL: "https://hub.example.test", Provider: hubclientIdentityProvider{}}
	}
	cfg.Logger = slog.New(slog.NewTextHandler(t.Output(), nil))
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)) + `"}`
		}
		return "1"
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys = keys
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, os.DirFS("../../hubserver/postgres/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	now := cfg.Clock().UTC()
	err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, AllocationGeneration: 1}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x5a}, 32), accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_example", Name: "Example project", Profile: "native", RequireDependencies: requireDependencies}, States: states}); err != nil {
			return err
		}
		for _, credential := range []struct{ id, token, scope string }{{"example_admin", string(cfg.InitialAdminToken), "admin"}, {"example_ci", "example-ci", "operator"}} {
			row := accessstore.Credential{OrganizationID: organization, ID: credential.id, Name: credential.id, Hash: apikey.HashToken(credential.token), Scope: credential.scope, Kind: "personal", KeyScope: "admin", ProjectAccess: "all", UserID: "user_example", MembershipID: "membership_example", CreatedAt: now, UpdatedAt: now}
			if credential.id == "example_ci" {
				row.Kind, row.KeyScope, row.ProjectAccess, row.UserID, row.MembershipID = "service", "write", "selected", "", ""
			}
			if err := w.CreateAccessCredential(t.Context(), row); err != nil {
				return err
			}
			if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credential.id, ProjectID: "prj_example"}); err != nil {
				return err
			}
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_example", MembershipID: "membership_example", Email: "owner@example.test", Role: "owner", Active: true, PrincipalID: "example_admin", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "user_example", ProjectID: "prj_example", CanWrite: true, ManageRunner: true})
	})
	if err := errors.Join(err, repository.Close()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func hubclientSeedPostgres(t *testing.T, cfg hubserver.Config, organization tracker.OrganizationID, seed func(tenantstore.Writer) error) {
	t.Helper()
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, os.DirFS("../../hubserver/postgres/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteTenant(t.Context(), organization, corestore.Serving, seed)
	if err := errors.Join(err, repository.Close()); err != nil {
		t.Fatal(err)
	}
}
