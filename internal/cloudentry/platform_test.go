//go:build !windows

package cloudentry

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
)

var platformRoutes = []string{"/platform", "/platform/tenants", "/platform/staff", "/platform/audit", "/platform/health", "/platform/allowlist", "/api/cloud/platform/organizations", "/api/cloud/platform/allowlist", "/api/cloud/platform/health", "/api/cloud/platform/audit"}

var platformEvents = map[string]string{
	"/platform":                         "platform_opened",
	"/api/cloud/platform/organizations": "platform_organizations_viewed",
	"/api/cloud/platform/allowlist":     "platform_allowlist_viewed",
	"/api/cloud/platform/health":        "platform_health_viewed",
	"/api/cloud/platform/audit":         "platform_audit_viewed",
}

func withClientShell(s *Service) {
	s.config.clientFS = fstest.MapFS{entryClientShell: {Data: []byte(`<html><head></head><body><div id="root"></div></body></html>`)}}
}

func supportIdentityBrowser(t *testing.T, f entryFixture) *browser {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	hosted := auth.HostedIdentity{Subject: "user_alice", OrganizationID: "porg_alpha", SessionID: "impersonation_session", CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), SupportActor: "support@example.test", SupportReason: "troubleshooting"}
	f.provider.mu.Lock()
	f.provider.sessions[hosted.SessionID] = hosted
	f.provider.mu.Unlock()
	token := "impersonation-session-token"
	if err := f.service.auth.createSession(t.Context(), apikey.HashToken(token), "impersonation-csrf", auth.Identity{Subject: "user_alice", Email: "support@example.test", EmailVerified: true, Hosted: &hosted}); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, f.service.Handler())
	base, _ := url.Parse(testPublicURL)
	b.jar.SetCookies(base, []*http.Cookie{{Name: f.service.cookieName("session"), Value: token, Path: "/"}})
	return b
}

func platformAuditCount(t *testing.T, s *Service, subject, event string) int {
	t.Helper()
	var count int
	if err := s.auth.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit WHERE subject = ? AND event = ?", subject, event).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func decodeJSON(t *testing.T, body string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), target); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func TestPlatformAuthorization(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	f := newEntryFixture(t)
	withClientShell(f.service)
	f.provider.users["user_staff"] = "staff@example.test"
	anonymous := newBrowser(t, f.service.Handler())
	customer := newBrowser(t, f.service.Handler())
	customer.login("/auth/oidc/start", "user_alice:")
	support := newBrowser(t, f.service.Handler())
	support.login("/auth/oidc/start", "user_support:")
	staff := newBrowser(t, f.service.Handler())
	staff.login("/auth/oidc/start", "user_staff:")
	f.provider.member("user_staff", "porg_alpha", "owner")
	scoped := newBrowser(t, f.service.Handler())
	scoped.login("/auth/oidc/start", "user_staff:porg_alpha")
	impersonation := supportIdentityBrowser(t, f)

	for _, route := range platformRoutes {
		page := strings.HasPrefix(route, "/platform")
		for _, test := range []struct {
			name     string
			browser  *browser
			subject  string
			status   int
			location string
		}{
			{"anonymous", anonymous, "", map[bool]int{true: http.StatusSeeOther, false: http.StatusUnauthorized}[page], map[bool]string{true: "/auth/oidc/start?return=%2Fplatform"}[page]},
			{"customer", customer, "user_alice", http.StatusForbidden, ""},
			{"support session identity", impersonation, "user_alice", http.StatusForbidden, ""},
			{"staff organization session", scoped, "user_staff", http.StatusOK, ""},
			{"staff", staff, "user_staff", http.StatusOK, ""},
			{"support staff", support, "user_support", http.StatusOK, ""},
		} {
			t.Run(test.name+" "+route, func(t *testing.T) {
				response, body := test.browser.get(route)
				if response.StatusCode != test.status || response.Header.Get("Location") != test.location {
					t.Fatalf("GET %s = %d %q: %s", route, response.StatusCode, response.Header.Get("Location"), body)
				}
				if page && test.status == http.StatusForbidden {
					assertEntryFallback(t, test.browser, body)
					for _, action := range []string{`href="/organizations" class="entry-button entry-primary">Return to organization`, `href="/organizations" class="entry-button">Sign in again`} {
						if !strings.Contains(body, action) {
							t.Fatalf("denied page lost action %s: %s", action, body)
						}
					}
				}
				if !page && test.status != http.StatusOK && strings.Contains(body, "org_alpha") {
					t.Fatalf("refused platform response leaked registry data: %s", body)
				}
			})
		}
	}
	for _, event := range platformEvents {
		want := 2
		if event == "platform_opened" {
			want = 12
		}
		if got := platformAuditCount(t, f.service, "user_staff", event); got != want {
			t.Errorf("staff audit %s = %d, want %d", event, got, want)
		}
		if got := platformAuditCount(t, f.service, "user_alice", event); got != 0 {
			t.Errorf("customer audit %s = %d, want 0", event, got)
		}
	}
}

