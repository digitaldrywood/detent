package cloudentry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type entryFixture struct {
	service  *Service
	provider *fakeProvider
	tenants  map[string]http.Handler
	fixtures map[string]tenantFixture
	database *sql.DB
}

func newEntryFixture(t *testing.T, configure ...func(*Config)) entryFixture {
	t.Helper()
	return newEntryFixtureWithConfig(t, slog.New(slog.DiscardHandler), configure)
}

func newEntryFixtureWithLogger(t *testing.T, logger *slog.Logger, configure ...func(*hubserver.Config)) entryFixture {
	t.Helper()
	return newEntryFixtureWithConfig(t, logger, nil, configure...)
}

func newEntryFixtureWithConfig(t *testing.T, logger *slog.Logger, configureEntry []func(*Config), configure ...func(*hubserver.Config)) entryFixture {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 42
	key := ed25519.NewKeyFromSeed(seed)
	provider := newFakeProvider()
	provider.Users["user_alice"] = "alice@example.test"
	provider.Users["user_bob"] = "bob@example.test"
	provider.Users["user_carol"] = "carol@example.test"
	provider.Users["user_support"] = "support@example.test"
	provider.Member("user_alice", "porg_alpha", "owner")
	provider.Member("user_alice", "porg_beta", "owner")
	provider.Member("user_bob", "porg_beta", "member")
	database := entrySharedDatabase(t)
	alpha, alphaHandler := newTenantIn(t, database, provider, key, "org_alpha", "porg_alpha", "user_alice", "Alpha secret project", configure...)
	beta, betaHandler := newTenantIn(t, database, provider, key, "org_beta", "porg_beta", "user_alice", "Beta secret project", configure...)
	tenants := map[string]http.Handler{testSocketEndpoint("alpha.sock"): alphaHandler, testSocketEndpoint("beta.sock"): betaHandler}
	cfg := Config{
		Platform:  PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"},
		PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: key, Provider: provider, StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}, StateDir: t.TempDir(),
		LookupEnv: func(string) string { return "" }, Logger: logger, ClientFS: fstest.MapFS{},
		Transport: func(organization Organization) (http.RoundTripper, error) {
			return handlerTransport{tenants[organization.Endpoint]}, nil
		},
	}
	for _, configure := range configureEntry {
		configure(&cfg)
	}
	if cfg.Storage.Backend == "" {
		entryPostgresConfig(t, database)(&cfg)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	for _, tenant := range []struct {
		fixture  tenantFixture
		endpoint string
		name     string
	}{{alpha, testSocketEndpoint("alpha.sock"), "Alpha"}, {beta, testSocketEndpoint("beta.sock"), "Beta"}} {
		if _, err := service.Registry().Register(t.Context(), Organization{ID: tenant.fixture.ID, ProviderID: tenant.fixture.Provider, Name: tenant.name, Endpoint: tenant.endpoint, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return entryFixture{service: service, provider: provider, tenants: tenants, fixtures: map[string]tenantFixture{"org_alpha": alpha, "org_beta": beta}, database: database}
}

type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return (entrytest.HandlerTransport{Handler: h.handler}).RoundTrip(request)
}

func TestSharedEntryTwoOrganizationsOneOrigin(t *testing.T) {
	for _, backend := range []string{"postgres"} {
		t.Run(backend, func(t *testing.T) { testSharedEntryTwoOrganizationsOneOrigin(t, entryBackendConfig(t, backend)) })
	}
}

func testSharedEntryTwoOrganizationsOneOrigin(t *testing.T, configureStorage func(*Config)) {
	t.Parallel()
	f := newEntryFixture(t, configureStorage)
	alice := newBrowser(t, f.service.Handler())
	if location := alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha"); location != "/organizations/org_alpha/organization" {
		t.Fatalf("post-login location = %q", location)
	}
	response, alphaPage := alice.Get("/organizations/org_alpha/app/bootstrap")
	if response.StatusCode != http.StatusOK || !strings.Contains(alphaPage, "Alpha secret project") || strings.Contains(alphaPage, "Beta secret project") {
		t.Fatalf("alpha page status = %d body = %s", response.StatusCode, alphaPage)
	}
	if !strings.Contains(alphaPage, `"api_base":"/api/v2/organizations/org_alpha"`) || response.Header.Get("Content-Security-Policy") == "" || len(response.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("alpha page is not scoped or hardened: %v", response.Header)
	}
	unauthorized, _ := alice.Get("/organizations/org_beta/app/bootstrap")
	if unauthorized.StatusCode != http.StatusSeeOther || !strings.HasPrefix(unauthorized.Header.Get("Location"), "/auth/oidc/start?organization=org_beta") {
		t.Fatalf("unauthorized beta = %d %q", unauthorized.StatusCode, unauthorized.Header.Get("Location"))
	}
	base, _ := url.Parse(testPublicURL)
	stale := newBrowser(t, f.service.Handler())
	stale.Jar.SetCookies(base, alice.Jar.Cookies(base))
	alice.Login("/organizations/org_beta/organization", "user_alice:porg_beta")
	if response, _ := stale.Get("/organizations/org_alpha/app/bootstrap"); response.StatusCode == http.StatusOK {
		t.Fatal("the pre-authentication session cookie survived re-authentication")
	}
	_, betaPage := alice.Get("/organizations/org_beta/app/bootstrap")
	if !strings.Contains(betaPage, "Beta secret project") || strings.Contains(betaPage, "Alpha secret project") {
		t.Fatalf("beta page = %s", betaPage)
	}
	response, alphaAgain := alice.Get("/organizations/org_alpha/app/bootstrap")
	if response.StatusCode != http.StatusOK || !strings.Contains(alphaAgain, "Alpha secret project") {
		t.Fatal("authorizing beta replaced the alpha authorization")
	}
	for _, organization := range []string{"org_alpha", "org_beta"} {
		var bootstrap struct {
			CSRF string `json:"csrf_token"`
		}
		response := alice.Do(http.MethodGet, "/organizations/"+organization+"/app/bootstrap", nil, map[string]string{"Accept": "application/json"})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("bootstrap status = %d", response.StatusCode)
		}
		if err := json.Unmarshal([]byte(response.Body), &bootstrap); err != nil {
			t.Fatal(err)
		}
		for _, rename := range []struct {
			name, key, current string
		}{
			{"Renamed " + organization, "rename-" + organization, "Renamed " + organization},
			{"Latest " + organization, "later-" + organization, "Latest " + organization},
			{"Renamed " + organization, "rename-" + organization, "Latest " + organization},
		} {
			request := httptest.NewRequest(http.MethodPut, testPublicURL+"/api/v2/organizations/"+organization+"/name", strings.NewReader(`{"name":"`+rename.name+`","idempotency_key":"`+rename.key+`"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", testPublicURL)
			request.Header.Set("X-CSRF-Token", bootstrap.CSRF)
			for _, cookie := range alice.Jar.Cookies(base) {
				request.AddCookie(cookie)
			}
			recorder := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("rename status = %d: %s", recorder.Code, recorder.Body.String())
			}
			updated, err := f.service.registry.Organization(t.Context(), organization)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Name != rename.current {
				t.Fatalf("chooser organization name = %q, want %q", updated.Name, rename.current)
			}
		}
	}
	alphaCSRF, betaCSRF := csrfFrom(t, alphaPage), csrfFrom(t, betaPage)
	if alphaCSRF == betaCSRF {
		t.Fatal("organizations share a CSRF token")
	}
	for _, test := range []struct {
		name   string
		csrf   string
		origin string
		status int
	}{
		{"other organization token", betaCSRF, testPublicURL, http.StatusForbidden},
		{"cross-origin form", alphaCSRF, "https://attacker.example.test", http.StatusForbidden},
		{"missing origin", alphaCSRF, "", http.StatusForbidden},
		{"scoped token", alphaCSRF, testPublicURL, http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"name": "Created " + test.name, "grant_access": true, "idempotency_key": "create-" + strings.ReplaceAll(test.name, " ", "-")})
			if err != nil {
				t.Fatal(err)
			}
			recorder := attachmentRequest(t, alice, http.MethodPost, "/api/v2/organizations/org_alpha/projects", bytes.NewReader(body), map[string]string{"Origin": test.origin, "Content-Type": "application/json", "X-CSRF-Token": test.csrf})
			response := page{StatusCode: recorder.Code, Body: recorder.Body.String()}
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
		})
	}
	_, alphaAfter := alice.Get("/organizations/org_alpha/app/bootstrap")
	_, betaAfter := alice.Get("/organizations/org_beta/app/bootstrap")
	if !strings.Contains(alphaAfter, "Created scoped token") || strings.Contains(betaAfter, "Created scoped token") {
		t.Fatal("scoped mutation reached the wrong organization")
	}
	_, chooser := alice.Get("/organizations")
	if strings.Contains(chooser, `href="/organization"`) {
		t.Fatal("entry pages link to the unscoped tenant route")
	}
	if !strings.Contains(chooser, `href="/organizations/org_alpha/organization"`) || !strings.Contains(chooser, `href="/organizations/org_beta/organization"`) {
		t.Fatalf("chooser = %s", chooser)
	}
	f.provider.RemoveMember("user_alice", "porg_beta")
	f.provider.ExpireAccess()
	if response, _ := alice.Get("/organizations/org_beta/app/bootstrap"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("removed membership status = %d", response.StatusCode)
	}
	if response, _ := alice.Get("/organizations/org_alpha/app/bootstrap"); response.StatusCode != http.StatusOK {
		t.Fatal("removing beta membership affected alpha")
	}
	logout := alice.Do(http.MethodPost, "/organizations/org_alpha/logout", url.Values{"csrf": {alphaCSRF}}, nil)
	if logout.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d", logout.StatusCode)
	}
	if response, _ := alice.Get("/organizations/org_alpha/app/bootstrap"); response.StatusCode != http.StatusSeeOther {
		t.Fatal("signed-out browser still reads alpha")
	}
	if len(f.provider.Revoked) < 2 {
		t.Fatalf("provider revoked sessions = %v", f.provider.Revoked)
	}
}

func TestSharedEntryLogoutRevokesLocalAndProviderSessions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		wantStatus int
	}{
		{name: "customer", wantStatus: http.StatusSeeOther},
		{name: "entry fallback", wantStatus: http.StatusSeeOther},
		{name: "support", wantStatus: http.StatusSeeOther},
		{name: "expired local session", wantStatus: http.StatusSeeOther},
		{name: "provider failure", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newEntryFixture(t)
			client := newBrowser(t, f.service.Handler())
			if tt.name == "support" {
				client.Login("/organizations", "user_support:")
				_, page := client.Get("/support")
				client.Do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "reason": {"customer-request"}, "csrf": {csrfFrom(t, page)}}, nil)
				callback, _ := client.Get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request"))
				if callback.StatusCode != http.StatusSeeOther {
					t.Fatalf("support callback = %d %s", callback.StatusCode, callback.Body)
				}
			} else {
				client.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			}
			pagePath, logoutPath := "/organizations/org_alpha/app/bootstrap", "/organizations/org_alpha/logout"
			if tt.name == "entry fallback" {
				pagePath, logoutPath = "/platform", "/logout"
			}
			pageResponse, page := client.Get(pagePath)
			if policy := pageResponse.Header.Get("Content-Security-Policy"); policy != contentSecurity {
				t.Fatalf("page content security = %q, want %q", policy, contentSecurity)
			}
			var hash string
			if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT token_hash FROM platform.sessions WHERE revoked_at IS NULL").Scan(&hash); err != nil {
				t.Fatal(err)
			}
			session, err := f.service.auth.session(t.Context(), hash)
			if err != nil {
				t.Fatal(err)
			}
			authorization, err := f.service.auth.authorization(t.Context(), session, "org_alpha")
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "expired local session" {
				if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE platform.sessions SET expires_at = $1 WHERE token_hash = $2", formatTime(time.Now().Add(-time.Minute)), hash); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.auth.session(t.Context(), hash); !errors.Is(err, ErrNoSession) {
					t.Fatalf("expired session authenticated before logout: %v", err)
				}
			}
			if tt.name == "provider failure" {
				f.provider.RevokeErr = errors.New("private provider credential")
			}
			denied := client.Do(http.MethodPost, logoutPath, url.Values{"csrf": {"invalid"}}, nil)
			if denied.StatusCode != http.StatusForbidden || denied.Header.Get("Location") != "" || len(f.provider.Revoked) != 0 {
				t.Fatalf("invalid CSRF logout = %d, location %q, provider revocations %v", denied.StatusCode, denied.Header.Get("Location"), f.provider.Revoked)
			}
			if _, _, err := f.service.auth.storedSession(t.Context(), hash); err != nil {
				t.Fatalf("invalid CSRF revoked local session: %v", err)
			}
			response := client.Do(http.MethodPost, logoutPath+"?return=https%3A%2F%2Fattacker.example.test", url.Values{"csrf": {csrfFrom(t, page)}, "redirect": {"https://attacker.example.test"}}, nil)
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("logout status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
			wantLocation := "https://detent.build"
			if tt.wantStatus == http.StatusServiceUnavailable {
				wantLocation = ""
				if !strings.Contains(response.Body, "Provider sign-out could not be confirmed") {
					t.Fatal("provider failure did not retain the sign-out error page")
				}
			}
			if location := response.Header.Get("Location"); location != wantLocation {
				t.Fatalf("logout location = %q, want %q", location, wantLocation)
			}
			if strings.Contains(response.Body, "private provider credential") {
				t.Fatal("logout exposed provider error")
			}
			cleared := false
			for _, cookie := range (&http.Response{Header: response.Header}).Cookies() {
				if cookie.Name == f.service.cookieName("session") && cookie.Value == "" && cookie.MaxAge == -1 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatal("logout did not clear browser session")
			}
			var revoked bool
			if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT revoked_at IS NOT NULL FROM platform.sessions WHERE token_hash = $1", hash).Scan(&revoked); err != nil || !revoked {
				t.Fatalf("local session not revoked: %v", err)
			}
			if _, err := f.service.auth.authorization(t.Context(), session, "org_alpha"); !errors.Is(err, ErrNoSession) {
				t.Fatalf("logged-out organization authorization remains active: %v", err)
			}
			for _, id := range []string{session.Identity.SessionID, authorization.Identity.SessionID} {
				found := false
				for _, revokedID := range f.provider.Revoked {
					found = found || revokedID == id
				}
				if !found {
					t.Fatalf("provider session %q was not revoked: %v", id, f.provider.Revoked)
				}
			}
		})
	}
}

func TestSharedEntryBoundaries(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	f.service.config.Build = buildinfo.Info{Version: "1.2.4", Commit: strings.Repeat("a", 40)}
	health := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"version":"1.2.4"`) || !strings.Contains(health.Body.String(), f.service.config.Build.Commit) {
		t.Fatalf("release health identity=%d %s", health.Code, health.Body)
	}

	for _, tt := range []struct {
		name, method, target, location string
		status                         int
	}{
		{"login alias", http.MethodGet, "/login", "/", http.StatusSeeOther},
		{"login return", http.MethodGet, "/login?return=%2Forganizations%2Forg_alpha%2Fwork", "/?return=%2Forganizations%2Forg_alpha%2Fwork", http.StatusSeeOther},
		{"login external return", http.MethodGet, "/login?return=https%3A%2F%2Fattacker.example.test", "/?return=https%3A%2F%2Fattacker.example.test", http.StatusSeeOther},
		{"home head", http.MethodHead, "/", "", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := newBrowser(t, f.service.Handler()).Do(tt.method, tt.target, nil, nil)
			if response.StatusCode != tt.status || response.Header.Get("Location") != tt.location {
				t.Fatalf("response = %d, location %q; want %d, %q", response.StatusCode, response.Header.Get("Location"), tt.status, tt.location)
			}
		})
	}

	alice := newBrowser(t, f.service.Handler())
	alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	stranger := newBrowser(t, f.service.Handler())
	tests := []struct {
		name   string
		client *browser
		target string
		header map[string]string
		status int
	}{
		{"unauthenticated API", stranger, "/api/v2/organizations/org_alpha/projects/p/work-items", nil, http.StatusUnauthorized},
		{"unknown organization", alice, "/organizations/org_missing/organization", nil, http.StatusNotFound},
		{"encoded separator", alice, "/organizations/org_alpha/projects%2Fx", nil, http.StatusNotFound},
		{"dot segment", alice, "/organizations/org_alpha/../org_beta/organization", nil, http.StatusNotFound},
		{"spoofed assertion without session", stranger, "/organizations/org_alpha/organization", map[string]string{cloudassert.Header: "forged.value", "X-Forwarded-Host": "tenant"}, http.StatusSeeOther},
		{"bearer is not browser authority", alice, "/organizations/org_alpha/organization", map[string]string{"Authorization": "Bearer detent_invalid"}, http.StatusSeeOther},
		{"absolute open redirect", stranger, "/auth/oidc/start?return=https%3A%2F%2Fattacker.example.test", nil, http.StatusBadRequest},
		{"open redirect", alice, "/auth/oidc/start?return=%2F%2Fattacker.example.test", nil, http.StatusBadRequest},
		{"cross-organization return", alice, "/auth/oidc/start?organization=org_alpha&return=%2Forganizations%2Forg_beta%2Forganization", nil, http.StatusBadRequest},
		{"legacy unscoped route", alice, "/organization", nil, http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := test.client.Do(http.MethodGet, test.target, nil, test.header)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, test.status, response.Body)
			}
			if strings.Contains(response.Body, "Alpha secret project") {
				t.Fatal("boundary response exposed tenant content")
			}
		})
	}
	key := createTestAccessKey(t, alice, "selected", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}}, "", "read")
	metadata := stranger.Do(http.MethodGet, "/organizations/org_alpha/api/v2/organizations/org_alpha/projects", nil, map[string]string{"Authorization": "Bearer " + key.Token})
	if metadata.StatusCode != http.StatusOK {
		t.Fatalf("machine projects status = %d", metadata.StatusCode)
	}
	transport, stop := entryTenantTransport(t, f.tenants[testSocketEndpoint("alpha.sock")])
	for _, failure := range []struct {
		name  string
		setup bool
	}{
		{name: "transport setup failure", setup: true},
		{name: "stopped tenant"},
	} {
		t.Run(failure.name, func(t *testing.T) {
			f.service.config.Transport = func(Organization) (http.RoundTripper, error) {
				if failure.setup {
					return nil, errors.New("tenant transport unavailable")
				}
				return transport, nil
			}
			if !failure.setup {
				if response, _ := alice.Get("/organizations/org_alpha/app/bootstrap"); response.StatusCode != http.StatusOK {
					t.Fatalf("tenant before stop = %d", response.StatusCode)
				}
				stop()
			}
			for _, request := range []struct {
				name, method, path, accept string
				bearer, html               bool
			}{
				{name: "browser root", method: http.MethodGet, path: "/organizations/org_alpha?tab=work&view=board", accept: "text/html", html: true},
				{name: "browser accept list", method: http.MethodGet, path: "/organizations/org_alpha/organization", accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", html: true},
				{name: "browser head", method: http.MethodHead, path: "/organizations/org_alpha/organization", accept: "text/html", html: true},
				{name: "JSON request", method: http.MethodGet, path: "/organizations/org_alpha", accept: "application/json"},
				{name: "JSON accept list", method: http.MethodGet, path: "/organizations/org_alpha", accept: "application/json, text/html"},
				{name: "no accept", method: http.MethodGet, path: "/organizations/org_alpha"},
				{name: "HTML excluded", method: http.MethodGet, path: "/organizations/org_alpha", accept: "text/html;q=0"},
				{name: "API path", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/organizations/org_alpha/projects", accept: "text/html"},
				{name: "machine caller", method: http.MethodGet, path: "/organizations/org_alpha", accept: "text/html", bearer: true},
				{name: "mutation", method: http.MethodPost, path: "/organizations/org_alpha/projects", accept: "text/html"},
			} {
				t.Run(request.name, func(t *testing.T) {
					headers := map[string]string{"Accept": request.accept}
					if request.bearer {
						headers["Authorization"] = "Bearer " + testAdminKey
					}
					response := alice.Do(request.method, request.path, nil, headers)
					if response.StatusCode != http.StatusBadGateway {
						t.Fatalf("tenant failure = %d: %s", response.StatusCode, response.Body)
					}
					if request.html {
						assertEntryFallback(t, alice, response.Body)
						if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") || !strings.Contains(response.Body, `href="`+strings.ReplaceAll(request.path, "&", "&amp;")+`" class="entry-button entry-primary">Try again`) {
							t.Fatalf("browser failure lost HTML or retry path: %s", response.Body)
						}
					} else {
						want := `{"code":"tenant_unavailable","message":"The organization is temporarily unavailable"}`
						if failure.setup {
							want += "\n"
						}
						if response.Body != want || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
							t.Fatalf("tenant JSON changed: %q, want %q", response.Body, want)
						}
					}
				})
			}
		})
	}
}

