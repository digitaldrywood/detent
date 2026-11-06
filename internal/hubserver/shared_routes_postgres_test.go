package hubserver

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type sharedRouteFixture struct {
	services map[string]*Service
	key      ed25519.PrivateKey
}

func newSharedRouteFixture(t *testing.T) sharedRouteFixture {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	key := hostedSharedKey(49)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	result := sharedRouteFixture{services: map[string]*Service{}, key: key}
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		seedStartupOrganization(t, store.postgres, org)
		now := time.Now()
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			id := string(org) + "_owner"
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: id, Hash: id, Scope: "admin", Kind: "personal", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: id, Email: id + "@example.test", MembershipID: id, Role: "owner", Active: true, PrincipalID: id, CreatedAt: now, UpdatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		organizationConfig := cfg
		hosted := *cfg.Hosted
		entry := *cfg.Hosted.SharedEntry
		hosted.SharedEntry = &entry
		hosted.OrganizationID, hosted.WorkOSOrganizationID = string(org), "provider_"+string(org)
		organizationConfig.Hosted = &hosted
		service, err := Open(t.Context(), organizationConfig)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		})
		result.services[string(org)] = service
	}
	return result
}

func (f sharedRouteFixture) request(t *testing.T, org, path, body string, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
	t.Helper()
	id, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	claims := cloudassert.Claims{Issuer: "entry", Audience: org, Email: org + "_owner@example.test", Subject: org + "_owner", Role: "owner", Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest([]byte(body)), IssuedAt: time.Now(), ExpiresAt: time.Now().Add(20 * time.Second), ID: id}
	if mutate != nil {
		mutate(&claims)
	}
	assertion, err := cloudassert.Sign(f.key, claims)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(cloudassert.Header, assertion)
	response := httptest.NewRecorder()
	f.services[org].Handler().ServeHTTP(response, request)
	return response
}

func TestPostgresSharedKeyManagementRoutes(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org := range f.services {
		for _, route := range []struct {
			path, body string
			want       int
		}{
			{"admin", `{}`, http.StatusOK},
			{"context", `{}`, http.StatusOK},
			{"audit", `{"key_id":"example_key","owner":"example_owner","event":"key_revoked"}`, http.StatusNoContent},
		} {
			t.Run(org+"/"+route.path, func(t *testing.T) {
				path := "/internal/v1/keys/" + route.path
				response := f.request(t, org, path, route.body, nil)
				if response.Code != route.want {
					t.Fatalf("allowed=%d %s", response.Code, response.Body)
				}
				for _, mutate := range []func(*cloudassert.Claims){
					func(c *cloudassert.Claims) { c.Kind = cloudassert.KindBrowser },
					func(c *cloudassert.Claims) { c.Audience = "org_foreign" },
				} {
					response = f.request(t, org, path, route.body, mutate)
					if response.Code != http.StatusNotFound && response.Code != http.StatusUnauthorized {
						t.Fatalf("refused=%d %s", response.Code, response.Body)
					}
				}
				if route.path != "audit" {
					response = f.request(t, org, path, route.body, func(c *cloudassert.Claims) { c.Subject = "foreign_owner" })
					if response.Code != http.StatusForbidden {
						t.Fatalf("foreign member=%d %s", response.Code, response.Body)
					}
				}
			})
		}
	}
}