func TestPlatformOrganizationsAndHealth(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	f := newEntryFixture(t)
	f.provider.users["user_staff"] = "staff@example.test"
	for _, test := range []struct {
		code       string
		canSupport bool
	}{{"user_support:", true}, {"user_staff:", false}} {
		b := newBrowser(t, f.service.Handler())
		b.login("/auth/oidc/start", test.code)
		response, body := b.get("/api/cloud/platform/organizations")
		var listing struct {
			CSRF          string                 `json:"csrf"`
			CanSupport    bool                   `json:"can_support"`
			Organizations []platformOrganization `json:"organizations"`
			Unavailable   []string               `json:"unavailable"`
		}
		decodeJSON(t, body, &listing)
		if response.StatusCode != http.StatusOK || listing.CSRF == "" || listing.CanSupport != test.canSupport || len(listing.Organizations) != 2 || !slices.Contains(listing.Unavailable, "plan") || !slices.Contains(listing.Unavailable, "grants") {
			t.Fatalf("%s organizations = %d %+v", test.code, response.StatusCode, listing)
		}
		for _, organization := range listing.Organizations {
			if organization.State != "ready" || organization.CreatedAt == "" || !organization.Billing.Available || organization.Billing.Status != "free" || organization.CanSupport != test.canSupport || organization.RunnerCount == nil {
				t.Fatalf("%s organization = %+v", test.code, organization)
			}
		}
	}
	staff := newBrowser(t, f.service.Handler())
	staff.login("/auth/oidc/start", "user_staff:")
	_, body := staff.get("/api/cloud/platform/health")
	var health struct {
		Registry  struct{ OK bool }               `json:"registry"`
		Tenants   struct{ Expected, Running int } `json:"tenants"`
		Admission *platformAdmission              `json:"admission"`
	}
	decodeJSON(t, body, &health)
	if !health.Registry.OK || health.Tenants.Expected != 2 || health.Tenants.Running != 2 || health.Admission != nil {
		t.Fatalf("health = %s", body)
	}
	_, body = staff.get("/api/cloud/platform/allowlist")
	var allowlist map[string]any
	decodeJSON(t, body, &allowlist)
	if allowlist["self_service"] != false {
		t.Fatalf("allowlist without allocation = %s", body)
	}
}

func TestPlatformAllowlistAndAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("SQLite tenant provisioning integration")
	}

	t.Parallel()
	f := newProvisioningFixture(t, 3, func(a *AllocationConfig) {
		a.AllowedEmails, a.AllowedDomains = []string{"dana@example.test"}, []string{"example.org"}
	})
	f.service.config.ConfigPath = "/etc/detent/cloud.yaml"
	if err := os.MkdirAll(f.roots[0], 0o700); err != nil {
		t.Fatal(err)
	}
	f.provider.users["user_staff"] = "staff@example.test"
	staff := newBrowser(t, f.service.Handler())
	staff.login("/auth/oidc/start", "user_staff:")
	_, body := staff.get("/api/cloud/platform/allowlist")
	var allowlist struct {
		SelfService    bool     `json:"self_service"`
		Open           bool     `json:"open"`
		AllowedEmails  []string `json:"allowed_emails"`
		AllowedDomains []string `json:"allowed_domains"`
		Source         struct {
			File string   `json:"file"`
			Keys []string `json:"keys"`
		} `json:"source"`
	}
	decodeJSON(t, body, &allowlist)
	if !allowlist.SelfService || allowlist.Open || strings.Join(allowlist.AllowedEmails, ",") != "dana@example.test" || strings.Join(allowlist.AllowedDomains, ",") != "example.org" ||
		allowlist.Source.File != "/etc/detent/cloud.yaml" || strings.Join(allowlist.Source.Keys, ",") != "allocation.allowed_emails,allocation.allowed_domains" {
		t.Fatalf("allowlist = %s", body)
	}
	_, body = staff.get("/api/cloud/platform/health")
	var health struct {
		Admission platformAdmission `json:"admission"`
	}
	decodeJSON(t, body, &health)
	if health.Admission.MaxTenants != 3 || health.Admission.MaxConcurrent != 2 || !health.Admission.DiskMeasured || health.Admission.FreeDiskBytes == 0 {
		t.Fatalf("admission = %s", body)
	}
}

func TestPlatformStaffLanding(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	f := newEntryFixture(t)
	f.provider.member("user_support", "porg_alpha", "owner")
	f.provider.users["user_staff"] = "staff@example.test"
	f.provider.users["user_admin"] = "bootstrap@example.test"
	for _, test := range []struct {
		name, target, code, landing, home string
		chooserStatus                     int
		chooserLocation                   string
		platformRole                      string
		canCreate                         bool
	}{
		{"member with organizations", "/auth/oidc/start", "user_support:", "/organizations", "/organizations", http.StatusOK, "", "support", false},
		{"member without organizations", "/auth/oidc/start", "user_staff:", "/platform", "/platform", http.StatusOK, "", "viewer", false},
		{"customer default", "/auth/oidc/start", "user_alice:", "/organizations", "/organizations", http.StatusOK, "", "", false},
		{"member organization session", "/auth/oidc/start", "user_support:porg_alpha", "/organizations", "/organizations", http.StatusOK, "", "support", false},
		{"member without organizations may create", "/auth/oidc/start", "user_admin:", "/platform", "/platform", http.StatusOK, "", "admin", true},
		{"member with one organization may create", "/auth/oidc/start", "user_support:", "/organizations", "/organizations", http.StatusOK, "", "support", true},
		{"non-member may create", "/auth/oidc/start", "user_carol:", "/organizations", "/organizations", http.StatusOK, "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.service.config.Allocation = nil
			if test.canCreate {
				f.service.config.Allocation = &AllocationConfig{MaxPerIdentity: 1}
			}
			user, _, _ := strings.Cut(test.code, ":")
			session := accountSession{Subject: user, Email: f.provider.users[user], Identity: auth.HostedIdentity{Subject: user}}
			if landing := f.service.landing(t.Context(), session.Email, session.Identity); landing != test.landing {
				t.Errorf("landing = %q, want %q", landing, test.landing)
			}
			account, err := f.service.accountContextFor(t.Context(), session)
			if err != nil || account.Destination != f.service.config.PublicURL+test.landing || account.CanCreate != test.canCreate {
				t.Errorf("administration context = %+v (%v)", account, err)
			}
			b := newBrowser(t, f.service.Handler())
			if location := b.login(test.target, test.code); location != test.landing {
				t.Fatalf("post-login location = %q, want %q", location, test.landing)
			}
			if home, _ := b.get("/"); home.StatusCode != http.StatusSeeOther || home.Header.Get("Location") != test.home {
				t.Fatalf("home = %d %q", home.StatusCode, home.Header.Get("Location"))
			}
			if chooser, _ := b.get("/organizations"); chooser.StatusCode != test.chooserStatus || chooser.Header.Get("Location") != test.chooserLocation {
				t.Fatalf("chooser = %d %q", chooser.StatusCode, chooser.Header.Get("Location"))
			}
			for _, path := range []string{"/api/cloud/session", "/api/cloud/organizations"} {
				_, body := b.get(path)
				var session map[string]any
				decodeJSON(t, body, &session)
				if session["platform_role"] != test.platformRole || session["can_create"] != test.canCreate {
					t.Fatalf("%s = %s", path, body)
				}
			}
		})
	}
}