func TestSharedEntryLoginTransactions(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	if _, err := f.service.Registry().Register(t.Context(), Organization{ID: "org_disabled", ProviderID: "porg_disabled", Name: "Disabled", State: "disabled", Endpoint: testSocketEndpoint("disabled.sock"), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, query, selected, authorized, location, role, code string
		missingMembership                                       bool
		status                                                  int
		fallback                                                bool
	}{
		{name: "selected alpha", selected: "porg_alpha", authorized: "org_alpha", location: "/alpha/work", status: http.StatusSeeOther},
		{name: "switch alpha", query: "organization=org_alpha", selected: "porg_alpha", authorized: "org_alpha", location: "/alpha/work", status: http.StatusSeeOther},
		{name: "switch beta", query: "organization=org_beta", selected: "porg_beta", authorized: "org_beta", location: "/beta/work", status: http.StatusSeeOther},
		{name: "switch removed membership", query: "organization=org_beta", selected: "porg_beta", missingMembership: true, status: http.StatusUnauthorized},
		{name: "support identity cannot switch", query: "organization=org_beta", code: "support|support@example.test|user_alice|porg_beta|customer-request", status: http.StatusUnauthorized},
		{name: "selected beta", selected: "porg_beta", authorized: "org_beta", location: "/beta/work", status: http.StatusSeeOther},
		{name: "reporter selected", selected: "porg_alpha", authorized: "org_alpha", location: "/alpha/reporter", role: "reporter", status: http.StatusSeeOther},
		{name: "reporter invitation return", query: "organization=org_alpha&return=/organizations/org_alpha/work", selected: "porg_alpha", authorized: "org_alpha", location: "/alpha/reporter", role: "reporter", status: http.StatusSeeOther},
		{name: "reporter deep link", query: "organization=org_alpha&return=/organizations/org_alpha/work/i/wi_example", selected: "porg_alpha", authorized: "org_alpha", location: "/organizations/org_alpha/work/i/wi_example", role: "reporter", status: http.StatusSeeOther},
		{name: "chooser return alpha", query: "return=/organizations", selected: "porg_alpha", authorized: "org_alpha", location: "/alpha/work", status: http.StatusSeeOther},
		{name: "chooser return beta", query: "return=/organizations", selected: "porg_beta", authorized: "org_beta", location: "/beta/work", status: http.StatusSeeOther},
		{name: "deep link", query: "organization=org_beta&return=/organizations/org_beta/organization", selected: "porg_beta", authorized: "org_beta", location: "/organizations/org_beta/organization", status: http.StatusSeeOther},
		{name: "mismatch", query: "organization=org_alpha&return=/organizations/org_alpha/work", selected: "porg_beta", status: http.StatusForbidden},
		{name: "no selection", location: "/organizations", status: http.StatusSeeOther},
		{name: "unknown selection", selected: "porg_unknown", location: "/organizations", status: http.StatusSeeOther},
		{name: "unready selection", selected: "porg_disabled", location: "/organizations", status: http.StatusSeeOther},
		{name: "server rendered fallback", selected: "porg_beta", authorized: "org_beta", location: "/organizations/org_beta/organization", status: http.StatusSeeOther, fallback: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.missingMembership {
				f.provider.RemoveMember("user_alice", tt.selected)
				t.Cleanup(func() { f.provider.Member("user_alice", tt.selected, "owner") })
			}
			if tt.role != "" {
				f.provider.Member("user_alice", tt.selected, tt.role)
				t.Cleanup(func() { f.provider.Member("user_alice", tt.selected, "owner") })
			}
			if tt.selected == "porg_unknown" || tt.selected == "porg_disabled" {
				f.provider.Member("user_alice", tt.selected, "owner")
				t.Cleanup(func() { f.provider.RemoveMember("user_alice", tt.selected) })
			}
			f.service.config.ClientFS = fstest.MapFS{}
			if !tt.fallback {
				f.service.config.ClientFS = fstest.MapFS{entryClientShell: {Data: []byte(`<html><head></head><body><div id="root"></div></body></html>`)}}
			}
			b := newBrowser(t, f.service.Handler())
			start, _ := b.Get("/auth/oidc/start?" + tt.query)
			code := tt.code
			if code == "" {
				code = "user_alice:" + tt.selected
			}
			response, _ := b.Get("/auth/oidc/callback?" + url.Values{"code": {code}, "state": {stateOf(t, start)}}.Encode())
			if response.StatusCode != tt.status || response.Header.Get("Location") != tt.location {
				t.Fatalf("callback = %d %q, want %d %q", response.StatusCode, response.Header.Get("Location"), tt.status, tt.location)
			}
			for _, organization := range []string{"org_alpha", "org_beta"} {
				response, _ := b.Get("/organizations/" + organization + "/app/bootstrap")
				if organization == tt.authorized {
					if response.StatusCode != http.StatusOK {
						t.Fatalf("first selected tenant request = %d: %s", response.StatusCode, response.Body)
					}
				} else if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start?organization="+organization) {
					t.Fatalf("unselected tenant %s = %d: %s", organization, response.StatusCode, response.Body)
				}
			}
		})
	}
	f.service.config.ClientFS = fstest.MapFS{}
	for _, tt := range []struct {
		name, query, organization, screenHint string
		status                                int
	}{
		{name: "sign in"},
		{name: "create account", query: "screen_hint=sign-up", screenHint: "sign-up"},
		{name: "organization create account", query: "organization=org_alpha&screen_hint=sign-up", organization: "porg_alpha", screenHint: "sign-up"},
		{name: "unknown hint", query: "screen_hint=unexpected"},
		{name: "switch unknown organization", query: "organization=org_missing", status: http.StatusNotFound},
		{name: "switch unready organization", query: "organization=org_disabled", status: http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response, _ := alice.Get("/auth/oidc/start?" + tt.query)
			if tt.status != 0 {
				if response.StatusCode != tt.status || response.Header.Get("Location") != "" {
					t.Fatalf("switch target admission = %d %q, want %d without redirect", response.StatusCode, response.Header.Get("Location"), tt.status)
				}
				return
			}
			location, err := url.Parse(response.Header.Get("Location"))
			if response.StatusCode != http.StatusSeeOther || err != nil {
				t.Fatalf("authorization redirect = %d %q: %v", response.StatusCode, response.Header.Get("Location"), err)
			}
			query := location.Query()
			if query.Get("organization_id") != tt.organization || query.Get("screen_hint") != tt.screenHint || query.Has("screen_hint") != (tt.screenHint != "") {
				t.Fatalf("authorization query = %v", query)
			}
			if query.Get("state") == "" || query.Get("verifier") == "" {
				t.Fatal("authorization lost state or PKCE")
			}
		})
	}

	first, _ := alice.Get("/auth/oidc/start?organization=org_alpha")
	second, _ := alice.Get("/auth/oidc/start?organization=org_beta")
	firstState := stateOf(t, first)
	secondState := stateOf(t, second)
	if response, _ := alice.Get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_beta"}, "state": {secondState}}.Encode()); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("second tab callback = %d", response.StatusCode)
	}
	if response, _ := alice.Get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_alpha"}, "state": {firstState}}.Encode()); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first tab callback = %d", response.StatusCode)
	}
	if response, _ := alice.Get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_beta"}, "state": {secondState}}.Encode()); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed callback = %d", response.StatusCode)
	}
	wrong, _ := alice.Get("/auth/oidc/start?organization=org_alpha")
	if response, _ := alice.Get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_beta"}, "state": {stateOf(t, wrong)}}.Encode()); response.StatusCode != http.StatusForbidden {
		t.Fatalf("organization mismatch = %d", response.StatusCode)
	}
	for _, organization := range []string{"org_alpha", "org_beta"} {
		response, _ := alice.Get("/organizations/" + organization + "/app/bootstrap")
		want := http.StatusOK
		if organization == "org_alpha" {
			want = http.StatusSeeOther
		}
		if response.StatusCode != want {
			t.Fatalf("%s after concurrent logins = %d", organization, response.StatusCode)
		}
	}
	other := newBrowser(t, f.service.Handler())
	stolen, _ := alice.Get("/auth/oidc/start")
	if response, _ := other.Get("/auth/oidc/callback?" + url.Values{"code": {"user_bob:"}, "state": {stateOf(t, stolen)}}.Encode()); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback without the initiating browser = %d", response.StatusCode)
	}
}

