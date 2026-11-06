package hubserver

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
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

func newSharedRouteFixture(t *testing.T, configure ...func(*Config)) sharedRouteFixture {
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
		for _, apply := range configure {
			apply(&organizationConfig)
		}
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
					sharedRouteBrowserClaims,
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

func TestPostgresSharedPlatformReadRoutes(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig)
	for org := range f.services {
		for _, resource := range []string{"members", "runners", "projects", "usage", "users"} {
			t.Run(org+"/"+resource, func(t *testing.T) {
				path := "/internal/v1/platform/" + resource
				response := f.request(t, org, path, `{}`, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("allowed=%d %s", response.Code, response.Body)
				}
				if resource == "usage" {
					var usage platformTenantUsage
					if json.Unmarshal(response.Body.Bytes(), &usage) != nil || usage.Allowances["projects"].Limit == nil || usage.Allowances["projects"].Used != 0 || usage.WindowEndsAt == "" {
						t.Fatalf("tenant usage: %s", response.Body)
					}
				}
				if resource == "users" && !strings.Contains(response.Body.String(), org+"_owner@example.test") {
					t.Fatalf("missing tenant owner: %s", response.Body)
				}
				for other := range f.services {
					if other != org && strings.Contains(response.Body.String(), other+"_owner@example.test") {
						t.Fatalf("foreign tenant owner: %s", response.Body)
					}
				}
				for _, mutate := range []func(*cloudassert.Claims){
					sharedRouteBrowserClaims,
					func(c *cloudassert.Claims) { c.Audience = "org_foreign" },
				} {
					response = f.request(t, org, path, `{}`, mutate)
					if response.Code != http.StatusNotFound && response.Code != http.StatusUnauthorized {
						t.Fatalf("refused=%d %s", response.Code, response.Body)
					}
				}
			})
		}
	}
}

func sharedRouteBillingConfig(cfg *Config) {
	plans := pilotHostedPlans()
	cfg.Hosted.Plans = &plans
	cfg.Hosted.Billing = &HostedBillingConfig{
		Mode: "test", AccountID: "acct_fixture", CustomerID: "cus_" + cfg.Hosted.OrganizationID,
		PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_2196_secret"), GraceSeconds: 3600, ReconcileSeconds: 60,
		Provider:    &hostedCreditProvider{hostedBillingProvider: &hostedBillingProvider{}},
		Prices:      []HostedBillingPrice{{PriceID: "price_fixture", Label: "Example", Plan: plans.Plans[1].PlanReference}},
		CreditPacks: []HostedCreditPack{{PriceID: "price_credit", Label: "AI credits", USDCents: 500}},
	}
}

func TestPostgresSharedCreditRoutes(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig)
	for org := range f.services {
		t.Run(org, func(t *testing.T) {
			path := "/internal/v1/platform/ai-credits"
			balance := func(want int64) {
				t.Helper()
				response := f.request(t, org, path+"/balance", `{}`, nil)
				var result struct {
					Balance int64 `json:"balance_micros"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Balance != want {
					t.Fatalf("balance=%d %s want=%d", response.Code, response.Body, want)
				}
			}
			balance(0)
			body := `{"idempotency_key":"example_adjustment","amount_usd":"1.00","reason":"Example credit"}`
			for _, target := range []string{path, path + "/balance"} {
				response := f.request(t, org, target, body, sharedRouteBrowserClaims)
				if response.Code != http.StatusNotFound {
					t.Fatalf("browser=%d %s", response.Code, response.Body)
				}
				response = f.request(t, org, target, body, func(c *cloudassert.Claims) { c.Audience = "org_foreign" })
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("foreign=%d %s", response.Code, response.Body)
				}
			}
			balance(0)
			for range 2 {
				response := f.request(t, org, path, body, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("adjust=%d %s", response.Code, response.Body)
				}
			}
			balance(1000000)
		})
	}
}

func sharedRouteBrowserClaims(c *cloudassert.Claims) {
	c.Kind = cloudassert.KindBrowser
	c.ProviderOrganization = "provider_" + c.Audience
	c.ProviderSession = "example_session"
	c.Binding, c.CSRF = strings.Repeat("a", 64), strings.Repeat("b", 64)
	c.SessionCreatedAt, c.SessionExpiresAt, c.AccessExpiresAt = c.IssuedAt, c.ExpiresAt, c.ExpiresAt
}

func TestPostgresSharedBillingRoutes(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig)
	for org := range f.services {
		t.Run(org, func(t *testing.T) {
			path := "/internal/v1/billing/binding"
			response := f.request(t, org, path, `{}`, nil)
			var binding struct {
				CustomerID string `json:"customer_id"`
				Enabled    bool   `json:"enabled"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &binding) != nil || !binding.Enabled || binding.CustomerID != "cus_"+org {
				t.Fatalf("binding=%d %s", response.Code, response.Body)
			}
			for _, target := range []string{path, "/internal/v1/billing/events"} {
				response = f.request(t, org, target, `{}`, sharedRouteBrowserClaims)
				if response.Code != http.StatusNotFound {
					t.Fatalf("browser=%d %s", response.Code, response.Body)
				}
				response = f.request(t, org, target, `{}`, func(c *cloudassert.Claims) { c.Audience = "org_foreign" })
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("foreign=%d %s", response.Code, response.Body)
				}
			}
			for _, customer := range []string{"cus_foreign", "cus_" + org} {
				body, err := json.Marshal(hostedSharedBillingEventRequest{EventID: "evt_example", EventType: "invoice.paid", CustomerID: customer})
				if err != nil {
					t.Fatal(err)
				}
				response = f.request(t, org, "/internal/v1/billing/events", string(body), nil)
				want := http.StatusNoContent
				if customer == "cus_foreign" {
					want = http.StatusConflict
				}
				if response.Code != want {
					t.Fatalf("event=%d %s want=%d", response.Code, response.Body, want)
				}
			}
			if err := f.services[org].database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				event, err := r.BillingEvent(t.Context(), "evt_example")
				if err == nil && (string(event.OrganizationID) != org || event.Type != "invoice.paid") {
					t.Fatalf("event=%+v", event)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPostgresSharedMCPRoutes(t *testing.T) {
	f := newSharedRouteFixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"example-client","version":"1"}}}`
	for org := range f.services {
		claims := func(c *cloudassert.Claims) {
			c.Kind = cloudassert.KindKey
			c.ProviderOrganization = "provider_" + org
			c.Key = &apikey.KeyAuthority{ID: "key_" + org, CredentialBinding: strings.Repeat("c", 64), Kind: "personal", Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "all"}}
		}
		response := f.request(t, org, "/organizations/"+org+"/mcp", body, claims)
		if response.Code != http.StatusOK || response.Header().Get("Mcp-Session-Id") == "" {
			t.Fatalf("%s allowed=%d %s", org, response.Code, response.Body)
		}
		for _, change := range []func(*cloudassert.Claims){func(c *cloudassert.Claims) { c.Audience = "org_foreign" }, func(c *cloudassert.Claims) { c.Subject = "foreign_owner" }} {
			denied := f.request(t, org, "/organizations/"+org+"/mcp", body, func(c *cloudassert.Claims) { claims(c); change(c) })
			if denied.Code != http.StatusUnauthorized && denied.Code != http.StatusForbidden {
				t.Fatalf("%s refused=%d %s", org, denied.Code, denied.Body)
			}
		}
	}
}
