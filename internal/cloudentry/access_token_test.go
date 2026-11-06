package cloudentry

import (
	"bytes"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
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

func TestSharedEntryAccessTokenVerification(t *testing.T) {

	t.Parallel()
	const target = "/api/v2/organizations/org_alpha/projects"
	tests := []struct {
		name string
		run  func(t *testing.T, f entryFixture, alice *browser)
	}{
		{"valid token makes no provider calls for reads or writes", func(t *testing.T, f entryFixture, alice *browser) {
			_, organizationPage := alice.get("/organizations/org_alpha/app/bootstrap")
			before := f.provider.providerCalls()
			for range 5 {
				if response, body := alice.get(target); response.StatusCode != http.StatusOK {
					t.Fatalf("read status = %d: %s", response.StatusCode, body)
				}
			}
			createEntryTestProject(t, alice, "Token project", csrfFrom(t, organizationPage))
			if calls := f.provider.providerCalls() - before; calls != 0 {
				t.Fatalf("provider calls = %d, want 0", calls)
			}
		}},
		{"expired token refreshes once then verifies locally", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.expireAccess()
			before := f.provider.providerCalls()
			if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("first read status = %d", response.StatusCode)
			}
			if calls := f.provider.providerCalls() - before; calls != 1 || f.provider.refreshes != 1 {
				t.Fatalf("provider calls after expiry = %d (refreshes %d), want one refresh", calls, f.provider.refreshes)
			}
			for range 3 {
				if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
					t.Fatalf("read status = %d", response.StatusCode)
				}
			}
			if calls := f.provider.providerCalls() - before; calls != 1 {
				t.Fatalf("provider calls after the refresh = %d, want 1", calls)
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
					if response, body := alice.get("/api/v2/organizations/" + organization + "/projects"); response.StatusCode != http.StatusOK {
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
			if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("read before expiry = %d, want 200 until the access token expires", response.StatusCode)
			}
			f.provider.expireAccess()
			if response, _ := alice.get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked API read = %d, want 401", response.StatusCode)
			}
			events := alice.do(http.MethodGet, "/organizations/org_alpha/projects/prj_example/events", nil, map[string]string{
				"Accept": "text/event-stream", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
			})
			if events.StatusCode != http.StatusUnauthorized || events.Header.Get("Location") != "" || len(events.Header.Values("Set-Cookie")) != 0 {
				t.Fatalf("revoked event stream = %d %v, want 401 without redirect or cookies", events.StatusCode, events.Header)
			}
			response, _ := alice.get("/organizations/org_alpha/app/bootstrap")
			if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start") {
				t.Fatalf("revoked page read = %d %q, want sign-in redirect", response.StatusCode, response.Header.Get("Location"))
			}
			if f.provider.refreshes != 1 {
				t.Fatalf("refreshes = %d, want 1: the dropped authorization must not refresh again", f.provider.refreshes)
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
			if response, _ := alice.get("/api/v2/organizations/org_beta/projects"); response.StatusCode != http.StatusOK {
				t.Fatalf("remaining membership status = %d, want 200", response.StatusCode)
			}
			response, _ := alice.get("/organizations/org_alpha/app/bootstrap")
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
			if response, body := alice.get(html.UnescapeString(link[1])); response.StatusCode != http.StatusPermanentRedirect || response.Header.Get("Location") == "" {
				t.Fatalf("browser retry = %d: %s", response.StatusCode, body)
			}
			if response, body := alice.get("/organizations"); response.StatusCode != http.StatusOK || !strings.Contains(body, `/organizations/org_alpha/organization`) {
				t.Fatalf("return to chooser = %d: %s", response.StatusCode, body)
			}
			if response, body := alice.get("/organizations/org_alpha/app/bootstrap"); response.StatusCode != http.StatusOK {
				t.Fatalf("reselect organization = %d: %s", response.StatusCode, body)
			}
			if issued := f.provider.sequence - before; issued != 0 || f.provider.refreshes != 0 {
				t.Fatalf("outage recovery issued tokens = %d, refreshes = %d", issued, f.provider.refreshes)
			}
		}},
		{"concurrent requests on an expired token refresh once", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.mu.Lock()
			f.provider.sharedSessions = true
			f.provider.mu.Unlock()
			alice.login("/organizations/org_beta/organization", "user_alice:porg_beta")
			for index, organization := range []string{"org_alpha", "org_beta"} {
				f.provider.expireAccess()
				start := make(chan struct{})
				var wg sync.WaitGroup
				statuses := make([]int, 8)
				for i := range statuses {
					wg.Go(func() {
						<-start
						statuses[i] = alice.do(http.MethodGet, "/api/v2/organizations/"+organization+"/projects", nil, nil).StatusCode
					})
				}
				close(start)
				wg.Wait()
				for i, status := range statuses {
					if status != http.StatusOK {
						t.Fatalf("tenant %s request %d status = %d", organization, i, status)
					}
				}
				if f.provider.refreshes != index+1 {
					t.Fatalf("refreshes = %d, want %d", f.provider.refreshes, index+1)
				}
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

func TestSharedEntryProxyLogsLatencyWithoutIdentity(t *testing.T) {

	t.Parallel()
	var logs lockedBuffer
	f := newEntryFixtureWithLogger(t, slog.New(slog.NewTextHandler(&logs, nil)))
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	f.provider.expireAccess()
	alice.get("/organizations/org_alpha/organization?tab=x")
	_, organizationPage := alice.get("/organizations/org_alpha/app/bootstrap")
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