func TestSharedEntryLoginCookieReplacement(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{testPublicURL, "https://entry.example.test"} {
		t.Run(origin, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			f.service.config.PublicURL = origin
			f.service.secure = strings.HasPrefix(origin, "https:")
			base, err := url.Parse(origin)
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name            string
				starts          int
				supersededFirst bool
				legacyOnStart   bool
			}{
				{name: "single start", starts: 1},
				{name: "newest callback first", starts: 2},
				{name: "superseded callback first", starts: 2, supersededFirst: true},
				{name: "retry loop", starts: 100, supersededFirst: true},
				{name: "legacy cookies on start", starts: 2, legacyOnStart: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					b := newBrowser(t, f.service.Handler())
					b.Origin = origin
					seedLegacy := func() {
						b.Jar.SetCookies(base, []*http.Cookie{
							{Name: f.service.cookieName("login_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Value: "old-token-a", Path: "/", Secure: f.service.secure},
							{Name: f.service.cookieName("login_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Value: "old-token-b", Path: "/", Secure: f.service.secure},
						})
					}
					assertLoginCookies := func(want int) {
						t.Helper()
						count := 0
						supportFound := false
						for _, cookie := range b.Jar.Cookies(base) {
							if strings.HasPrefix(cookie.Name, f.service.cookieName("login")) {
								count++
							}
							if cookie.Name == f.service.cookieName("support") && cookie.Value == "existing-support" {
								supportFound = true
							}
						}
						if count != want {
							t.Fatalf("login cookie count = %d, want %d", count, want)
						}
						if !supportFound {
							t.Fatal("login changed the support cookie")
						}
					}
					b.Jar.SetCookies(base, []*http.Cookie{
						{Name: f.service.cookieName("session"), Value: "existing-session", Path: "/", Secure: f.service.secure},
						{Name: f.service.cookieName("support"), Value: "existing-support", Path: "/", Secure: f.service.secure},
					})
					if test.legacyOnStart {
						seedLegacy()
					}
					var firstState, newestState string
					for range test.starts {
						start, _ := b.Get("/auth/oidc/start?organization=org_beta&return=/organizations/org_beta/app/bootstrap")
						newestState = stateOf(t, start)
						if firstState == "" {
							firstState = newestState
						}
						assertLoginCookies(1)
						for _, cookie := range (&http.Response{Header: start.Header}).Cookies() {
							if cookie.Name == f.service.cookieName("session") || cookie.Name == f.service.cookieName("support") {
								t.Fatalf("login start changed %s", cookie.Name)
							}
							if cookie.Value != "" && (!cookie.HttpOnly || cookie.Secure != f.service.secure || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 || cookie.MaxAge > 600) {
								t.Fatalf("login cookie attributes = %+v", cookie)
							}
						}
					}
					callback := func(state string) page {
						response, _ := b.Get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_beta"}, "state": {state}}.Encode())
						return response
					}
					assertSuperseded := func() {
						t.Helper()
						response := callback(firstState)
						if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Body, "This sign-in link is invalid or has already been used") || !strings.Contains(response.Body, "Sign in again") {
							t.Fatalf("superseded callback = %d: %s", response.StatusCode, response.Body)
						}
					}
					if test.supersededFirst {
						assertSuperseded()
						assertLoginCookies(1)
					}
					seedLegacy()
					response := callback(newestState)
					if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/organizations/org_beta/app/bootstrap" {
						t.Fatalf("newest callback = %d %q: %s", response.StatusCode, response.Header.Get("Location"), response.Body)
					}
					assertLoginCookies(0)
					if response, _ := b.Get("/organizations/org_beta/app/bootstrap"); response.StatusCode != http.StatusOK {
						t.Fatalf("authenticated return path = %d", response.StatusCode)
					}
					if test.starts > 1 && !test.supersededFirst {
						assertSuperseded()
					}
				})
			}
		})
	}
}

