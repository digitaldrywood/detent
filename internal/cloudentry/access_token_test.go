package cloudentry

import (
	"bytes"
	"encoding/json"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// endSessions revokes a user's provider sessions without the entry's
// knowledge, as an administrator revoking them in WorkOS would.
func (p *fakeProvider) endSessions(user string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, session := range p.sessions {
		if session.Subject == user {
			delete(p.sessions, id)
		}
	}
}

func TestSharedEntryAccountSessionValidity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		staff      bool
		refresh    bool
		entryFirst bool
	}{
		{name: "customer token proxy first"},
		{name: "customer token entry first", entryFirst: true},
		{name: "customer refresh proxy first", refresh: true},
		{name: "customer refresh entry first", refresh: true, entryFirst: true},
		{name: "admin token proxy first", staff: true},
		{name: "admin token entry first", staff: true, entryFirst: true},
		{name: "admin refresh proxy first", staff: true, refresh: true},
		{name: "admin refresh entry first", staff: true, refresh: true, entryFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			if test.staff {
				f.provider.users["user_alice"] = "bootstrap@example.test"
			}
			alice := newBrowser(t, f.service.Handler())
			alice.login("/auth/oidc/start?organization=org_alpha", "user_alice:porg_alpha")
			alice.login("/auth/oidc/start?organization=org_beta", "user_alice:porg_beta")
			key := createTestAccessKey(t, alice, "global", nil, "")
			alice.login("/auth/oidc/start", "user_alice:")
			base, _ := url.Parse(testPublicURL)
			var session accountSession
			for _, cookie := range alice.jar.Cookies(base) {
				if cookie.Name == f.service.cookieName("session") {
					var err error
					session, err = f.service.auth.session(t.Context(), apikey.HashToken(cookie.Value))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if session.Hash == "" {
				t.Fatal("missing account session")
			}
			f.provider.mu.Lock()
			delete(f.provider.sessions, session.Identity.SessionID)
			f.provider.mu.Unlock()
			for _, organization := range []string{"org_alpha", "org_beta"} {
				authorized, err := f.service.auth.authorization(t.Context(), session, organization)
				if err != nil || authorized.Identity.SessionID == session.Identity.SessionID {
					t.Fatalf("carried authorization %s = %+v, %v", organization, authorized.Identity, err)
				}
				tokens, err := f.service.auth.tokens(authorized)
				if err != nil {
					t.Fatal(err)
				}
				access, rotated, err := f.provider.RefreshAccess(t.Context(), tokens.RefreshToken)
				if err != nil || access.SessionID != authorized.Identity.SessionID {
					t.Fatalf("carried session cannot refresh: %+v, %v", access, err)
				}
				if err := f.service.auth.storeTokens(t.Context(), authorized.Binding, rotated); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.provider.CurrentSession(t.Context(), session.Identity); !errors.Is(err, auth.ErrHostedIdentity) {
				t.Fatalf("ended account session = %v", err)
			}
			if test.refresh {
				f.provider.expireAccess()
			}
			before := f.provider.providerCalls()
			for _, credential := range []struct{ path, token string }{
				{"/organizations/org_alpha/api/cloud/metadata", testAdminKey},
				{"/api/v2/organizations/org_alpha/projects", key.Token},
			} {
				if response := alice.do(http.MethodGet, credential.path, nil, map[string]string{"Authorization": "Bearer " + credential.token}); response.StatusCode != http.StatusOK {
					t.Fatalf("credential proxy = %d: %s", response.StatusCode, response.Body)
				}
			}
			if calls := f.provider.providerCalls() - before; calls != 1 {
				t.Fatalf("credential provider calls = %d, want only the API key membership check", calls)
			}
			entry := func() {
				for _, path := range []string{"/api/cloud/session", "/api/cloud/organizations"} {
					if response, body := alice.get(path); response.StatusCode != http.StatusUnauthorized {
						t.Fatalf("entry %s = %d: %s", path, response.StatusCode, body)
					}
				}
			}
			if test.entryFirst {
				entry()
			}
			for _, organization := range []string{"org_alpha", "org_beta"} {
				if response, body := alice.get("/api/v2/organizations/" + organization + "/usage"); response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("proxy %s = %d: %s", organization, response.StatusCode, body)
				}
				if response, _ := alice.get("/organizations/" + organization + "/organization"); response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start") {
					t.Fatalf("org page %s = %d, %q", organization, response.StatusCode, response.Header.Get("Location"))
				}
			}
			entry()
		})
	}
}

