package sessions_test

import (
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entryfixture"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/harness"
)

func TestSharedEntryAccessTokenVerification(t *testing.T) {

	t.Parallel()
	const target = "/api/v2/organizations/org_alpha/projects"
	tests := []struct {
		name string
		run  func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser)
	}{
		{"valid token makes no provider calls for reads or writes", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			_, organizationPage := alice.Get("/organizations/org_alpha/app/bootstrap")
			before := f.Provider.ProviderCalls()
			for range 5 {
				if response, body := alice.Get(target); response.StatusCode != http.StatusOK {
					t.Fatalf("read status = %d: %s", response.StatusCode, body)
				}
			}
			harness.CreateProject(t, alice, "Token project", harness.CSRF(t, organizationPage))
			if calls := f.Provider.ProviderCalls() - before; calls != 0 {
				t.Fatalf("provider calls = %d, want 0", calls)
			}
		}},
		{"expired token refreshes once then verifies locally", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			f.Provider.ExpireAccess()
			before := f.Provider.ProviderCalls()
			if response, _ := alice.Get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("first read status = %d", response.StatusCode)
			}
			if calls := f.Provider.ProviderCalls() - before; calls != 1 || f.Provider.Refreshes != 1 {
				t.Fatalf("provider calls after expiry = %d (refreshes %d), want one refresh", calls, f.Provider.Refreshes)
			}
			for range 3 {
				if response, _ := alice.Get(target); response.StatusCode != http.StatusOK {
					t.Fatalf("read status = %d", response.StatusCode)
				}
			}
			if calls := f.Provider.ProviderCalls() - before; calls != 1 {
				t.Fatalf("provider calls after the refresh = %d, want 1", calls)
			}
		}},
		{"shared session refreshes alternating organizations across token lifetimes", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			f.Provider.Mu.Lock()
			f.Provider.SharedSessions = true
			f.Provider.Mu.Unlock()
			alice.Login("/organizations/org_beta/organization", "user_alice:porg_beta")
			for range 3 {
				f.Provider.ExpireAccess()
				for _, organization := range []string{"org_alpha", "org_beta"} {
					if response, body := alice.Get("/api/v2/organizations/" + organization + "/projects"); response.StatusCode != http.StatusOK {
						t.Fatalf("%s read after expiry = %d: %s", organization, response.StatusCode, body)
					}
				}
			}
			if f.Provider.Refreshes != 6 {
				t.Fatalf("refreshes = %d, want 6", f.Provider.Refreshes)
			}
		}},
		{"refresh returning another organization drops the authorization", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			f.Provider.Mu.Lock()
			f.Provider.RefreshOrg = "porg_beta"
			f.Provider.Mu.Unlock()
			f.Provider.ExpireAccess()
			if response, _ := alice.Get(target); response.StatusCode != http.StatusForbidden {
				t.Fatalf("mismatched organization status = %d, want 403", response.StatusCode)
			}
			if response, _ := alice.Get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("read after authorization drop = %d, want 401", response.StatusCode)
			}
			if f.Provider.Refreshes != 1 {
				t.Fatalf("refreshes = %d, want 1", f.Provider.Refreshes)
			}
		}},
		{"revoked session signs the browser out", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			f.Provider.EndSessions("user_alice")
			if response, _ := alice.Get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("read before expiry = %d, want 200 until the access token expires", response.StatusCode)
			}
			f.Provider.ExpireAccess()
			if response, _ := alice.Get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked API read = %d, want 401", response.StatusCode)
			}
			events := alice.Do(http.MethodGet, "/organizations/org_alpha/projects/prj_example/events", nil, map[string]string{
				"Accept": "text/event-stream", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty",
			})
			if events.StatusCode != http.StatusUnauthorized || events.Header.Get("Location") != "" || len(events.Header.Values("Set-Cookie")) != 0 {
				t.Fatalf("revoked event stream = %d %v, want 401 without redirect or cookies", events.StatusCode, events.Header)
			}
			response, _ := alice.Get("/organizations/org_alpha/app/bootstrap")
			if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start") {
				t.Fatalf("revoked page read = %d %q, want sign-in redirect", response.StatusCode, response.Header.Get("Location"))
			}
			if f.Provider.Refreshes != 1 {
				t.Fatalf("refreshes = %d, want 1: the dropped authorization must not refresh again", f.Provider.Refreshes)
			}
		}},
		{"removed member is refused at the next refresh", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			f.Provider.Mu.Lock()
			f.Provider.SharedSessions = true
			f.Provider.Mu.Unlock()
			alice.Login("/organizations/org_beta/organization", "user_alice:porg_beta")
			f.Provider.RemoveMember("user_alice", "porg_alpha")
			f.Provider.ExpireAccess()
			if response, _ := alice.Get(target); response.StatusCode != http.StatusForbidden {
				t.Fatalf("removed member status = %d, want 403", response.StatusCode)
			}
			before := f.Provider.ProviderCalls()
			if response, _ := alice.Get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("removed member read after authorization drop = %d, want 401", response.StatusCode)
			}
			if calls := f.Provider.ProviderCalls() - before; calls != 0 {
				t.Fatalf("provider calls after the drop = %d, want 0", calls)
			}
			if response, _ := alice.Get("/api/v2/organizations/org_beta/projects"); response.StatusCode != http.StatusOK {
				t.Fatalf("remaining membership status = %d, want 200", response.StatusCode)
			}
			response, _ := alice.Get("/organizations/org_alpha/app/bootstrap")
			if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start") {
				t.Fatalf("removed member page = %d %q, want sign-in redirect", response.StatusCode, response.Header.Get("Location"))
			}
		}},
		{"unreachable key set keeps the authorization", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			const chosen = "/organizations/org_alpha/organization?tab=work&view=board"
			if response, body := alice.Get("/organizations"); response.StatusCode != http.StatusOK || !strings.Contains(body, `/organizations/org_alpha/organization`) {
				t.Fatalf("chooser before the outage = %d: %s", response.StatusCode, body)
			}
			before := f.Provider.Sequence
			f.Provider.Mu.Lock()
			f.Provider.KeysDown = true
			f.Provider.Mu.Unlock()
			if response, body := alice.Get(target); response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"unavailable"`) {
				t.Fatalf("API with the key set down = %d: %s", response.StatusCode, body)
			}
			response, body := alice.Get(chosen)
			if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Temporarily unavailable") {
				t.Fatalf("browser outage = %d: %s", response.StatusCode, body)
			}
			harness.AssertEntryFallback(t, alice, body)
			for _, denial := range []string{"Access unavailable", "unavailable to your account", "access may have changed", "Sign in again"} {
				if strings.Contains(body, denial) {
					t.Fatalf("browser outage claims access was lost: %s", body)
				}
			}
			link := regexp.MustCompile(`href="([^"]+)"[^>]*>Try again</a>`).FindStringSubmatch(body)
			if len(link) != 2 || html.UnescapeString(link[1]) != chosen {
				t.Fatalf("retry link = %v, want %s", link, chosen)
			}
			f.Provider.Mu.Lock()
			f.Provider.KeysDown = false
			f.Provider.Mu.Unlock()
			if response, _ := alice.Get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("status after the key set recovered = %d, want 200", response.StatusCode)
			}
			if response, body := alice.Get(html.UnescapeString(link[1])); response.StatusCode != http.StatusOK || !strings.Contains(body, "Your organization") || !strings.Contains(body, "Alpha secret project") || !strings.Contains(body, "/organizations/org_alpha/") {
				t.Fatalf("browser retry = %d: %s", response.StatusCode, body)
			}
			if response, body := alice.Get("/organizations"); response.StatusCode != http.StatusOK || !strings.Contains(body, `/organizations/org_alpha/organization`) {
				t.Fatalf("return to chooser = %d: %s", response.StatusCode, body)
			}
			if response, body := alice.Get("/organizations/org_alpha/app/bootstrap"); response.StatusCode != http.StatusOK {
				t.Fatalf("reselect organization = %d: %s", response.StatusCode, body)
			}
			if issued := f.Provider.Sequence - before; issued != 0 || f.Provider.Refreshes != 0 {
				t.Fatalf("outage recovery issued tokens = %d, refreshes = %d", issued, f.Provider.Refreshes)
			}
		}},
		{"concurrent requests on an expired token refresh once", func(t *testing.T, f entryfixture.Fixture, alice *harness.Browser) {
			f.Provider.Mu.Lock()
			f.Provider.SharedSessions = true
			f.Provider.Mu.Unlock()
			alice.Login("/organizations/org_beta/organization", "user_alice:porg_beta")
			for index, organization := range []string{"org_alpha", "org_beta"} {
				f.Provider.ExpireAccess()
				start := make(chan struct{})
				var wg sync.WaitGroup
				statuses := make([]int, 8)
				for i := range statuses {
					wg.Go(func() {
						<-start
						statuses[i] = alice.Do(http.MethodGet, "/api/v2/organizations/"+organization+"/projects", nil, nil).StatusCode
					})
				}
				close(start)
				wg.Wait()
				for i, status := range statuses {
					if status != http.StatusOK {
						t.Fatalf("tenant %s request %d status = %d", organization, i, status)
					}
				}
				if f.Provider.Refreshes != index+1 {
					t.Fatalf("refreshes = %d, want %d", f.Provider.Refreshes, index+1)
				}
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := entryfixture.New(t)
			alice := harness.NewBrowser(t, f.Service.Handler())
			alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			test.run(t, f, alice)
		})
	}
}