func TestSharedEntryInvitation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, message           string
		entryCode, callbackCode int
		starts                  int
	}{
		{name: "new user sign-up", entryCode: 303, callbackCode: 303},
		{name: "existing user sign-in", entryCode: 303, callbackCode: 303},
		{name: "reporter sign-in", entryCode: 303, callbackCode: 303},
		{name: "legacy token alias", entryCode: 303, callbackCode: 303},
		{name: "repeated invitation starts", entryCode: 303, callbackCode: 303, starts: 3},
		{name: "wrong account", entryCode: 303, callbackCode: 403, message: "different account at example.test"},
		{name: "expired", entryCode: 403, message: "has expired"},
		{name: "used", entryCode: 403, message: "already been used"},
		{name: "provider rejected expired invitation", entryCode: 303, callbackCode: 403, message: "has expired"},
		{name: "expired during login", entryCode: 303, callbackCode: 403, message: "has expired"},
		{name: "unknown", entryCode: 403, message: "unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newEntryFixture(t)
			alice := newBrowser(t, f.service.Handler())
			alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			_, page := alice.Get("/organizations/org_alpha/app/bootstrap")
			role := "member"
			if tt.name == "reporter sign-in" {
				role = "reporter"
			}
			invite := inviteEntryTestMember(t, alice, "carol@example.test", role, csrfFrom(t, page))
			if invite.StatusCode != http.StatusCreated {
				t.Fatalf("invite = %d: %s", invite.StatusCode, invite.Body)
			}
			if role == "reporter" {
				f.service.config.ClientFS = fstest.MapFS{entryClientShell: {Data: []byte("<html><head></head><body></body></html>")}}
			}
			invitation := f.provider.Invitations["inv_carol"]
			switch tt.name {
			case "new user sign-up":
				delete(f.provider.Users, "user_carol")
			case "expired":
				invitation.ExpiresAt = time.Now().Add(-time.Hour)
			case "used":
				invitation.State, invitation.AcceptedUserID = "accepted", "user_carol"
			}
			f.provider.Invitations["inv_carol"] = invitation
			browser := newBrowser(t, f.service.Handler())
			path := "/invite?invitation_token=inv_carol"
			if tt.name == "legacy token alias" {
				path = "/invite?token=inv_carol"
			}
			if tt.name == "unknown" {
				path = "/invite?invitation_token=inv_unknown"
			}
			start, body := browser.Get(path)
			var supersededState string
			if tt.starts > 1 {
				supersededState = stateOf(t, start)
			}
			for i := 1; i < tt.starts; i++ {
				start, body = browser.Get(path)
			}
			if supersededState != "" {
				response, _ := browser.Get("/auth/oidc/callback?" + url.Values{"code": {"user_carol:"}, "state": {supersededState}}.Encode())
				if response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("superseded invitation callback = %d", response.StatusCode)
				}
			}
			if start.StatusCode != tt.entryCode {
				t.Fatalf("entry = %d, want %d: %s", start.StatusCode, tt.entryCode, body)
			}
			assertExplanation := func(body string) {
				t.Helper()
				for _, want := range []string{tt.message, "Ask the person who invited you for a new invitation", `href="https://detent.build"`} {
					if !strings.Contains(body, want) {
						t.Errorf("explanation missing %q", want)
					}
				}
				if strings.Contains(body, "carol@example.test") || strings.Contains(body, "inv_carol") {
					t.Fatal("explanation leaked recipient or token")
				}
			}
			if start.StatusCode != http.StatusSeeOther {
				assertExplanation(body)
				return
			}
			authorization, _ := url.Parse(start.Header.Get("Location"))
			wantHint := ""
			if tt.name == "new user sign-up" {
				wantHint = "sign-up"
				f.provider.Users["user_carol"] = "carol@example.test"
			}
			if authorization.Query().Get("invitation_token") != "inv_carol" || authorization.Query().Get("screen_hint") != wantHint {
				t.Fatalf("AuthKit invitation URL = %s", authorization)
			}
			if tt.name == "expired during login" || tt.name == "provider rejected expired invitation" {
				invitation.ExpiresAt = time.Now().Add(-time.Hour)
				f.provider.Invitations["inv_carol"] = invitation
			}
			code := "user_carol:"
			if tt.name == "wrong account" {
				code = "user_bob:"
			}
			query := url.Values{"code": {code}, "state": {stateOf(t, start)}, "invitation_token": {"attacker_token"}}
			if tt.name == "provider rejected expired invitation" {
				query.Set("error", "access_denied")
			}
			accepted, body := browser.Get("/auth/oidc/callback?" + query.Encode())
			if accepted.StatusCode != tt.callbackCode {
				t.Fatalf("callback = %d, want %d: %s", accepted.StatusCode, tt.callbackCode, body)
			}
			if accepted.StatusCode != http.StatusSeeOther {
				assertExplanation(body)
				memberships, _ := f.provider.Memberships(t.Context(), "user_carol", "porg_alpha")
				if len(memberships) != 0 {
					t.Fatal("failed invitation granted membership")
				}
				return
			}
			if !strings.HasPrefix(accepted.Header.Get("Location"), "/auth/oidc/start?organization=org_alpha") {
				t.Fatalf("next = %s", accepted.Header.Get("Location"))
			}
			if role == "reporter" {
				start, _ := browser.Get(accepted.Header.Get("Location"))
				response, _ := browser.Get("/auth/oidc/callback?" + url.Values{"code": {"user_carol:porg_alpha"}, "state": {stateOf(t, start)}}.Encode())
				if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/alpha/reporter" {
					t.Fatalf("reporter invitation destination = %d %q", response.StatusCode, response.Header.Get("Location"))
				}
				return
			}
			browser.Login(accepted.Header.Get("Location"), "user_carol:porg_alpha")
			response, body := browser.Get("/organizations/org_alpha/app/bootstrap")
			if response.StatusCode != http.StatusOK || strings.Contains(body, "Alpha secret project") {
				t.Fatalf("member page = %d (project access must require explicit grant)", response.StatusCode)
			}
			if response, _ := browser.Get("/organizations/org_beta/app/bootstrap"); response.StatusCode != http.StatusSeeOther {
				t.Fatalf("member reached other organization = %d", response.StatusCode)
			}
			if response, body := browser.Get(path); response.StatusCode != http.StatusForbidden || !strings.Contains(body, "already been used") {
				t.Fatalf("used link = %d: %s", response.StatusCode, body)
			}
		})
	}
}

