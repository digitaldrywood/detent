package access_test

import (
	"crypto/ed25519"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryCatalogFixture struct {
	admin         *sql.DB
	repository    *postgres.Repository
	organizations []string
	handlers      map[string]http.Handler
	key           ed25519.PrivateKey
}

func openEntryCatalog(t *testing.T, configure ...func(*hubfixture.Config)) entryCatalogFixture {
	t.Helper()
	public, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	fixture := entryCatalogFixture{
		admin: admin, repository: repository, organizations: []string{"org_startup", "org_other"},
		handlers: make(map[string]http.Handler), key: key,
	}
	keys, details := hubfixture.SecretKeys(t), hubtest.NewDetails()
	for _, org := range fixture.organizations {
		organization := tracker.OrganizationID(org)
		connection := hubtest.PostgresConfig(t, admin)
		cfg := hubfixture.Config{
			DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection,
			TenantRepository: repository, AccessRepository: repository,
			SecretKeys: keys, RunDetails: details, Logger: hubfixture.DiscardLogger(),
			Hosted: &hostedplans.HostedConfig{
				OrganizationID: org, WorkOSOrganizationID: "provider_" + org,
				PublicURL: "https://hub.example.test", Provider: hubfixture.NewHostedProvider(),
				SharedEntry: &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{public}, Generation: 1},
			},
		}
		for _, apply := range configure {
			apply(&cfg)
		}
		deployment, generation := cfg.Hosted.Deployment()
		now, owner := time.Now().UTC(), org+"_owner"
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example " + org, Slug: strings.ReplaceAll(org, "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, Deployment: deployment, AllocationGeneration: generation, PublicURL: cfg.Hosted.PublicURL}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: owner, Name: "Example owner", Hash: owner, Scope: "admin", Kind: "personal", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, UserID: owner, MembershipID: owner, KeyScope: "admin"}); err != nil {
				return err
			}
			return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: owner, Email: owner + "@example.test", MembershipID: owner, Role: "owner", Active: true, PrincipalID: owner, CreatedAt: now, UpdatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		service, err := hubserver.Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		})
		fixture.handlers[org] = service.Handler()
	}
	return fixture
}

func (f entryCatalogFixture) request(t *testing.T, org, path, body string, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
	t.Helper()
	id, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := cloudassert.Claims{Issuer: "entry", Audience: org, Email: org + "_owner@example.test", Subject: org + "_owner", Role: "owner", Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest([]byte(body)), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id}
	mutate(&claims)
	assertion, err := cloudassert.Sign(f.key, claims)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), claims.Method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(cloudassert.Header, assertion)
	response := httptest.NewRecorder()
	f.handlers[org].ServeHTTP(response, request)
	return response
}
