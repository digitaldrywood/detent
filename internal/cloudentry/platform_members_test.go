//go:build !windows

package cloudentry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
)

func TestPlatformMemberSeeding(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		config   Config
		existing map[string]string
		want     map[string]string
		fail     bool
	}{
		{name: "legacy mapping", config: Config{Platform: PlatformConfig{BootstrapAdminEmail: " BOOT@Example.test "}, StaffEmails: []string{"viewer@example.test", "support@example.test"}, SupportActors: []string{"support@example.test", "both@example.test"}, EntitlementAdministrators: []string{"billing@example.test", "BOTH@example.test", "billing@example.test"}}, want: map[string]string{"boot@example.test": "admin", "viewer@example.test": "viewer", "support@example.test": "support", "billing@example.test": "billing", "both@example.test": "admin"}},
		{name: "combined legacy admin", config: Config{SupportActors: []string{"both@example.test"}, EntitlementAdministrators: []string{"both@example.test"}}, want: map[string]string{"both@example.test": "admin"}},
		{name: "existing admin ignores lists", config: Config{StaffEmails: []string{"new@example.test"}, SupportActors: []string{"new@example.test"}}, existing: map[string]string{"existing@example.test": "admin"}, want: map[string]string{"existing@example.test": "admin"}},
		{name: "bootstrap restored", config: Config{Platform: PlatformConfig{BootstrapAdminEmail: "boot@example.test"}}, existing: map[string]string{"boot@example.test": "viewer"}, want: map[string]string{"boot@example.test": "admin"}},
		{name: "bootstrap added to populated table", config: Config{Platform: PlatformConfig{BootstrapAdminEmail: "boot@example.test"}}, existing: map[string]string{"viewer@example.test": "viewer"}, want: map[string]string{"boot@example.test": "admin", "viewer@example.test": "viewer"}},
		{name: "no admin", config: Config{StaffEmails: []string{"viewer@example.test"}}, fail: true},
		{name: "empty", fail: true},
		{name: "existing without admin", existing: map[string]string{"viewer@example.test": "viewer"}, fail: true},
		{name: "invalid bootstrap", config: Config{Platform: PlatformConfig{BootstrapAdminEmail: "invalid"}}, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			registry, err := OpenRegistry(t.Context(), filepath.Join(t.TempDir(), "registry.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := registry.Close(); err != nil {
					t.Error(err)
				}
			})
			for email, role := range test.existing {
				if _, err := registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES(?,?, 'existing','time','time')", email, role); err != nil {
					t.Fatal(err)
				}
			}
			test.config.now = time.Now
			test.config.Logger = slog.New(slog.DiscardHandler)
			err = registry.seedPlatformMembers(t.Context(), test.config)
			if (err != nil) != test.fail {
				t.Fatalf("seed = %v", err)
			}
			if test.fail {
				return
			}
			s := &Service{registry: registry}
			listing, err := s.readPlatformMembers(t.Context(), accountSession{})
			if err != nil || len(listing.Members) != len(test.want) {
				t.Fatalf("members = %+v, %v", listing, err)
			}
			for _, member := range listing.Members {
				if member.Role != test.want[member.Email] || s.platformRole(t.Context(), " "+strings.ToUpper(member.Email)+" ") != member.Role {
					t.Fatalf("member = %+v", member)
				}
			}
			var seeded int
			if err := registry.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM platform_member_changes WHERE action='seeded' AND actor_email='bootstrap' AND reason!=''").Scan(&seeded); err != nil || int64(seeded) != listing.Revision {
				t.Fatalf("seeded = %d, revision = %d, %v", seeded, listing.Revision, err)
			}
			if err := registry.seedPlatformMembers(t.Context(), test.config); err != nil {
				t.Fatal(err)
			}
			again, err := s.readPlatformMembers(t.Context(), accountSession{})
			if err != nil || again.Revision != listing.Revision {
				t.Fatalf("startup replay = %+v, %v", again, err)
			}
			if listing.Revision > 0 {
				for _, statement := range []string{"DELETE FROM platform_member_changes", "UPDATE platform_member_changes SET role='viewer'"} {
					if _, err := registry.store.db.ExecContext(t.Context(), statement); err == nil {
						t.Fatalf("audit mutation allowed: %s", statement)
					}
				}
			}
		})
	}
}