func TestRegistryRegister(t *testing.T) {
	t.Parallel()
	registry := openTestRegistry(t)
	base := Organization{ID: "org_a", ProviderID: "porg_a", Name: "A", Endpoint: testSocketEndpoint("a.sock"), Generation: 1}
	tests := []struct {
		name    string
		mutate  func(*Organization)
		changed bool
		err     bool
	}{
		{"new", func(*Organization) {}, true, false},
		{"repeat", func(*Organization) {}, false, false},
		{"rename", func(o *Organization) { o.Name = "A renamed" }, true, false},
		{"other provider", func(o *Organization) { o.ProviderID = "porg_other" }, false, true},
		{"provider reuse", func(o *Organization) { o.ID = "org_b" }, false, true},
		{"same generation move", func(o *Organization) { o.Name = "A renamed"; o.Endpoint = testSocketEndpoint("b.sock") }, false, true},
		{"generation move", func(o *Organization) {
			o.Name = "A renamed"
			o.Endpoint = testSocketEndpoint("b.sock")
			o.Generation = 2
		}, true, false},
		{"generation rollback", func(o *Organization) { o.Name = "A renamed"; o.Generation = 1 }, false, true},
		{"public endpoint", func(o *Organization) { o.Endpoint = "http://10.0.0.1:80"; o.Generation = 3 }, false, true},
		{"loopback tcp", func(o *Organization) { o.Endpoint = "http://127.0.0.1:7777"; o.Generation = 3 }, false, true},
		{"relative socket", func(o *Organization) { o.Endpoint = "unix:run/a.sock"; o.Generation = 3 }, false, true},
		{"bad id", func(o *Organization) { o.ID = "tenant"; o.ProviderID = "porg_x" }, false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			organization := base
			test.mutate(&organization)
			changed, err := registry.Register(t.Context(), organization)
			if (err != nil) != test.err || changed != test.changed {
				t.Fatalf("Register = %v, %v; want changed %v error %v", changed, err, test.changed, test.err)
			}
		})
	}
	if _, err := registry.store.db.ExecContext(t.Context(), "DELETE FROM platform.organizations"); err == nil {
		t.Fatal("registry deleted an organization record")
	}
}

