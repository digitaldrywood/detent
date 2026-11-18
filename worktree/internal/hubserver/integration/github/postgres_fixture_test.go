package github_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func postgresFiles(t *testing.T) fs.FS {
	t.Helper()
	files, err := postgres.MigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

type githubHostedProvider struct{ auth.HostedProvider }

func (p *githubHostedProvider) Memberships(context.Context, string, string) ([]auth.Membership, error) {
	m := auth.Membership{ID: "member_example", UserID: "user_example", OrganizationID: "provider_example", Status: "active"}
	m.Role.Slug = "owner"
	return []auth.Membership{m}, nil
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
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	config := hubtest.PostgresConfig(t, hubtest.Database(t))
	cfg.Postgres = &config
	cfg.Hosted = &hostedplans.HostedConfig{OrganizationID: "org_example", WorkOSOrganizationID: "provider_example", PublicURL: "http://127.0.0.1:7777", Provider: &githubHostedProvider{}}
	cfg.RunDetails = hubtest.NewDetails()
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
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, postgresFiles(t))
	if err != nil {
		return hubserver.Config{}, err
	}
	deployment, generation := "origin", int64(0)
	if cfg.Hosted.SharedEntry != nil {
		deployment, generation = "shared", cfg.Hosted.SharedEntry.Generation
	}
	now := time.Now().UTC()
	err = repository.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_example", Name: "Example", Slug: "example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: "org_example", ID: "hub_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_example", ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
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