func memberRequest(t *testing.T, b *browser, method, email string, body map[string]any, csrf bool) (int, []byte) {
	t.Helper()
	_, sessionBody := b.get("/api/cloud/session")
	var session struct {
		CSRF string `json:"csrf"`
	}
	decodeJSON(t, sessionBody, &session)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/cloud/platform/members"
	if email != "" {
		path += "/" + email
	}
	request := httptest.NewRequestWithContext(t.Context(), method, testPublicURL+path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", testPublicURL)
	if csrf {
		request.Header.Set("X-CSRF-Token", session.CSRF)
	}
	base, err := url.Parse(testPublicURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range b.jar.Cookies(base) {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	b.entry.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

func TestPlatformMembersAPI(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	for email, role := range map[string]string{"admin@example.test": "admin", "supporter@example.test": "support", "billing@example.test": "billing", "viewer@example.test": "viewer"} {
		if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES(?,?, 'fixture','time','time')", email, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"admin", "support", "billing", "viewer", "customer", "impersonation"} {
		t.Run(role, func(t *testing.T) {
			email := map[string]string{"admin": "admin@example.test", "support": "supporter@example.test", "billing": "billing@example.test", "viewer": "viewer@example.test", "customer": "customer@example.test", "impersonation": "admin@example.test"}[role]
			f.provider.users["user_"+role] = email
			b := newBrowser(t, f.service.Handler())
			b.login("/auth/oidc/start", "user_"+role+":")
			if role == "impersonation" {
				session, err := f.service.auth.session(t.Context(), apikey.HashToken(sessionCookie(t, b, f.service)))
				if err != nil {
					t.Fatal(err)
				}
				session.Identity.SupportActor = "admin@example.test"
				identityRaw, err := json.Marshal(session.Identity)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE sessions SET identity_json=? WHERE token_hash=?", string(identityRaw), session.Hash); err != nil {
					t.Fatal(err)
				}
			}
			response, raw := b.get("/api/cloud/platform/members")
			want := http.StatusOK
			if role == "customer" || role == "impersonation" {
				want = http.StatusForbidden
			}
			if response.StatusCode != want {
				t.Fatalf("list = %d %s", response.StatusCode, raw)
			}
			if want == http.StatusOK {
				var listing platformMembersResult
				decodeJSON(t, raw, &listing)
				if listing.Self.Email != email || listing.Self.Role != role || len(listing.Members) == 0 {
					t.Fatalf("member listing = %+v", listing)
				}
			}
			_, sessionRaw := b.get("/api/cloud/session")
			var account struct {
				PlatformRole string `json:"platform_role"`
			}
			decodeJSON(t, sessionRaw, &account)
			wantRole := role
			if role == "customer" || role == "impersonation" {
				wantRole = ""
			}
			if account.PlatformRole != wantRole {
				t.Fatalf("session role = %q, want %q", account.PlatformRole, wantRole)
			}
			_, listingRaw := b.get("/api/cloud/organizations")
			var listing struct {
				CanGrant bool `json:"can_grant"`
			}
			decodeJSON(t, listingRaw, &listing)
			if listing.CanGrant != (role == "admin" || role == "billing") {
				t.Fatalf("can_grant = %v for %s", listing.CanGrant, role)
			}
			if role == "admin" {
				return
			}
			for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
				target := "viewer@example.test"
				if method == http.MethodPost {
					target = ""
				}
				status, raw := memberRequest(t, b, method, target, map[string]any{"email": "new@example.test", "role": "viewer", "reason": "role test", "idempotency_key": "role_test", "expected_revision": 3}, true)
				if status != http.StatusForbidden {
					t.Fatalf("%s = %d %s", method, status, raw)
				}
			}
		})
	}
	admin := newBrowser(t, f.service.Handler())
	admin.login("/auth/oidc/start", "user_admin:")
	read := func() platformMembersResult {
		_, raw := admin.get("/api/cloud/platform/members")
		var result platformMembersResult
		decodeJSON(t, raw, &result)
		return result
	}
	for i, test := range []struct {
		name, method, email, role, reason string
		status                            int
		stale, noCSRF                     bool
	}{
		{name: "add", method: http.MethodPost, email: " New@Example.test ", role: "viewer", reason: "Add observer", status: 200},
		{name: "duplicate", method: http.MethodPost, email: "new@example.test", role: "viewer", reason: "Duplicate", status: 409},
		{name: "self change", method: http.MethodPatch, email: "admin@example.test", role: "viewer", reason: "Demote self", status: 403},
		{name: "bootstrap remove", method: http.MethodDelete, email: "bootstrap@example.test", reason: "Remove bootstrap", status: 403},
		{name: "unknown role", method: http.MethodPatch, email: "new@example.test", role: "owner", reason: "Unknown role", status: 422},
		{name: "reason required", method: http.MethodPatch, email: "new@example.test", role: "support", status: 422},
		{name: "oversized reason", method: http.MethodPatch, email: "new@example.test", role: "support", reason: strings.Repeat("x", 501), status: 422},
		{name: "stale", method: http.MethodPatch, email: "new@example.test", role: "billing", reason: "Stale update", stale: true, status: 409},
		{name: "csrf", method: http.MethodPatch, email: "new@example.test", role: "billing", reason: "CSRF", noCSRF: true, status: 403},
		{name: "change", method: http.MethodPatch, email: "new@example.test", role: "support", reason: "Enable support", status: 200},
		{name: "remove", method: http.MethodDelete, email: "new@example.test", reason: "Remove observer", status: 200},
		{name: "missing", method: http.MethodDelete, email: "missing@example.test", reason: "Remove missing", status: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			revision := read().Revision
			if test.stale {
				revision--
			}
			body := map[string]any{"reason": test.reason, "idempotency_key": fmt.Sprintf("member_%d", i), "expected_revision": revision}
			target := test.email
			if test.method == http.MethodPost {
				body["email"] = target
				target = ""
			}
			if test.method != http.MethodDelete {
				body["role"] = test.role
			}
			status, raw := memberRequest(t, admin, test.method, target, body, !test.noCSRF)
			if status != test.status {
				t.Fatalf("response = %d %s", status, raw)
			}
			if status == 200 {
				replayStatus, replay := memberRequest(t, admin, test.method, target, body, true)
				if replayStatus != 200 || !bytes.Equal(raw, replay) {
					t.Fatalf("replay = %d %s, first %s", replayStatus, replay, raw)
				}
				if got := read().Revision; got != revision+1 {
					t.Fatalf("revision = %d", got)
				}
				body["reason"] = "Different input"
				if status, raw := memberRequest(t, admin, test.method, target, body, true); status != 409 {
					t.Fatalf("changed replay = %d %s", status, raw)
				}
			}
		})
	}
	var changes int
	if err := f.service.registry.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM platform_member_changes WHERE actor_subject='user_admin' AND actor_email='admin@example.test' AND reason!='' AND action IN ('added','role_changed','removed')").Scan(&changes); err != nil || changes != 3 {
		t.Fatalf("changes = %d %v", changes, err)
	}
	for _, event := range []string{platformMemberList, platformMemberAdd, platformMemberChange, platformMemberRemove} {
		if platformAuditCount(t, f.service, "user_admin", event) == 0 {
			t.Fatalf("missing audit %s", event)
		}
	}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "DELETE FROM platform_members WHERE email='bootstrap@example.test'"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		body := map[string]any{"reason": "Last admin", "idempotency_key": "last_" + method, "expected_revision": read().Revision}
		if method == http.MethodPatch {
			body["role"] = "viewer"
		}
		if status, raw := memberRequest(t, admin, method, "admin@example.test", body, true); status != 403 {
			t.Fatalf("last admin = %d %s", status, raw)
		}
	}
}

func sessionCookie(t *testing.T, b *browser, s *Service) string {
	t.Helper()
	base, err := url.Parse(testPublicURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range b.jar.Cookies(base) {
		if cookie.Name == s.cookieName("session") {
			return cookie.Value
		}
	}
	t.Fatal("missing session cookie")
	return ""
}