func TestValidReturnPath(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path, organization string
		ok                 bool
	}{
		{"", "", true},
		{"/organizations", "", true},
		{"/organizations/org_a/work", "org_a", true},
		{"/organizations/org_a", "org_a", true},
		{"/organizations/org_b/work", "org_a", false},
		{"/organizations/org_ab/work", "org_a", false},
		{"/organizations/org_a/logout", "org_a", false},
		{"//attacker.example.test", "", false},
		{"https://attacker.example.test/organizations", "", false},
		{"/organizations/org_a/work?next=x", "org_a", false},
		{`/\attacker`, "", false},
		{"/organizations/org_a/../org_b", "org_a", false},
		{"/settings", "", false},
		{"/example-org/example-project/issues/1", "", true},
		{"/example-org/../settings", "", false},
		{"/example-org/logout", "", false},
		{"/oauth/authorize", "", false},
		{"/mcp", "", false},
		{"/example-org/example-project/issues/1", "org_a", false},
	} {
		if got := validReturnPath(test.path, test.organization); got != test.ok {
			t.Errorf("validReturnPath(%q, %q) = %v, want %v", test.path, test.organization, got, test.ok)
		}
	}
}

func TestConfigRequiresLoopbackListener(t *testing.T) {
	t.Parallel()
	attachments := newSpacesFixture(t, false).Config()
	seed := make([]byte, ed25519.SeedSize)
	for _, test := range []struct {
		listen string
		ok     bool
	}{{"127.0.0.1:8017", true}, {"[::1]:8017", true}, {"localhost:8017", true}, {"0.0.0.0:8017", false}, {":8017", false}, {"10.0.0.5:8017", false}} {
		config := Config{Attachments: &attachments, Platform: PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"}, PublicURL: "https://hub.example.test", ListenAddress: test.listen, Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(seed), Provider: newFakeProvider(), StateDir: "/state", Storage: StorageConfig{Backend: "postgres", PostgresDSN: "postgres://example.invalid/example", Pool: platformstore.PoolConfig{MaxConnections: 5, ServingConnections: 3, BackgroundConnections: 1}}}
		if err := config.validate(); (err == nil) != test.ok {
			t.Errorf("listen %q error = %v, want ok %v", test.listen, err, test.ok)
		}
	}
}

func TestSharedEntrySupportAccess(t *testing.T) {
	for _, backend := range []string{"postgres"} {
		t.Run(backend, func(t *testing.T) { testSharedEntrySupportAccess(t, entryBackendConfig(t, backend)) })
	}
}

func testSharedEntrySupportAccess(t *testing.T, configureStorage func(*Config)) {
	t.Parallel()
	f := newEntryFixture(t, configureStorage)
	support := newBrowser(t, f.service.Handler())
	support.Login("/organizations", "user_support:")
	if response, _ := support.Get("/organizations/org_alpha/app/bootstrap"); response.StatusCode == http.StatusOK {
		t.Fatal("staff session read customer content without support access")
	}
	response, page := support.Get("/support?organization=org_alpha")
	if response.StatusCode != http.StatusOK || !strings.Contains(page, `value="org_alpha"`) {
		t.Fatalf("support page = %d %s", response.StatusCode, page)
	}
	if !strings.Contains(page, `value="org_alpha" selected`) || strings.Contains(page, `value="org_beta"`) {
		t.Fatalf("support request lost or widened organization context: %s", page)
	}
	if invalid, _ := support.Get("/support?organization=org_missing"); invalid.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown support organization = %d", invalid.StatusCode)
	}
	start := support.Do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "reason": {"customer-request"}, "csrf": {csrfFrom(t, page)}}, nil)
	if start.StatusCode != http.StatusOK || !strings.Contains(start.Body, "impersonate") {
		t.Fatalf("support start = %d", start.StatusCode)
	}
	other := newBrowser(t, f.service.Handler())
	other.Login("/organizations", "user_support:")
	if response, _ := other.Get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request")); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("support callback in another browser = %d", response.StatusCode)
	}
	callback, _ := support.Get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request"))
	if callback.StatusCode != http.StatusSeeOther || callback.Header.Get("Location") != "/organizations/org_alpha/organization" {
		t.Fatalf("support callback = %d %q %s", callback.StatusCode, callback.Header.Get("Location"), callback.Body)
	}
	response, body := support.Get("/organizations/org_alpha/app/bootstrap")
	if response.StatusCode != http.StatusOK || (!strings.Contains(body, `"actor":"support@example.test"`) || !strings.Contains(body, `"email":"alice@example.test"`)) || !strings.Contains(body, "Alpha secret project") {
		t.Fatalf("support page = %d %s", response.StatusCode, body)
	}
	if response, _ := support.Get("/organizations/org_beta/app/bootstrap"); response.StatusCode == http.StatusOK {
		t.Fatal("support access for alpha opened beta")
	}
	if replay, _ := support.Get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request")); replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed support callback = %d", replay.StatusCode)
	}
	base, err := url.Parse(testPublicURL)
	if err != nil {
		t.Fatal(err)
	}
	var staff accountSession
	for _, cookie := range support.Jar.Cookies(base) {
		if cookie.Name == f.service.cookieName("session") {
			staff, err = f.service.auth.session(t.Context(), apikey.HashToken(cookie.Value))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if staff.Hash == "" {
		t.Fatal("support callback lost staff session")
	}
	authorized, err := f.service.auth.authorization(t.Context(), staff, "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	providerIdentity, err := f.provider.CurrentSession(t.Context(), authorized.Identity)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.service.auth.store.db.postgres.Authorization(t.Context(), staff.Hash, "org_alpha")
	if err != nil || !stored.Support || !stored.ExpiresAt.Equal(providerIdentity.ExpiresAt) || !stored.Identity.ExpiresAt.Equal(providerIdentity.ExpiresAt) || !stored.ExpiresAt.After(time.Now().Add(10*time.Minute)) {
		t.Fatalf("resulting support authorization expiry differs from provider session: %+v %v", stored, err)
	}
	if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE platform.authorizations SET expires_at=$1 WHERE binding=$2 AND organization_id=$3", time.Now().UTC(), stored.Binding, "org_alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.auth.authorization(t.Context(), staff, "org_alpha"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired support authorization remains usable: %v", err)
	}
	if expired, _ := support.Get("/organizations/org_alpha/app/bootstrap"); expired.StatusCode == http.StatusOK {
		t.Fatal("expired support authorization reached customer content")
	}
	support.Login("/auth/oidc/start", "user_support:")
	if response, _ := support.Get("/organizations/org_alpha/app/bootstrap"); response.StatusCode == http.StatusOK {
		t.Fatal("an ordinary sign-in carried support access into the new session")
	}
	alice := newBrowser(t, f.service.Handler())
	alice.Login("/organizations", "user_alice:")
	_, choices := alice.Get("/support")
	if denied := alice.Do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "csrf": {csrfFrom(t, choices)}}, nil); denied.StatusCode != http.StatusForbidden {
		t.Fatalf("customer support start = %d", denied.StatusCode)
	}
	for _, test := range []struct{ name, code string }{
		{"invalid reason", "support|support@example.test|user_alice|porg_alpha|curiosity"},
		{"other organization", "support|support@example.test|user_alice|porg_beta|customer-request"},
		{"other actor", "support|staff@example.test|user_alice|porg_alpha|customer-request"},
		{"reason differs from the started request", "support|support@example.test|user_alice|porg_alpha|troubleshooting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, page := support.Get("/support")
			support.Do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "reason": {"customer-request"}, "csrf": {csrfFrom(t, page)}}, nil)
			if response, _ := support.Get("/auth/oidc/callback?code=" + url.QueryEscape(test.code)); response.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
}

