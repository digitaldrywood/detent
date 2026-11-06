package hubserver

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresLegacyBootstrapTenants(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	key := hostedSharedKey(41)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedStartupOrganization(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.UpdateAccessOrganizationName(t.Context(), "Example "+string(org)); err != nil {
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
	for _, org := range []string{"org_first", "org_second"} {
		t.Run(org, func(t *testing.T) {
			entry := &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
			if err := entry.validate(org); err != nil {
				t.Fatal(err)
			}
			service := &Service{secretRewrapReady: make(chan struct{}), echo: echo.New(), database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, Logger: discardLogger(), now: func() time.Time { return now }, Hosted: &HostedConfig{OrganizationID: org, PublicURL: "https://hub.example.test", SharedEntry: entry}}}
			service.echo.Pre(service.hostedSharedEntry)
			service.echo.GET("/app/bootstrap", service.appBootstrapPayload)
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
			requireNativeStatus(t, response, http.StatusOK)
			var payload appBootstrap
			decodeHubResponse(t, response, &payload)
			if payload.Organization.ID != org || payload.Organization.Name != "Example "+org || payload.Organization.Logo == nil || payload.Organization.Logo.ImageKey != "logo_"+org {
				t.Fatalf("bootstrap=%+v", payload.Organization)
			}
			foreign := "org_first"
			if org == foreign {
				foreign = "org_second"
			}
			requireNativeStatus(t, serve(foreign), http.StatusUnauthorized)
		})
	}
}