func TestSharedEntryAccessTokenVerification(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	const target = "/api/v2/organizations/org_alpha/usage"
	tests := []struct {
		name string
		run  func(t *testing.T, f entryFixture, alice *browser)
	}{
		{"valid token reuses session checks for reads and rechecks mutations", func(t *testing.T, f entryFixture, alice *browser) {
			_, organizationPage := alice.get("/organizations/org_alpha/organization")
			before := f.provider.providerCalls()
			for range 5 {
				if response, body := alice.get(target); response.StatusCode != http.StatusOK {
					t.Fatalf("read status = %d: %s", response.StatusCode, body)
				}
			}
			created := alice.do(http.MethodPost, "/organizations/org_alpha/projects", url.Values{"name": {"Token project"}, "grant_access": {"true"}, "csrf": {csrfFrom(t, organizationPage)}}, nil)
			if created.StatusCode != http.StatusSeeOther {
				t.Fatalf("mutation status = %d: %s", created.StatusCode, created.Body)
			}
			if calls := f.provider.providerCalls() - before; calls != 1 {
				t.Fatalf("provider calls = %d, want one session check for the mutation", calls)
			}
		}},
		{"expired token refreshes once then verifies locally", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.expireAccess()
			before := f.provider.providerCalls()
			if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("first read status = %d", response.StatusCode)
			}
			if calls := f.provider.providerCalls() - before; calls != 2 || f.provider.refreshes != 1 {
				t.Fatalf("provider calls after expiry = %d (refreshes %d), want one session check and one refresh", calls, f.provider.refreshes)
			}
			for range 3 {
				if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
					t.Fatalf("read status = %d", response.StatusCode)
				}
			}
			if calls := f.provider.providerCalls() - before; calls != 2 {
				t.Fatalf("provider calls after the refresh = %d, want 2", calls)
			}
		}},
		{"shared session refreshes alternating organizations across token lifetimes", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.mu.Lock()
			f.provider.sharedSessions = true
			f.provider.mu.Unlock()
			alice.login("/organizations/org_beta/organization", "user_alice:porg_beta")
			for range 3 {
				f.provider.expireAccess()
				for _, organization := range []string{"org_alpha", "org_beta"} {
					if response, body := alice.get("/api/v2/organizations/" + organization + "/usage"); response.StatusCode != http.StatusOK {
						t.Fatalf("%s read after expiry = %d: %s", organization, response.StatusCode, body)
					}
				}
			}
			if f.provider.refreshes != 6 {
				t.Fatalf("refreshes = %d, want 6", f.provider.refreshes)
			}
		}},
		{"refresh returning another organization drops the authorization", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.mu.Lock()
			f.provider.refreshOrg = "porg_beta"
			f.provider.mu.Unlock()
			f.provider.expireAccess()
			if response, _ := alice.get(target); response.StatusCode != http.StatusForbidden {
				t.Fatalf("mismatched organization status = %d, want 403", response.StatusCode)
			}
			if response, _ := alice.get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("read after authorization drop = %d, want 401", response.StatusCode)
			}
			if f.provider.refreshes != 1 {
				t.Fatalf("refreshes = %d, want 1", f.provider.refreshes)
			}
		}},
		{"revoked session signs the browser out", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.endSessions("user_alice")
			if response, _ := alice.get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked API read = %d, want 401", response.StatusCode)
			}
			events := alice.do(http.MethodGet, "/organizations/org_alpha/projects/prj_example/events", nil, map[string]string{
				"Accept": "text/event-stream", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
			})
			if events.StatusCode != http.StatusUnauthorized || events.Header.Get("Location") != "" || len(events.Header.Values("Set-Cookie")) != 0 {
				t.Fatalf("revoked event stream = %d %v, want 401 without redirect or cookies", events.StatusCode, events.Header)
			}
			response, _ := alice.get("/organizations/org_alpha/organization")
			if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start") {
				t.Fatalf("revoked page read = %d %q, want sign-in redirect", response.StatusCode, response.Header.Get("Location"))
			}
			if response, _ := alice.get("/api/cloud/session"); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked entry read = %d, want 401", response.StatusCode)
			}
			if f.provider.refreshes != 0 {
				t.Fatalf("refreshes = %d, want 0 for an ended session", f.provider.refreshes)
			}
		}},
		{"removed member is refused at the next refresh", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.mu.Lock()
			f.provider.sharedSessions = true
			f.provider.mu.Unlock()
			alice.login("/organizations/org_beta/organization", "user_alice:porg_beta")
			f.provider.removeMember("user_alice", "porg_alpha")
			f.provider.expireAccess()
			if response, _ := alice.get(target); response.StatusCode != http.StatusForbidden {
				t.Fatalf("removed member status = %d, want 403", response.StatusCode)
			}
			before := f.provider.providerCalls()
			if response, _ := alice.get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("removed member read after authorization drop = %d, want 401", response.StatusCode)
			}
			if calls := f.provider.providerCalls() - before; calls != 0 {
				t.Fatalf("provider calls after the drop = %d, want 0", calls)
			}
			if response, _ := alice.get("/api/v2/organizations/org_beta/usage"); response.StatusCode != http.StatusOK {
				t.Fatalf("remaining membership status = %d, want 200", response.StatusCode)
			}
			response, _ := alice.get("/organizations/org_alpha/organization")
			if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start") {
				t.Fatalf("removed member page = %d %q, want sign-in redirect", response.StatusCode, response.Header.Get("Location"))
			}
		}},
		{"unreachable key set keeps the authorization", func(t *testing.T, f entryFixture, alice *browser) {
			const chosen = "/organizations/org_alpha/organization?tab=work&view=board"
			if response, body := alice.get("/organizations"); response.StatusCode != http.StatusOK || !strings.Contains(body, `/organizations/org_alpha/organization`) {
				t.Fatalf("chooser before the outage = %d: %s", response.StatusCode, body)
			}
			before := f.provider.sequence
			f.provider.mu.Lock()
			f.provider.keysDown = true
			f.provider.mu.Unlock()
			if response, body := alice.get(target); response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"unavailable"`) {
				t.Fatalf("API with the key set down = %d: %s", response.StatusCode, body)
			}
			response, body := alice.get(chosen)
			if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Temporarily unavailable") {
				t.Fatalf("browser outage = %d: %s", response.StatusCode, body)
			}
			assertEntryFallback(t, alice, body)
			for _, denial := range []string{"Access unavailable", "unavailable to your account", "access may have changed", "Sign in again"} {
				if strings.Contains(body, denial) {
					t.Fatalf("browser outage claims access was lost: %s", body)
				}
			}
			link := regexp.MustCompile(`href="([^"]+)"[^>]*>Try again</a>`).FindStringSubmatch(body)
			if len(link) != 2 || html.UnescapeString(link[1]) != chosen {
				t.Fatalf("retry link = %v, want %s", link, chosen)
			}
			f.provider.mu.Lock()
			f.provider.keysDown = false
			f.provider.mu.Unlock()
			if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("status after the key set recovered = %d, want 200", response.StatusCode)
			}
			if response, body := alice.get(html.UnescapeString(link[1])); response.StatusCode != http.StatusOK {
				t.Fatalf("browser retry = %d: %s", response.StatusCode, body)
			}
			if response, body := alice.get("/organizations"); response.StatusCode != http.StatusOK || !strings.Contains(body, `/organizations/org_alpha/organization`) {
				t.Fatalf("return to chooser = %d: %s", response.StatusCode, body)
			}
			if response, body := alice.get("/organizations/org_alpha/organization"); response.StatusCode != http.StatusOK {
				t.Fatalf("reselect organization = %d: %s", response.StatusCode, body)
			}
			if issued := f.provider.sequence - before; issued != 0 || f.provider.refreshes != 0 {
				t.Fatalf("outage recovery issued tokens = %d, refreshes = %d", issued, f.provider.refreshes)
			}
		}},
		{"concurrent requests on an expired token refresh once", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.expireAccess()
			f.provider.mu.Lock()
			f.provider.refreshDelay = 50 * time.Millisecond
			f.provider.mu.Unlock()
			var wg sync.WaitGroup
			statuses := make([]int, 8)
			for i := range statuses {
				wg.Go(func() {
					statuses[i] = alice.do(http.MethodGet, target, nil, nil).StatusCode
				})
			}
			wg.Wait()
			for i, status := range statuses {
				if status != http.StatusOK {
					t.Fatalf("request %d status = %d", i, status)
				}
			}
			if f.provider.refreshes != 1 {
				t.Fatalf("refreshes = %d, want 1", f.provider.refreshes)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			alice := newBrowser(t, f.service.Handler())
			alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			test.run(t, f, alice)
		})
	}
}