func TestSharedEntryKeepsTenantStaticCachePolicy(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	for _, test := range []struct {
		name, target, cache string
	}{
		{name: "tenant static asset", target: "/organizations/org_alpha/static/fonts/Geist-Variable.woff2", cache: "no-cache"},
		{name: "tenant page", target: "/organizations/org_alpha/organization", cache: "no-store"},
		{name: "missing tenant static asset", target: "/organizations/org_alpha/static/fonts/absent.woff2", cache: "no-store"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, _ := alice.Get(test.target)
			if got := response.Header.Get("Cache-Control"); got != test.cache {
				t.Fatalf("GET %s Cache-Control = %q (status %d), want %q", test.target, got, response.StatusCode, test.cache)
			}
			if policy := response.Header.Get("Content-Security-Policy"); policy != contentSecurity {
				t.Fatalf("GET %s content security = %q", test.target, policy)
			}
		})
	}
}

func TestEntryAuthDependencyOutages(t *testing.T) {
	for _, table := range []string{"sessions", "authorizations", "platform_members", "transactions", "keyring"} {
		t.Run(table, func(t *testing.T) {
			admin := entryPostgresDatabase(t)
			f := newEntryFixture(t, entryPostgresConfig(t, admin), func(cfg *Config) { cfg.LookupEnv = entrySecretEnvironment(1, 1) })
			b := newBrowser(t, f.service.Handler())
			b.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			other := newBrowser(t, f.service.Handler())
			other.Login("/organizations/org_beta/organization", "user_bob:porg_beta")
			start, _ := b.Get("/auth/oidc/start?organization=org_alpha")
			callback := "/auth/oidc/callback?" + url.Values{"state": {stateOf(t, start)}, "code": {"user_alice:porg_alpha"}}.Encode()
			if table == "keyring" {
				var err error
				f.service.auth.keys, err = hubsecrets.FromEnvironment(entrySecretEnvironment(2, 2))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE platform."+table+" RENAME TO example_auth_unavailable"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := admin.ExecContext(context.WithoutCancel(t.Context()), "ALTER TABLE platform.example_auth_unavailable RENAME TO "+table); err != nil {
						t.Error(err)
					}
				})
			}
			targets := []string{"/organizations/org_alpha/app/bootstrap"}
			if table == "transactions" {
				targets = []string{callback}
			} else if table != "authorizations" && table != "keyring" {
				targets = append(targets, "/api/cloud/session", "/api/cloud/account/api-keys", "/organizations", "/platform", "/support", "/alpha/work")
			}
			for _, target := range targets {
				response, _ := b.Get(target)
				if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("WWW-Authenticate") != "" || response.Header.Get("Location") != "" {
					t.Errorf("GET %s = %d challenge %q redirect %q; want 503 without credential challenge or redirect", target, response.StatusCode, response.Header.Get("WWW-Authenticate"), response.Header.Get("Location"))
				}
			}
			if table == "keyring" {
				var revoked int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM platform.authorizations WHERE revoked_at IS NOT NULL").Scan(&revoked); err != nil || revoked != 0 {
					t.Fatalf("keyring outage revoked authorizations = %d, error %v", revoked, err)
				}
			}
		})
	}
}

