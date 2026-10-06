package cloudentry

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	const target = "/api/v2/organizations/org_alpha/usage"
	tests := []struct {
		name string
		run  func(t *testing.T, f entryFixture, alice *browser)
	}{
		{"valid token makes no provider calls for reads or writes", func(t *testing.T, f entryFixture, alice *browser) {
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
		{"revoked session signs the browser out", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.endSessions("user_alice")
			if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("read before expiry = %d, want 200 until the access token expires", response.StatusCode)
			}
			f.provider.expireAccess()
			if response, _ := alice.get(target); response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked API read = %d, want 401", response.StatusCode)
			}
			response, _ := alice.get("/organizations/org_alpha/organization")
			if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/auth/oidc/start") {
				t.Fatalf("revoked page read = %d %q, want sign-in redirect", response.StatusCode, response.Header.Get("Location"))
			}
			if f.provider.refreshes != 1 {
				t.Fatalf("refreshes = %d, want 1: the dropped authorization must not refresh again", f.provider.refreshes)
			}
		}},
		{"removed member is refused at the next refresh", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.removeMember("user_alice", "porg_alpha")
			f.provider.expireAccess()
			if response, _ := alice.get(target); response.StatusCode != http.StatusForbidden {
				t.Fatalf("removed member status = %d, want 403", response.StatusCode)
			}
			before := f.provider.providerCalls()
			if response, _ := alice.get(target); response.StatusCode == http.StatusOK {
				t.Fatal("removed member read after the authorization was dropped")
			}
			if calls := f.provider.providerCalls() - before; calls != 0 {
				t.Fatalf("provider calls after the drop = %d, want 0", calls)
			}
		}},
		{"unreachable key set keeps the authorization", func(t *testing.T, f entryFixture, alice *browser) {
			f.provider.mu.Lock()
			f.provider.keysDown = true
			f.provider.mu.Unlock()
			if response, _ := alice.get(target); response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status with the key set down = %d", response.StatusCode)
			}
			f.provider.mu.Lock()
			f.provider.keysDown = false
			f.provider.mu.Unlock()
			if response, _ := alice.get(target); response.StatusCode != http.StatusOK {
				t.Fatalf("status after the key set recovered = %d, want 200", response.StatusCode)
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
	alice.get("/organizations/org_alpha/organization")
	output := logs.String()
	for _, want := range []string{"shared entry proxied request", "verification=refreshed", "verification=token", "duration_ms=", "auth_ms=", "status=200"} {
		if !strings.Contains(output, want) {
			t.Fatalf("log lacks %q:\n%s", want, output)
		}
	}
	for _, secret := range []string{"alice@example.test", "access_", "refresh_", "tab=x", "detent_session"} {
		if strings.Contains(output, secret) {
			t.Fatalf("log exposes %q:\n%s", secret, output)
		}
	}
}