func TestSharedEntrySupportSessionValidity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		wantProxy   int
		wantAccount int
	}{
		{"active", http.StatusOK, http.StatusOK},
		{"ended staff", http.StatusUnauthorized, http.StatusUnauthorized},
		{"ended impersonation", http.StatusUnauthorized, http.StatusOK},
		{"expired impersonation", http.StatusUnauthorized, http.StatusOK},
		{"one hour cap", http.StatusUnauthorized, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			support := newBrowser(t, f.service.Handler())
			support.login("/auth/oidc/start", "user_support:")
			_, page := support.get("/support")
			if response := support.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "reason": {"customer-request"}, "csrf": {csrfFrom(t, page)}}, nil); response.StatusCode != http.StatusOK {
				t.Fatalf("start support = %d", response.StatusCode)
			}
			if response, _ := support.get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request")); response.StatusCode != http.StatusSeeOther {
				t.Fatalf("support callback = %d", response.StatusCode)
			}
			before := f.provider.verifications
			for range 3 {
				if response, _ := support.get("/api/v2/organizations/org_alpha/usage"); response.StatusCode != http.StatusOK {
					t.Fatalf("active support = %d", response.StatusCode)
				}
			}
			if calls := f.provider.verifications - before; calls != 6 {
				t.Fatalf("support verifications = %d, want both sessions rechecked on all three requests", calls)
			}
			var capped auth.HostedIdentity
			f.provider.mu.Lock()
			for id, identity := range f.provider.sessions {
				switch {
				case test.name == "ended staff" && identity.Subject == "user_support":
					delete(f.provider.sessions, id)
				case identity.SupportActor != "" && test.name == "ended impersonation":
					delete(f.provider.sessions, id)
				case identity.SupportActor != "" && test.name == "expired impersonation":
					identity.ExpiresAt = time.Now().Add(-time.Second)
					f.provider.sessions[id] = identity
				case identity.SupportActor != "" && test.name == "one hour cap":
					identity.CreatedAt = time.Now().Add(-time.Hour)
					identity.ExpiresAt = time.Now().Add(time.Hour)
					f.provider.sessions[id] = identity
					capped = identity
				}
			}
			f.provider.mu.Unlock()
			if capped.SessionID != "" {
				encoded, err := json.Marshal(capped)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE authorizations SET identity_json = ? WHERE support = 1", string(encoded)); err != nil {
					t.Fatal(err)
				}
			}
			if response, _ := support.get("/api/v2/organizations/org_alpha/usage"); response.StatusCode != test.wantProxy {
				t.Fatalf("support proxy = %d, want %d", response.StatusCode, test.wantProxy)
			}
			if response, _ := support.get("/api/cloud/session"); response.StatusCode != test.wantAccount {
				t.Fatalf("staff account = %d, want %d", response.StatusCode, test.wantAccount)
			}
		})
	}
}