type entrySessionFailureProvider struct {
	auth.HostedProvider
	failure error
}

func (p entrySessionFailureProvider) CurrentSession(context.Context, auth.HostedIdentity) (auth.HostedIdentity, error) {
	return auth.HostedIdentity{}, p.failure
}

func (p entrySessionFailureProvider) Exchange(context.Context, string, string, string) (auth.Identity, error) {
	return auth.Identity{}, p.failure
}

func TestEntryProviderSessionFailures(t *testing.T) {
	f := newEntryFixture(t)
	b := newBrowser(t, f.service.Handler())
	b.Login("/organizations", "user_alice:")
	provider := f.service.config.Provider
	for _, test := range []struct {
		name    string
		failure error
		want    int
	}{
		{"unavailable", &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}, http.StatusServiceUnavailable},
		{"cancelled", context.Canceled, http.StatusServiceUnavailable},
		{"revoked", &auth.HostedIdentityError{Reason: auth.HostedReasonSessionNotFound}, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.service.config.Provider = entrySessionFailureProvider{HostedProvider: provider, failure: test.failure}
			response := b.Do(http.MethodPost, "/api/cloud/mcp", nil, nil)
			if response.StatusCode != test.want || response.Header.Get("WWW-Authenticate") != "" {
				t.Fatalf("session failure = %d challenge %q, want %d without challenge", response.StatusCode, response.Header.Get("WWW-Authenticate"), test.want)
			}
			start, _ := b.Get("/auth/oidc/start")
			callback, _ := b.Get("/auth/oidc/callback?" + url.Values{"state": {stateOf(t, start)}, "code": {"example_code"}}.Encode())
			if callback.StatusCode != test.want || callback.Header.Get("WWW-Authenticate") != "" {
				t.Fatalf("exchange failure = %d challenge %q, want %d without challenge", callback.StatusCode, callback.Header.Get("WWW-Authenticate"), test.want)
			}
		})
	}
}

func TestEntryLoginKeyOutagePreservesAuthorizations(t *testing.T) {
	admin := entryPostgresDatabase(t)
	f := newEntryFixture(t, entryPostgresConfig(t, admin), func(cfg *Config) { cfg.LookupEnv = entrySecretEnvironment(1, 1) })
	b := newBrowser(t, f.service.Handler())
	b.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	var err error
	f.service.auth.keys, err = hubsecrets.FromEnvironment(entrySecretEnvironment(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	start, _ := b.Get("/auth/oidc/start?organization=org_alpha")
	response, _ := b.Get("/auth/oidc/callback?" + url.Values{"state": {stateOf(t, start)}, "code": {"user_alice:porg_alpha"}}.Encode())
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Location") != "" {
		t.Errorf("replacement during key outage = %d redirect %q, want 503 without redirect", response.StatusCode, response.Header.Get("Location"))
	}
	var revoked int
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM platform.sessions WHERE revoked_at IS NOT NULL)+(SELECT count(*) FROM platform.authorizations WHERE revoked_at IS NOT NULL)").Scan(&revoked); err != nil || revoked != 0 {
		t.Fatalf("replacement during key outage revoked %d sessions/authorizations, error %v", revoked, err)
	}
}
