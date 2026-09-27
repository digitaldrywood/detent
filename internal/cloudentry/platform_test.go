//go:build !windows

package cloudentry

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
)

var platformRoutes = []string{"/platform", "/api/cloud/platform/organizations", "/api/cloud/platform/allowlist", "/api/cloud/platform/health"}

var platformEvents = map[string]string{
	"/platform":                         "platform_opened",
	"/api/cloud/platform/organizations": "platform_organizations_viewed",
	"/api/cloud/platform/allowlist":     "platform_allowlist_viewed",
	"/api/cloud/platform/health":        "platform_health_viewed",
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
	impersonation := supportIdentityBrowser(t, f)

	for _, route := range platformRoutes {
		page := route == "/platform"
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
			{"staff", staff, "user_staff", http.StatusOK, ""},
			{"support staff", support, "user_support", http.StatusOK, ""},
		} {
			t.Run(test.name+" "+route, func(t *testing.T) {
				response, body := test.browser.get(route)
				if response.StatusCode != test.status || response.Header.Get("Location") != test.location {
					t.Fatalf("GET %s = %d %q: %s", route, response.StatusCode, response.Header.Get("Location"), body)
				}
				if !page && test.status != http.StatusOK && strings.Contains(body, "org_alpha") {
					t.Fatalf("refused platform response leaked registry data: %s", body)
				}
			})
		}
	}
	for _, event := range platformEvents {
		if got := platformAuditCount(t, f.service, "user_staff", event); got != 1 {
			t.Errorf("staff audit %s = %d, want 1", event, got)
		}
		if got := platformAuditCount(t, f.service, "user_alice", event); got != 0 {
			t.Errorf("customer audit %s = %d, want 0", event, got)
		}
	}
}

func TestPlatformOrganizationsAndHealth(t *testing.T) {
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
		if response.StatusCode != http.StatusOK || listing.CSRF == "" || listing.CanSupport != test.canSupport || len(listing.Organizations) != 2 || len(listing.Unavailable) != len(platformUnavailable) {
			t.Fatalf("%s organizations = %d %+v", test.code, response.StatusCode, listing)
		}
		for _, organization := range listing.Organizations {
			if organization.State != "ready" || organization.CreatedAt == "" || !organization.Billing.Available || organization.Billing.Status != "free" || organization.CanSupport != test.canSupport || organization.MemberCount != nil || organization.RunnerCount != nil {
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
	t.Parallel()
	f := newEntryFixture(t)
	for _, test := range []struct {
		name, target, code, landing, home string
		chooserStatus                     int
		chooserLocation                   string
		staff                             bool
	}{
		{"staff default", "/auth/oidc/start", "user_support:", "/platform", "/platform", http.StatusSeeOther, "/platform", true},
		{"staff chooser return", "/organizations", "user_support:", "/organizations", "/platform", http.StatusSeeOther, "/platform", true},
		{"customer default", "/auth/oidc/start", "user_alice:", "/organizations", "/organizations", http.StatusOK, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
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
				if session["staff"] != test.staff || session["can_create"] != false {
					t.Fatalf("%s = %s", path, body)
				}
			}
		})
	}
}

func TestStaffCannotCreateOrJoinOrganizations(t *testing.T) {
	t.Parallel()
	json := map[string]string{"Accept": "application/json"}
	t.Run("create", func(t *testing.T) {
		t.Parallel()
		f := newProvisioningFixture(t, 3, nil)
		f.provider.users["user_staff"] = "staff@example.test"
		staff := newBrowser(t, f.service.Handler())
		staff.login("/auth/oidc/start", "user_staff:")
		if response, _ := staff.get("/organizations/new"); response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/platform" {
			t.Fatalf("staff create page = %d %q", response.StatusCode, response.Header.Get("Location"))
		}
		_, body := staff.get("/api/cloud/session")
		var session struct {
			CSRF      string `json:"csrf"`
			CanCreate bool   `json:"can_create"`
		}
		decodeJSON(t, body, &session)
		created := staff.do(http.MethodPost, "/organizations", url.Values{"name": {"Staff org"}, "creation_key": {"staff-creation-key-0001"}, "csrf": {session.CSRF}}, json)
		if session.CanCreate || created.StatusCode != http.StatusForbidden || !strings.Contains(created.Body, "staff_session") {
			t.Fatalf("staff create = %d %s", created.StatusCode, created.Body)
		}
		organizations, err := f.service.registry.List(t.Context())
		if err != nil || len(organizations) != 0 {
			t.Fatalf("staff create recorded organizations %+v (%v)", organizations, err)
		}
	})
	t.Run("join", func(t *testing.T) {
		t.Parallel()
		f := newEntryFixture(t)
		if _, err := f.provider.Invite(t.Context(), "porg_alpha", "support@example.test", "", ""); err != nil {
			t.Fatal(err)
		}
		staff := newBrowser(t, f.service.Handler())
		staff.login("/auth/oidc/start", "user_support:")
		if response, _ := staff.get("/invitations/join"); response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/platform" {
			t.Fatalf("staff join page = %d %q", response.StatusCode, response.Header.Get("Location"))
		}
		_, body := staff.get("/api/cloud/session")
		var session struct {
			CSRF string `json:"csrf"`
		}
		decodeJSON(t, body, &session)
		joined := staff.do(http.MethodPost, "/invitations/join", url.Values{"token": {"inv_support"}, "csrf": {session.CSRF}}, json)
		if joined.StatusCode != http.StatusForbidden || !strings.Contains(joined.Body, "staff_session") {
			t.Fatalf("staff join = %d %s", joined.StatusCode, joined.Body)
		}
		invited := newBrowser(t, f.service.Handler())
		start, _ := invited.get("/invite?invitation_token=inv_support")
		provider, err := url.Parse(start.Header.Get("Location"))
		if start.StatusCode != http.StatusSeeOther || err != nil {
			t.Fatalf("invitation start = %d", start.StatusCode)
		}
		callback, _ := invited.get("/auth/oidc/callback?" + url.Values{"code": {"user_support:"}, "state": {provider.Query().Get("state")}}.Encode())
		if callback.StatusCode != http.StatusForbidden {
			t.Fatalf("staff invitation callback = %d", callback.StatusCode)
		}
		if memberships, _ := f.provider.Memberships(t.Context(), "user_support", ""); len(memberships) != 0 {
			t.Fatalf("staff joined a customer organization: %+v", memberships)
		}
		if got := platformAuditCount(t, f.service, "user_support", "invitation_accepted"); got != 0 {
			t.Fatalf("staff invitation audited as accepted %d times", got)
		}
	})
}

func TestPlatformSupportStartRequiresCSRFAndReason(t *testing.T) {
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
}