func TestSharedEntrySessionRecheckAgreement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		advance  time.Duration
		method   string
		shorten  bool
		wantCode int
	}{
		{"cached revocation", providerSessionRecheck - time.Second, http.MethodGet, false, http.StatusOK},
		{"rechecked revocation", providerSessionRecheck, http.MethodGet, false, http.StatusUnauthorized},
		{"mutation rechecks revocation", 0, http.MethodPost, false, http.StatusUnauthorized},
		{"shortened expiry ends cached access", 2 * time.Second, http.MethodGet, true, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newEntryFixture(t)
			alice := newBrowser(t, f.service.Handler())
			alice.login("/auth/oidc/start?organization=org_alpha", "user_alice:porg_alpha")
			clock := time.Now()
			f.service.config.now = func() time.Time { return clock }
			if test.shorten {
				f.provider.mu.Lock()
				for id, identity := range f.provider.sessions {
					identity.ExpiresAt = clock.Add(time.Second)
					f.provider.sessions[id] = identity
				}
				f.provider.mu.Unlock()
			}
			if response, _ := alice.get("/api/cloud/session"); response.StatusCode != http.StatusOK {
				t.Fatalf("initial account check = %d", response.StatusCode)
			}
			if !test.shorten {
				f.provider.endSessions("user_alice")
			}
			clock = clock.Add(test.advance)
			base, _ := url.Parse(testPublicURL)
			request := httptest.NewRequest(test.method, testPublicURL+"/organizations/org_alpha/organization", nil)
			request.Header.Set("Origin", testPublicURL)
			for _, cookie := range alice.jar.Cookies(base) {
				request.AddCookie(cookie)
			}
			c := echo.New().NewContext(request, httptest.NewRecorder())
			organization, err := f.service.registry.Organization(t.Context(), "org_alpha")
			if err != nil {
				t.Fatal(err)
			}
			var claims cloudassert.Claims
			if status, _, err := f.service.browserClaims(c, organization, &claims); status != test.wantCode || (err == nil) != (test.wantCode == http.StatusOK) {
				t.Fatalf("proxy session check = %d, %v", status, err)
			}
			if response, _ := alice.get("/api/cloud/session"); response.StatusCode != test.wantCode {
				t.Fatalf("entry session check = %d, want %d", response.StatusCode, test.wantCode)
			}
		})
	}
}