func TestPlatformMembersCreateAndJoinOrganizations(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	json := map[string]string{"Accept": "application/json"}
	t.Run("create", func(t *testing.T) {
		t.Parallel()
		f := newProvisioningFixture(t, 3, nil)
		f.provider.users["user_staff"] = "staff@example.test"
		staff := newBrowser(t, f.service.Handler())
		if landing := staff.login("/auth/oidc/start", "user_staff:"); landing != "/platform" {
			t.Fatalf("member who may create lands at %q", landing)
		}
		if response, _ := staff.get("/organizations/new"); response.StatusCode != http.StatusOK {
			t.Fatalf("staff create page = %d %q", response.StatusCode, response.Header.Get("Location"))
		}
		_, body := staff.get("/api/cloud/session")
		var session struct {
			CSRF      string `json:"csrf"`
			CanCreate bool   `json:"can_create"`
		}
		decodeJSON(t, body, &session)
		created := staff.do(http.MethodPost, "/organizations", url.Values{"name": {"Staff org"}, "creation_key": {"staff-creation-key-0001"}, "csrf": {session.CSRF}}, json)
		if !session.CanCreate || created.StatusCode != http.StatusCreated {
			t.Fatalf("staff create = %d %s", created.StatusCode, created.Body)
		}
		organizations, err := f.service.registry.List(t.Context())
		if err != nil || len(organizations) != 1 {
			t.Fatalf("staff create recorded organizations %+v (%v)", organizations, err)
		}
	})
	t.Run("join", func(t *testing.T) {
		t.Parallel()
		f := newEntryFixture(t)
		alice := newBrowser(t, f.service.Handler())
		alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
		_, page := alice.get("/organizations/org_alpha/organization")
		invitation := alice.do(http.MethodPost, "/organizations/org_alpha/organization/invite", url.Values{"email": {"support@example.test"}, "role": {"member"}, "csrf": {csrfFrom(t, page)}}, nil)
		if invitation.StatusCode != http.StatusSeeOther {
			t.Fatalf("invite platform member = %d: %s", invitation.StatusCode, invitation.Body)
		}
		staff := newBrowser(t, f.service.Handler())
		staff.login("/auth/oidc/start", "user_support:")
		invited := newBrowser(t, f.service.Handler())
		start, _ := invited.get("/invite?invitation_token=inv_support")
		provider, err := url.Parse(start.Header.Get("Location"))
		if start.StatusCode != http.StatusSeeOther || err != nil {
			t.Fatalf("invitation start = %d", start.StatusCode)
		}
		callback, _ := invited.get("/auth/oidc/callback?" + url.Values{"code": {"user_support:"}, "state": {provider.Query().Get("state")}}.Encode())
		if callback.StatusCode != http.StatusSeeOther {
			t.Fatalf("staff invitation callback = %d", callback.StatusCode)
		}
		if memberships, _ := f.provider.Memberships(t.Context(), "user_support", ""); len(memberships) != 1 {
			t.Fatalf("staff joined a customer organization: %+v", memberships)
		}
		if got := platformAuditCount(t, f.service, "user_support", "invitation_accepted"); got != 1 {
			t.Fatalf("staff invitation audited as accepted %d times", got)
		}
	})
}