func TestSharedEntryProxyLogsLatencyWithoutIdentity(t *testing.T) {

	t.Parallel()
	var logs entrytest.LockedBuffer
	f := entryfixture.NewWithLogger(t, slog.New(slog.NewTextHandler(&logs, nil)))
	alice := harness.NewBrowser(t, f.Service.Handler())
	alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	f.Provider.ExpireAccess()
	alice.Get("/organizations/org_alpha/organization?tab=x")
	_, organizationPage := alice.Get("/organizations/org_alpha/app/bootstrap")
	path := "/api/v2/organizations/org_alpha/runner-enrollments"
	request := httptest.NewRequest(http.MethodPost, alice.Origin+path, strings.NewReader(`{"scope":"projects","project_ids":["project_unknown"],"operations":["read"],"ttl_seconds":900}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", alice.Origin)
	request.Header.Set("X-CSRF-Token", harness.CSRF(t, organizationPage))
	for _, cookie := range alice.Jar.Cookies(request.URL) {
		request.AddCookie(cookie)
	}
	rejected := httptest.NewRecorder()
	f.Service.Handler().ServeHTTP(rejected, request)
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid enrollment status = %d: %s", rejected.Code, rejected.Body.String())
	}
	if !strings.Contains(rejected.Body.String(), "Enrollment projects must be unique and belong to the organization") {
		t.Fatalf("enrollment reason was lost: %s", rejected.Body.String())
	}
	privateField := "private-token@example.test"
	request = httptest.NewRequest(http.MethodPost, alice.Origin+path, strings.NewReader(`{"`+privateField+`":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", alice.Origin)
	request.Header.Set("X-CSRF-Token", harness.CSRF(t, organizationPage))
	for _, cookie := range alice.Jar.Cookies(request.URL) {
		request.AddCookie(cookie)
	}
	rejected = httptest.NewRecorder()
	f.Service.Handler().ServeHTTP(rejected, request)
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