func TestSharedEntryProxyLogsLatencyWithoutIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	var logs lockedBuffer
	f := newEntryFixtureWithLogger(t, slog.New(slog.NewTextHandler(&logs, nil)))
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	f.provider.expireAccess()
	alice.get("/organizations/org_alpha/organization?tab=x")
	_, organizationPage := alice.get("/organizations/org_alpha/organization")
	path := "/api/v2/organizations/org_alpha/runner-enrollments"
	request := httptest.NewRequest(http.MethodPost, alice.origin+path, strings.NewReader(`{"scope":"projects","project_ids":["project_unknown"],"operations":["read"],"ttl_seconds":900}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", alice.origin)
	request.Header.Set("X-CSRF-Token", csrfFrom(t, organizationPage))
	for _, cookie := range alice.jar.Cookies(request.URL) {
		request.AddCookie(cookie)
	}
	rejected := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(rejected, request)
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid enrollment status = %d: %s", rejected.Code, rejected.Body.String())
	}
	if !strings.Contains(rejected.Body.String(), "Enrollment projects must be unique and belong to the organization") {
		t.Fatalf("enrollment reason was lost: %s", rejected.Body.String())
	}
	privateField := "private-token@example.test"
	request = httptest.NewRequest(http.MethodPost, alice.origin+path, strings.NewReader(`{"`+privateField+`":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", alice.origin)
	request.Header.Set("X-CSRF-Token", csrfFrom(t, organizationPage))
	for _, cookie := range alice.jar.Cookies(request.URL) {
		request.AddCookie(cookie)
	}
	rejected = httptest.NewRecorder()
	f.service.Handler().ServeHTTP(rejected, request)
	if rejected.Code != http.StatusUnprocessableEntity || !strings.Contains(rejected.Body.String(), privateField) {
		t.Fatalf("invalid field response lost its actionable reason: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	output := logs.String()
	if strings.Contains(output, privateField) {
		t.Fatal("proxy logs expose the request-controlled field name")
	}
	for _, want := range []string{"shared entry proxied request", "verification=refreshed", "verification=token", "duration_ms=", "auth_ms=", "status=200"} {
		if !strings.Contains(output, want) {
			t.Fatalf("log lacks %q:\n%s", want, output)
		}
	}
	for _, want := range []string{"path=" + path, "status=422", "error=\"Unprocessable Entity\""} {
		if !strings.Contains(output, want) {
			t.Fatalf("failure log lacks %q:\n%s", want, output)
		}
	}
	for _, secret := range []string{"alice@example.test", "access_", "refresh_", "tab=x", "detent_session"} {
		if strings.Contains(output, secret) {
			t.Fatalf("log exposes %q:\n%s", secret, output)
		}
	}
}