func TestPlatformSupportStartRequiresCSRFAndReason(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	f := newEntryFixture(t)
	staff := newBrowser(t, f.service.Handler())
	staff.login("/auth/oidc/start", "user_support:")
	_, body := staff.get("/api/cloud/platform/organizations")
	var listing struct {
		CSRF string `json:"csrf"`
	}
	decodeJSON(t, body, &listing)
	for _, test := range []struct {
		name    string
		form    url.Values
		headers map[string]string
		status  int
		want    string
	}{
		{"missing csrf", url.Values{"organization": {"org_alpha"}, "reason": {"troubleshooting"}}, nil, http.StatusForbidden, ""},
		{"wrong csrf", url.Values{"organization": {"org_alpha"}, "reason": {"troubleshooting"}, "csrf": {"forged"}}, nil, http.StatusForbidden, ""},
		{"cross origin", url.Values{"organization": {"org_alpha"}, "reason": {"troubleshooting"}, "csrf": {listing.CSRF}}, map[string]string{"Origin": "https://attacker.example.test"}, http.StatusForbidden, ""},
		{"missing reason", url.Values{"organization": {"org_alpha"}, "csrf": {listing.CSRF}}, nil, http.StatusUnprocessableEntity, ""},
		{"invalid reason", url.Values{"organization": {"org_alpha"}, "reason": {"curiosity"}, "csrf": {listing.CSRF}}, nil, http.StatusUnprocessableEntity, ""},
		{"valid", url.Values{"organization": {"org_alpha"}, "reason": {"troubleshooting"}, "csrf": {listing.CSRF}}, nil, http.StatusOK, "using reason troubleshooting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := staff.do(http.MethodPost, "/support/start", test.form, test.headers)
			if response.StatusCode != test.status || !strings.Contains(response.Body, test.want) {
				t.Fatalf("support start = %d: %s", response.StatusCode, response.Body)
			}
		})
	}
	if got := platformAuditCount(t, f.service, "user_support", "support_requested:troubleshooting"); got != 1 {
		t.Fatalf("support request audit = %d, want 1", got)
	}
	callback, _ := staff.get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|troubleshooting"))
	if callback.StatusCode != http.StatusSeeOther {
		t.Fatalf("support callback = %d %s", callback.StatusCode, callback.Body)
	}
	for _, event := range []string{"support_requested:troubleshooting", "support_started:troubleshooting"} {
		if got := platformAuditCount(t, f.service, "user_support", event); got != 1 {
			t.Errorf("audit %s = %d, want 1", event, got)
		}
	}
}

type fanOutTransport struct {
	delay    time.Duration
	mu       sync.Mutex
	active   int
	peak     int
	requests int
}

func (f *fanOutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.active++
	f.requests++
	f.peak = max(f.peak, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	select {
	case <-time.After(f.delay):
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	recorder := httptest.NewRecorder()
	if request.URL.Path == "/internal/v1/health" {
		recorder.WriteHeader(http.StatusNoContent)
	} else {
		recorder.Header().Set("Content-Type", "application/json")
		recorder.WriteHeader(http.StatusOK)
		_, _ = recorder.WriteString(`{"enabled":false,"status":"free"}`)
	}
	return recorder.Result(), nil
}

func TestPlatformTenantFanOutIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()
	const tenants = 40
	for _, test := range []struct {
		name        string
		delay       time.Duration
		deadline    time.Duration
		allReached  bool
		maxDuration time.Duration
	}{
		{"all tenants answer within the deadline", 20 * time.Millisecond, 10 * time.Second, true, 10 * time.Second},
		{"slow tenants past the deadline are unavailable", 400 * time.Millisecond, 500 * time.Millisecond, false, 3 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &fanOutTransport{delay: test.delay}
			seed := make([]byte, ed25519.SeedSize)
			provider := newFakeProvider()
			provider.users["user_support"] = "support@example.test"
			service, err := Open(t.Context(), Config{Platform: PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"}, PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(seed), Provider: provider,
				StaffEmails: []string{"support@example.test"}, StateDir: t.TempDir(), Logger: slog.New(slog.DiscardHandler), clientFS: fstest.MapFS{},
				transport: func(Organization) (http.RoundTripper, error) { return transport, nil }, platformDeadline: test.deadline})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			for index := range tenants {
				id := fmt.Sprintf("org_t%02d", index)
				if _, err := service.Registry().Register(t.Context(), Organization{ID: id, ProviderID: "p" + id, Name: id, Endpoint: testSocketEndpoint(id + ".sock"), Generation: 1}); err != nil {
					t.Fatal(err)
				}
			}
			staff := newBrowser(t, service.Handler())
			staff.login("/auth/oidc/start", "user_support:")
			for _, route := range []string{"/api/cloud/platform/organizations", "/api/cloud/platform/health"} {
				transport.mu.Lock()
				transport.peak, transport.requests = 0, 0
				transport.mu.Unlock()
				started := time.Now()
				response, body := staff.get(route)
				elapsed := time.Since(started)
				if response.StatusCode != http.StatusOK || elapsed > test.maxDuration {
					t.Fatalf("%s = %d after %s", route, response.StatusCode, elapsed)
				}
				transport.mu.Lock()
				peak, requests := transport.peak, transport.requests
				transport.mu.Unlock()
				calls := 1
				if route == "/api/cloud/platform/organizations" {
					calls = 4
				}
				if peak > platformTenantWorkers || test.allReached && (peak != platformTenantWorkers || requests != tenants*calls) || !test.allReached && requests >= tenants*calls {
					t.Fatalf("%s peak concurrency = %d, requests = %d", route, peak, requests)
				}
				reached := 0
				if route == "/api/cloud/platform/health" {
					var health struct {
						Tenants struct{ Expected, Running int } `json:"tenants"`
					}
					decodeJSON(t, body, &health)
					if health.Tenants.Expected != tenants {
						t.Fatalf("health = %s", body)
					}
					reached = health.Tenants.Running
				} else {
					var listing struct {
						Organizations []platformOrganization `json:"organizations"`
					}
					decodeJSON(t, body, &listing)
					for _, organization := range listing.Organizations {
						if organization.Billing.Available {
							reached++
						}
					}
				}
				if (reached == tenants) != test.allReached {
					t.Fatalf("%s reached %d of %d tenants", route, reached, tenants)
				}
			}
		})
	}
}

