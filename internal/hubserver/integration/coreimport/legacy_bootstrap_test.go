package coreimport_test

import (
	"bytes"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/orgsettings"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresLegacyBootstrapTenants(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	now := time.Now().UTC().Truncate(time.Microsecond)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize))
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	configs := make(map[tracker.OrganizationID]hubserver.Config, len(organizations))
	for _, org := range organizations {
		cfg := hubfixture.StartupConfig()
		cfg.Hosted.OrganizationID = string(org)
		cfg.Hosted.WorkOSOrganizationID = "provider_" + string(org)
		cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{
			Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1,
		}
		cfg.SecretKeys = hubfixture.SecretKeys(t)
		cfg.Clock = func() time.Time { return now }
		cfg.Logger = hubfixture.DiscardLogger()
		cfg.DatabaseBackend = hubserver.DatabasePostgres
		configs[org] = cfg
		deployment, generation := cfg.Hosted.Deployment()
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example " + string(org), Slug: "example-" + strings.TrimPrefix(string(org), "org_"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: cfg.Hosted.WorkOSOrganizationID, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
				return err
			}
			if err := w.UpdateOrganizationLogo(t.Context(), accessstore.Logo{ImageKey: "logo_" + string(org)}, 1); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "viewer", Name: "Example", Hash: apikey.HashToken(string(org)), Scope: "operator", NativeOnly: true, UserID: "user", MembershipID: "membership", ProjectAccess: "selected", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "user", Email: "viewer@example.test", MembershipID: "membership", Role: "viewer", Active: true, PrincipalID: "viewer", CreatedAt: now, UpdatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range organizations {
		org := string(organization)
		t.Run(org, func(t *testing.T) {
			cfg := configs[organization]
			connection := hubtest.PostgresConfig(t, admin, hubtest.WithClock(cfg.Clock))
			cfg.Postgres = &connection
			service, err := hubserver.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			target := "/organizations/" + org + "/app/bootstrap"
			serve := func(audience string) *httptest.ResponseRecorder {
				t.Helper()
				id, err := cloudassert.NewID()
				if err != nil {
					t.Fatal(err)
				}
				claims := cloudassert.Claims{Issuer: "entry", Audience: audience, Generation: 1, Kind: cloudassert.KindBrowser, Method: http.MethodGet, Path: target, BodyDigest: cloudassert.BodyDigest(nil), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id, Subject: "user", Email: "viewer@example.test", ProviderOrganization: "provider_" + audience, ProviderSession: "session", SessionCreatedAt: now, SessionExpiresAt: now.Add(time.Hour), AccessExpiresAt: now.Add(time.Hour), Role: "viewer", Binding: cloudassert.AuthorizationBinding("shared-user", audience, "session"), CSRF: cloudassert.CSRFToken("shared-user", audience)}
				assertion, err := cloudassert.Sign(key, claims)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodGet, target, nil)
				request.Header.Set(cloudassert.Header, assertion)
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				return response
			}
			response := serve(org)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var payload struct {
				Organization struct {
					ID   string            `json:"id"`
					Name string            `json:"name"`
					Logo *orgsettings.Logo `json:"logo"`
				} `json:"organization"`
			}
			hubfixture.Decode(t, response, &payload)
			if payload.Organization.ID != org || payload.Organization.Name != "Example "+org || payload.Organization.Logo == nil || payload.Organization.Logo.ImageKey != "logo_"+org {
				t.Fatalf("bootstrap=%+v", payload.Organization)
			}
			foreign := "org_first"
			if org == foreign {
				foreign = "org_second"
			}
			hubfixture.RequireStatus(t, serve(foreign), http.StatusUnauthorized)
		})
	}
}