func TestStaffSessionsReverifyEveryRead(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, email string
		staff       bool
		wantExtra   int
	}{
		{name: "staff re-verify every read", email: "support@example.test", staff: true, wantExtra: 3},
		{name: "customers reuse the recent check", email: "dana@example.test", wantExtra: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			seed := make([]byte, ed25519.SeedSize)
			provider := newFakeProvider()
			provider.users["user_reader"] = test.email
			staff := []string{"other-staff@example.test"}
			if test.staff {
				staff = append(staff, test.email)
			}
			service, err := Open(t.Context(), Config{Platform: PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"}, PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(seed), Provider: provider,
				StaffEmails: staff, StateDir: t.TempDir(), Logger: slog.New(slog.DiscardHandler), clientFS: fstest.MapFS{},
				transport: func(Organization) (http.RoundTripper, error) { return &fanOutTransport{}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			reader := newBrowser(t, service.Handler())
			reader.login("/auth/oidc/start", "user_reader:")
			reader.get("/api/cloud/session")
			provider.mu.Lock()
			before := provider.verifications
			provider.mu.Unlock()
			for range 3 {
				reader.get("/api/cloud/session")
			}
			provider.mu.Lock()
			extra := provider.verifications - before
			provider.mu.Unlock()
			if extra != test.wantExtra {
				t.Fatalf("provider verifications for 3 reads = %d, want %d", extra, test.wantExtra)
			}
		})
	}
}

func TestPlatformAuditTimeline(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	f.provider.users["user_staff"] = "staff@example.test"
	staff := newBrowser(t, f.service.Handler())
	staff.login("/auth/oidc/start", "user_staff:")
	ctx := t.Context()
	for _, statement := range []struct {
		auth  bool
		query string
	}{
		{false, "UPDATE organizations SET state='deleted' WHERE id='org_beta'"},
		{true, `INSERT INTO audit(subject,organization_id,event,recorded_at) VALUES ('user_staff','','{"kind":"operator_administration","retry_identity":"private-replay","output":{"csrf":"private-token"}}','2026-10-01T12:00:01Z')`},
		{true, "INSERT INTO audit(subject,organization_id,event,recorded_at) VALUES ('user_staff','org_alpha','support_requested:troubleshooting','2026-10-01T12:00:00.1Z'),('user_staff','org_alpha','support_started:troubleshooting','2026-10-01T12:00:00Z'),('missing_subject','org_beta','organization_deleted','2026-09-30T23:59:59.999999999Z'),('user_staff','','platform_opened','2026-10-01T12:00:00.000000001Z')"},
		{false, "INSERT INTO entitlement_changes(organization_id,idempotency_key,action,grant_id,plan_id,plan_version,expires_at,reason,staff_email,staff_subject,recorded_at) VALUES ('org_alpha','grant_key','grant','comp_a','growth',1,'2026-12-31T23:59:59Z','pilot','staff@example.test','user_staff','2026-10-01T12:00:00.100000000Z'),('org_beta','revoke_key','revoke','comp_b','team',2,'','ended','billing@example.test','user_billing','2026-09-29T12:00:00Z')"},
		{false, "INSERT INTO organization_events(organization_id,event,generation,recorded_at) VALUES ('org_alpha','ready',1,'2026-10-01T12:00:00.1Z')"},
		{false, "INSERT INTO platform_member_changes(email,action,previous_role,role,actor_email,actor_subject,reason,recorded_at) VALUES ('new@example.test','added','','support','staff@example.test','user_staff','new hire','2026-10-01T12:00:00.1Z'),('new@example.test','role_changed','support','viewer','staff@example.test','user_staff','read only','2026-09-28T12:00:00Z'),('old@example.test','removed','viewer','','staff@example.test','user_staff','left','2026-09-27T12:00:00Z'),('seed@example.test','seeded','','admin','bootstrap','','bootstrap','2026-09-26T12:00:00Z')"},
	} {
		db := f.service.registry.store.db
		if statement.auth {
			db = f.service.auth.store.db
		}
		if _, err := db.ExecContext(ctx, statement.query); err != nil {
			t.Fatal(err)
		}
	}
	all := []string{"member_added", "organization_ready", "plan_granted", "support_requested", "platform_opened", "support_started", "organization_deleted", "plan_revoked", "member_role_changed", "member_removed", "member_seeded"}
	read := func(query string) platformAuditResult {
		t.Helper()
		response, body := staff.get("/api/cloud/platform/audit?" + query)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("audit = %d: %s", response.StatusCode, body)
		}
		var result platformAuditResult
		decodeJSON(t, body, &result)
		return result
	}
	names := func(rows []platformAuditRow) []string {
		result := []string{}
		for _, row := range rows {
			result = append(result, row.Event)
		}
		return result
	}
	for _, test := range []struct {
		name, query string
		want        []string
	}{
		{"merged", "", all},
		{"tenant", "tenant=org_alpha", []string{"organization_ready", "plan_granted", "support_requested", "support_started"}},
		{"actor email substring", "actor=STAFF%40", []string{"member_added", "plan_granted", "support_requested", "platform_opened", "support_started", "member_role_changed", "member_removed"}},
		{"subject fallback", "actor=missing_subject", []string{"organization_deleted"}},
		{"system", "actor=system", []string{"organization_ready"}},
		{"event", "event=support_requested", []string{"support_requested"}},
		{"from", "from=2026-10-01", all[:6]},
		{"to", "to=2026-09-27", all[9:]},
		{"combined", "tenant=org_alpha&actor=staff&event=plan_granted&from=2026-10-01&to=2026-10-01", []string{"plan_granted"}},
		{"nanosecond boundary", "from=2026-10-01T12%3A00%3A00.000000001Z&to=2026-10-01T12%3A00%3A00.000000001Z", []string{"platform_opened"}},
		{"empty", "tenant=unknown", []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := "to=2026-10-01"
			if test.query != "" {
				query = test.query + "&" + query
			}
			if strings.Contains(test.query, "to=") {
				query = test.query
			}
			result := read(query)
			if got := names(result.Rows); !slices.Equal(got, test.want) {
				t.Fatalf("events = %v, want %v", got, test.want)
			}
			if !slices.Contains(result.Events, "support_requested") || slices.Contains(result.Events, "support_requested:troubleshooting") {
				t.Fatalf("event choices = %v", result.Events)
			}
		})
	}
	first := read("to=2026-10-01")
	for _, test := range []struct {
		query string
		match func(platformAuditRow) bool
	}{
		{"tenant=org_beta", func(row platformAuditRow) bool { return row.OrganizationID == "org_beta" }},
		{"actor=staff%40", func(row platformAuditRow) bool { return strings.Contains(row.Actor, "staff@") }},
		{"event=member_seeded", func(row platformAuditRow) bool { return row.Event == "member_seeded" }},
		{"from=2026-10-01T12%3A00%3A00.1Z", func(row platformAuditRow) bool {
			at, err := parseTime(row.At)
			return err == nil && !at.Before(time.Date(2026, 10, 1, 12, 0, 0, 100000000, time.UTC))
		}},
		{"to=2026-09-30", func(row platformAuditRow) bool { return row.At < "2026-10-01" }},
	} {
		t.Run("individual "+test.query, func(t *testing.T) {
			page := read(test.query)
			if len(page.Rows) == 0 {
				t.Fatal("filter excluded every matching event")
			}
			for _, row := range page.Rows {
				if !test.match(row) {
					t.Fatalf("filter admitted %+v", row)
				}
			}
		})
	}
	for _, row := range first.Rows {
		switch row.Event {
		case "support_requested", "support_started":
			if row.Actor != "staff@example.test" || row.Detail != "troubleshooting" || row.OrganizationName != "Alpha" {
				t.Fatalf("support row = %+v", row)
			}
		case "organization_ready":
			if row.Actor != "system" || row.Detail != "generation 1" {
				t.Fatalf("system row = %+v", row)
			}
		case "plan_granted":
			if row.Detail != "growth v1 until 2026-12-31T23:59:59Z · pilot" {
				t.Fatalf("grant row = %+v", row)
			}
		case "member_role_changed":
			if row.Detail != "new@example.test from support to viewer · read only" {
				t.Fatalf("member row = %+v", row)
			}
		case "organization_deleted":
			if row.Actor != "missing_subject" || !row.OrganizationDeleted || row.OrganizationName != "" {
				t.Fatalf("deleted row = %+v", row)
			}
		case "platform_opened":
			if row.Detail != "" {
				t.Fatalf("unexpected detail = %q", row.Detail)
			}
		}
	}
	for _, limit := range []int{1, 2, 3, 5, 200} {
		t.Run(fmt.Sprintf("cursor limit %d", limit), func(t *testing.T) {
			var got []string
			cursor := ""
			seen := map[string]bool{}
			for range len(all) + 1 {
				page := read(fmt.Sprintf("to=2026-10-01&limit=%d&cursor=%s", limit, url.QueryEscape(cursor)))
				got = append(got, names(page.Rows)...)
				for _, row := range page.Rows {
					key := row.Source + ":" + row.ID
					if seen[key] {
						t.Fatalf("duplicate %s", key)
					}
					seen[key] = true
				}
				if page.NextCursor == "" {
					break
				}
				cursor = page.NextCursor
			}
			if !slices.Equal(got, all) {
				t.Fatalf("pagination = %v, want %v", got, all)
			}
		})
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=bad", "from=yesterday", "to=bad", "from=2026-10-02&to=2026-10-01", "cursor=bad"} {
		t.Run(query, func(t *testing.T) {
			response, _ := staff.get("/api/cloud/platform/audit?" + query)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
	for _, role := range []string{"admin", "billing", "support", "viewer"} {
		t.Run(role, func(t *testing.T) {
			if _, err := f.service.registry.store.db.ExecContext(ctx, "UPDATE platform_members SET role=? WHERE email='staff@example.test'", role); err != nil {
				t.Fatal(err)
			}
			page := read("event=platform_audit_viewed")
			if len(page.Rows) == 0 || page.Rows[0].Actor != "staff@example.test" {
				t.Fatalf("view was not audited: %+v", page.Rows)
			}
		})
	}
}
