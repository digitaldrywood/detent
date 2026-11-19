package hubserver

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedSharedFixture struct {
	hostedSecurityFixture
	key ed25519.PrivateKey
}

func hostedSharedKey(seed byte) ed25519.PrivateKey {
	value := make([]byte, ed25519.SeedSize)
	for i := range value {
		value[i] = seed
	}
	return ed25519.NewKeyFromSeed(value)
}

func hostedSharedTestConfig(provider auth.HostedProvider, key ed25519.PrivateKey, generation int64) Config {
	legacyPlans := hostedplans.PilotPlans()
	return Config{Hosted: &hostedplans.HostedConfig{
		Plans: &legacyPlans, OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", BootstrapSubject: "user_owner",
		PublicURL: "https://hub.example.test", StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}, Provider: provider,
		SharedEntry: &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: generation},
	}}
}

func newHostedSharedFixture(t *testing.T, configure ...func(*Config)) hostedSharedFixture {
	t.Helper()
	key := hostedSharedKey(7)
	provider := newHostedSecurityProvider()
	cfg := hostedSharedTestConfig(provider, key, 1)
	for _, apply := range configure {
		apply(&cfg)
	}
	service := openHostedPostgresService(t, cfg)
	project := tracker.ProjectID("prj_security")
	seedHostedPostgresProject(t, service, project)
	return hostedSharedFixture{hostedSecurityFixture: hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/org_security/projects/" + string(project)}, key: key}
}

// accessRole is the role the entry would read from the user's access token.
func (f hostedSharedFixture) accessRole(subject string) string {
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	for _, member := range f.provider.members {
		if member.UserID == subject && member.Status == "active" {
			return member.Role.Slug
		}
	}
	return "member"
}

func (f hostedSharedFixture) claims(user *hostedSecurityUser, kind, method, target string, body []byte) cloudassert.Claims {
	now := time.Now()
	id, _ := cloudassert.NewID()
	claims := cloudassert.Claims{Issuer: "entry", Audience: "org_security", Generation: 1, Kind: kind, Method: method, Path: target, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id}
	if user != nil && kind == cloudassert.KindBrowser {
		hosted := user.identity.Hosted
		claims.Subject, claims.Email, claims.ProviderOrganization, claims.ProviderSession = hosted.Subject, user.identity.Email, hosted.OrganizationID, hosted.SessionID
		claims.SessionCreatedAt, claims.SessionExpiresAt = hosted.CreatedAt, hosted.ExpiresAt
		claims.SupportActor, claims.SupportReason = hosted.SupportActor, hosted.SupportReason
		claims.Role, claims.AccessExpiresAt = f.accessRole(hosted.Subject), now.Add(5*time.Minute)
		claims.Binding = cloudassert.AuthorizationBinding("shared-"+hosted.Subject, "org_security", hosted.SessionID)
		claims.CSRF = cloudassert.CSRFToken("shared-"+hosted.Subject, "org_security")
	}
	if user != nil && kind == cloudassert.KindService {
		claims.Subject, claims.Email, claims.ProviderSession = user.identity.Subject, user.identity.Email, user.identity.Hosted.SessionID
	}
	return claims
}

type hostedSharedRequest struct {
	headers  map[string]string
	user     *hostedSecurityUser
	kind     string
	method   string
	target   string
	body     string
	form     bool
	bearer   string
	csrf     string
	mutate   func(*cloudassert.Claims)
	unsigned bool
	signer   ed25519.PrivateKey
	cookie   string
}

func (f hostedSharedFixture) serve(t *testing.T, r hostedSharedRequest) *httptest.ResponseRecorder {
	t.Helper()
	if r.method == "" {
		r.method = http.MethodGet
	}
	if r.kind == "" {
		r.kind = cloudassert.KindBrowser
	}
	request := httptest.NewRequest(r.method, r.target, strings.NewReader(r.body))
	for name, value := range r.headers {
		request.Header.Set(name, value)
	}
	if r.form {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else if r.body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if r.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.csrf != "" {
		request.Header.Set("X-CSRF-Token", r.csrf)
	}
	if r.cookie != "" {
		request.AddCookie(&http.Cookie{Name: hostedCookie, Value: r.cookie})
	}
	if !r.unsigned {
		claims := f.claims(r.user, r.kind, r.method, request.URL.RequestURI(), []byte(r.body))
		if r.mutate != nil {
			r.mutate(&claims)
		}
		signer := f.key
		if r.signer != nil {
			signer = r.signer
		}
		value, err := cloudassert.Sign(signer, claims)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set(cloudassert.Header, value)
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func (f hostedSharedFixture) sessionCredential(t *testing.T, user hostedSecurityUser) hubapi.APICredential {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/organizations/org_security/organization", nil)
	claims := f.claims(&user, cloudassert.KindBrowser, request.Method, request.URL.RequestURI(), nil)
	session, binding, err := f.service.sharedAccessSession(t.Context(), f.service.echo.NewContext(request, httptest.NewRecorder()), claims)
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := f.service.hostedSharedCredential(t.Context(), session, binding, claims.Role)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func (f hostedSharedFixture) member(t *testing.T, name, role, grant string, subjects ...string) hostedSecurityUser {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	subject := "user_" + name
	if len(subjects) > 0 {
		subject = subjects[0]
	}
	identity := auth.Identity{Subject: subject, Email: name + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{
		Subject: subject, OrganizationID: "org_provider", SessionID: "session_" + name, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	f.provider.mu.Lock()
	f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
	f.provider.mu.Unlock()
	membership, err := f.provider.CreateMembership(t.Context(), identity.Subject, "org_provider", role)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.addHostedMember(t.Context(), identity, membership); err != nil {
		t.Fatal(err)
	}
	user := hostedSecurityUser{identity: identity}
	if grant != "" {
		f.grant(t, user, grant == "write", false)
	}
	return user
}

func TestHostedSharedEntryRejectsBypass(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	origin := hostedSharedKey(9)
	tests := []struct {
		name    string
		request hostedSharedRequest
		status  int
	}{
		{"unsigned organization page", hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization", unsigned: true}, http.StatusUnauthorized},
		{"unsigned native API with cookie", hostedSharedRequest{user: &owner, target: f.base + "/work-items", unsigned: true, cookie: "spoofed"}, http.StatusUnauthorized},
		{"unsigned bearer API", hostedSharedRequest{target: f.base + "/work-items", unsigned: true, bearer: "detent_token"}, http.StatusUnauthorized},
		{"untrusted signer", hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization", signer: origin}, http.StatusUnauthorized},
		{"other tenant audience", hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization", mutate: func(c *cloudassert.Claims) { c.Audience = "org_other" }}, http.StatusUnauthorized},
		{"stale allocation generation", hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization", mutate: func(c *cloudassert.Claims) { c.Generation = 2 }}, http.StatusUnauthorized},
		{"path substitution", hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization", mutate: func(c *cloudassert.Claims) { c.Path = "/organizations/org_security/projects/prj_security" }}, http.StatusUnauthorized},
		{"expired access token", hostedSharedRequest{user: &owner, target: f.base + "/work-items", mutate: func(c *cloudassert.Claims) { c.AccessExpiresAt = time.Now().Add(-time.Second) }}, http.StatusUnauthorized},
		{"browser assertion with bearer", hostedSharedRequest{user: &owner, target: f.base + "/work-items", bearer: "detent_token"}, http.StatusUnauthorized},
		{"machine assertion without bearer", hostedSharedRequest{kind: cloudassert.KindMachine, target: f.base + "/work-items"}, http.StatusUnauthorized},
		{"other organization prefix", hostedSharedRequest{user: &owner, target: "/organizations/org_other/organization"}, http.StatusNotFound},
		{"unscoped legacy page", hostedSharedRequest{user: &owner, target: "/organization"}, http.StatusNotFound},
		{"unscoped project page", hostedSharedRequest{user: &owner, target: "/projects/prj_security"}, http.StatusNotFound},
		{"legacy v1 API", hostedSharedRequest{kind: cloudassert.KindMachine, target: "/api/v1/work-items", bearer: "detent_token"}, http.StatusNotFound},
		{"entry-owned login", hostedSharedRequest{user: &owner, target: "/organizations/org_security/auth/oidc/start"}, http.StatusNotFound},
		{"entry-owned invitation", hostedSharedRequest{user: &owner, target: "/organizations/org_security/invite?invitation_token=x"}, http.StatusNotFound},
		{"nested organization prefix", hostedSharedRequest{user: &owner, target: "/organizations/org_security/organizations/org_security/organization"}, http.StatusNotFound},
		{"encoded separator", hostedSharedRequest{user: &owner, target: "/organizations/org_security/projects%2Fprj_security"}, http.StatusNotFound},
		{"dot segment", hostedSharedRequest{user: &owner, target: "/organizations/org_security/projects/../organization"}, http.StatusNotFound},
		{"empty segment", hostedSharedRequest{user: &owner, target: "/organizations/org_security//organization"}, http.StatusNotFound},
		{"duplicate scope parameter", hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization?cursor=a&cursor=b"}, http.StatusNotFound},
		{"browser assertion on internal route", hostedSharedRequest{user: &owner, method: http.MethodPost, target: "/internal/v1/sessions/revoke", body: `{"bindings":["x"]}`}, http.StatusNotFound},
		{"browser assertion on user search", hostedSharedRequest{user: &owner, method: http.MethodPost, target: "/internal/v1/platform/users", body: `{"q":"owner"}`}, http.StatusNotFound},
		{"service assertion on customer route", hostedSharedRequest{kind: cloudassert.KindService, target: "/organizations/org_security/organization"}, http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(t, test.request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private-project-sentinel") {
				t.Fatal("rejected response exposed tenant content")
			}
		})
	}
	for _, kind := range []string{cloudassert.KindBrowser, cloudassert.KindKey, cloudassert.KindMachine, cloudassert.KindService} {
		mutate := func(claims *cloudassert.Claims) {
			claims.OrganizationSlug = "example-org"
			if kind == cloudassert.KindKey {
				claims.Subject, claims.ProviderOrganization, claims.Role = owner.identity.Subject, "org_provider", "owner"
				claims.Key = &apikey.KeyAuthority{ID: "key_routes", Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
			}
		}
		user := &owner
		bearer := ""
		if kind != cloudassert.KindBrowser {
			user = nil
		}
		if kind == cloudassert.KindMachine {
			bearer = "detent_token"
		}
		missing := f.serve(t, hostedSharedRequest{user: user, kind: kind, bearer: bearer, target: "/missing-org/work", mutate: func(claims *cloudassert.Claims) {
			mutate(claims)
			claims.OrganizationSlug = ""
		}})
		if missing.Code != http.StatusNotFound {
			t.Fatalf("missing slug status = %d: %s", missing.Code, missing.Body.String())
		}
		for _, prefix := range []string{"/example-org", "/organizations/org_security"} {
			for _, path := range []string{
				"/internal/v1/sessions/revoke", "/internal/v1/github/webhook", "/internal/v1/keys/catalog", "/internal/v1/keys/call",
				"/organizations/org_security/work", "/login", "/auth/oidc/start", "/auth/oidc/callback", "/invite", "/logout",
				"/support", "/support/start", "/webhooks/stripe", "/organization/switch", "/organization/create", "/organization/join",
			} {
				t.Run(kind+prefix+path, func(t *testing.T) {
					response := f.serve(t, hostedSharedRequest{user: user, kind: kind, bearer: bearer, method: http.MethodPost, target: prefix + path, body: `{"bindings":["` + strings.Repeat("a", 64) + `"]}`, csrf: cloudassert.CSRFToken("shared-user_owner", "org_security"), mutate: mutate})
					if response.Code != missing.Code || response.Body.String() != missing.Body.String() {
						t.Fatalf("refusal = %d %s, want missing slug response %d %s", response.Code, response.Body.String(), missing.Code, missing.Body.String())
					}
				})
			}
		}
		if kind == cloudassert.KindBrowser || kind == cloudassert.KindKey {
			t.Run(kind+" readable API", func(t *testing.T) {
				response := f.serve(t, hostedSharedRequest{user: user, kind: kind, target: "/example-org" + f.base + "/work-items", mutate: mutate})
				if response.Code != http.StatusOK {
					t.Fatalf("readable API status = %d: %s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestHostedSharedEntryReplayIsDenied(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	request := httptest.NewRequest(http.MethodGet, "/organizations/org_security/organization", nil)
	value, err := cloudassert.Sign(f.key, f.claims(&owner, cloudassert.KindBrowser, http.MethodGet, request.URL.RequestURI(), nil))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{http.StatusOK, http.StatusUnauthorized} {
		request := httptest.NewRequest(http.MethodGet, "/organizations/org_security/organization", nil)
		request.Header.Set(cloudassert.Header, value)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", i, response.Code, want)
		}
	}
}

func TestHostedSharedEntryScopesPagesAndMutations(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t, func(c *Config) {
		c.StaticFS = fstest.MapFS{conversationClientShell: &fstest.MapFile{Data: []byte("<!doctype html><html><head></head><body></body></html>")}}
	})
	owner := f.member(t, "owner", "owner", "write")
	csrf := cloudassert.CSRFToken("shared-user_owner", "org_security")
	page := f.serve(t, hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization"})
	if page.Code != http.StatusOK {
		t.Fatalf("organization page status = %d: %s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{`href="/organizations/org_security/projects/prj_security"`, `action="/organizations/org_security/logout"`, `action="/organizations/org_security/organization/invite"`, `value="` + csrf + `"`, `href="/organizations?switch=1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("organization page is missing %s", want)
		}
	}
	for _, unscoped := range []string{`href="/organization"`, `action="/logout"`, `href="/projects/`, `action="/organization/switch"`, `/organization/create`, `/organization/join`} {
		if strings.Contains(body, unscoped) {
			t.Fatalf("organization page contains unscoped %s", unscoped)
		}
	}
	form := url.Values{"name": {"Shared project"}, "grant_access": {"true"}}
	for _, test := range []struct {
		name   string
		csrf   string
		status int
	}{
		{"missing token", "", http.StatusForbidden},
		{"token for another organization", cloudassert.CSRFToken("shared-user_owner", "org_other"), http.StatusForbidden},
		{"session token", csrf, http.StatusSeeOther},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := url.Values{}
			for key, value := range form {
				values[key] = value
			}
			if test.csrf != "" {
				values.Set("csrf", test.csrf)
			}
			response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: "/organizations/org_security/projects", body: values.Encode(), form: true})
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status == http.StatusSeeOther && !strings.HasPrefix(response.Header().Get("Location"), "/organizations/org_security/projects/") {
				t.Fatalf("redirect = %q", response.Header().Get("Location"))
			}
		})
	}
	mutate := func(claims *cloudassert.Claims) { claims.OrganizationSlug = "example-org" }
	raw, err := json.Marshal(tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "shared-readable"}, Title: "Readable issue", State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	created := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: "/example-org" + f.base + "/work-items", body: string(raw), csrf: csrf, mutate: mutate})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	for _, path := range []string{"/example-org/work", fmt.Sprintf("/example-org/private-project-sentinel/issues/%d", issue.Number)} {
		t.Run(path, func(t *testing.T) {
			response := f.serve(t, hostedSharedRequest{user: &owner, target: path, mutate: mutate})
			requireNativeStatus(t, response, http.StatusOK)
			if !strings.Contains(response.Body.String(), `<meta name="detent-router-base" content="/example-org">`) {
				t.Fatal("readable page is missing its router base")
			}
		})
	}
}

func TestHostedSharedEntryRevokesSessionsAndChecksProvider(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	viewer := f.member(t, "viewer", "viewer", "read")
	for _, handler := range []struct {
		name string
		call echo.HandlerFunc
	}{
		{"revocation", f.service.revokeHostedSharedSessions},
		{"owner bootstrap", f.service.bootstrapHostedSharedOwner},
		{"invitation acceptance", f.service.acceptHostedSharedInvitation},
		{"billing binding", f.service.hostedSharedBillingBinding},
		{"billing event", f.service.hostedSharedBillingEvent},
	} {
		for _, kind := range []string{"", cloudassert.KindBrowser, cloudassert.KindKey, cloudassert.KindMachine} {
			t.Run(handler.name+"/"+kind, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, "/internal/v1/test", strings.NewReader(`{"bindings":["`+cloudassert.AuthorizationBinding("shared-user_owner", "org_security", "session_owner")+`"]}`))
				request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				response := httptest.NewRecorder()
				c := f.service.echo.NewContext(request, response)
				if kind != "" {
					c.Set(hostedSharedClaimsKey, f.claims(&owner, kind, request.Method, request.URL.Path, nil))
				}
				if err := handler.call(c); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want not found: %s", response.Code, response.Body.String())
				}
			})
		}
	}
	target := f.base + "/work-items"
	if response := f.serve(t, hostedSharedRequest{user: &owner, target: target}); response.Code != http.StatusOK {
		t.Fatalf("owner read status = %d: %s", response.Code, response.Body.String())
	}
	binding := cloudassert.AuthorizationBinding("shared-user_owner", "org_security", "session_owner")
	revoke := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/sessions/revoke", body: `{"bindings":["` + binding + `"]}`})
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d: %s", revoke.Code, revoke.Body.String())
	}
	if response := f.serve(t, hostedSharedRequest{user: &owner, target: target}); response.Code == http.StatusOK {
		t.Fatal("revoked shared authorization still reads tenant content")
	}
	early := hostedSharedRequest{user: &viewer, target: target}
	earlyBinding := cloudassert.AuthorizationBinding("shared-user_viewer", "org_security", "session_viewer")
	if response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/sessions/revoke", body: `{"bindings":["` + earlyBinding + `"]}`}); response.Code != http.StatusNoContent {
		t.Fatalf("early revoke status = %d", response.Code)
	}
	if response := f.serve(t, early); response.Code == http.StatusOK {
		t.Fatal("binding revoked before first use still reads tenant content")
	}
	f.provider.mu.Lock()
	delete(f.provider.sessions, "session_viewer")
	f.provider.mu.Unlock()
	if response := f.serve(t, hostedSharedRequest{user: &viewer, target: target}); response.Code == http.StatusOK {
		t.Fatal("ended provider session still reads tenant content")
	}
	stranger := hostedSecurityUser{identity: auth.Identity{Subject: "user_stranger", Email: "stranger@example.test", Hosted: &auth.HostedIdentity{Subject: "user_stranger", OrganizationID: "org_provider", SessionID: "session_stranger", CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour)}}}
	f.provider.mu.Lock()
	f.provider.sessions["session_stranger"] = *stranger.identity.Hosted
	f.provider.mu.Unlock()
	if response := f.serve(t, hostedSharedRequest{user: &stranger, target: target}); response.Code == http.StatusOK {
		t.Fatal("non-member assertion reads tenant content")
	}
}

func TestHostedSharedEntryInvitationAcceptance(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	_ = owner
	invitation, err := f.provider.Invite(t.Context(), "org_provider", "invitee@example.test", "member", "user_owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		now := f.service.config.Clock()
		return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: "org_security", ID: invitation.ID, Email: "invitee@example.test", Role: "member", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	wrong := hostedSecurityUser{identity: auth.Identity{Subject: "user_wrong", Email: "wrong@example.test", Hosted: &auth.HostedIdentity{Subject: "user_wrong", SessionID: "session_wrong"}}}
	invitee := hostedSecurityUser{identity: auth.Identity{Subject: "user_invitee", Email: "invitee@example.test", Hosted: &auth.HostedIdentity{Subject: "user_invitee", SessionID: "session_invitee"}}}
	body := `{"token":"` + invitation.ID + `"}`
	for _, test := range []struct {
		name   string
		user   hostedSecurityUser
		status int
	}{
		{"different recipient", wrong, http.StatusForbidden},
		{"exact recipient", invitee, http.StatusNoContent},
		{"reused invitation", invitee, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(t, hostedSharedRequest{user: &test.user, kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/invitations/accept", body: body})
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
	var members int
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(t.Context(), "user_invitee")
		if err == nil && member.Active {
			members = 1
		}
		return err
	}); err != nil || members != 1 {
		t.Fatalf("invitee members = %d, %v", members, err)
	}
}

func TestHostedSharedBindingRequiresExplicitMigration(t *testing.T) {
	t.Parallel()
	key := hostedSharedKey(7)
	provider := newHostedSecurityProvider()
	origin := Config{Hosted: &hostedplans.HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", BootstrapSubject: "user_owner", PublicURL: "https://hub.example.test", Provider: provider}}
	service := openTestService(t, origin)
	originConfig := service.config
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), reopenHostedSharedPostgres(originConfig, hostedSharedTestConfig(provider, key, 1))); !errors.Is(err, ErrHostedDatabaseBinding) {
		t.Fatalf("origin database opened in shared mode: %v", err)
	}
	shared := openTestService(t, hostedSharedTestConfig(provider, key, 1))
	if _, err := hostedPostgresAdmin(t, shared).ExecContext(t.Context(), "UPDATE hosted_tenant SET allocation_generation=2 WHERE organization_id=$1", "org_security"); err == nil {
		t.Fatal("generation changed without a migration record")
	}
	if _, err := hostedPostgresAdmin(t, shared).ExecContext(t.Context(), "UPDATE hosted_tenant SET deployment='origin' WHERE organization_id=$1", "org_security"); err == nil {
		t.Fatal("deployment changed without a migration record")
	}
	sharedConfig := shared.config
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	matching, err := Open(t.Context(), reopenHostedSharedPostgres(sharedConfig, hostedSharedTestConfig(provider, key, 1)))
	if err != nil {
		t.Fatalf("matching PostgreSQL binding failed to reopen: %v", err)
	}
	if err := matching.Close(); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]Config{
		"origin reopen":    origin,
		"stale generation": hostedSharedTestConfig(provider, key, 2),
		"local reopen":     {},
		"different shared host": func() Config {
			c := hostedSharedTestConfig(provider, key, 1)
			c.Hosted.PublicURL = "https://other.example.test"
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			service, err := Open(t.Context(), reopenHostedSharedPostgres(sharedConfig, cfg))
			if err == nil {
				_ = service.Close()
				t.Fatal("database opened with a mismatched binding")
			}
		})
	}
}

func TestHostedSharedSupportSessionAudit(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	f.member(t, "owner", "owner", "write")
	now := time.Now().UTC().Truncate(time.Second)
	support := hostedSecurityUser{identity: auth.Identity{Subject: "user_owner", Email: "owner@example.test", Hosted: &auth.HostedIdentity{
		Subject: "user_owner", OrganizationID: "org_provider", SessionID: "session_support", CreatedAt: now, ExpiresAt: now.Add(time.Hour), SupportActor: "support@example.test", SupportReason: "troubleshooting",
	}}}
	f.provider.mu.Lock()
	f.provider.sessions["session_support"] = *support.identity.Hosted
	f.provider.mu.Unlock()
	for range 2 {
		if response := f.serve(t, hostedSharedRequest{user: &support, target: "/organizations/org_security/organization"}); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "support@example.test is acting as owner@example.test") {
			t.Fatalf("support page status = %d", response.Code)
		}
	}
	unlisted := support
	unlistedHosted := *support.identity.Hosted
	unlistedHosted.SupportActor, unlistedHosted.SessionID, unlistedHosted.SupportReason = "outsider@example.test", "session_outsider", "invalid"
	unlisted.identity.Hosted = &unlistedHosted
	f.provider.mu.Lock()
	f.provider.sessions["session_outsider"] = unlistedHosted
	f.provider.mu.Unlock()
	if response := f.serve(t, hostedSharedRequest{user: &unlisted, target: "/organizations/org_security/organization"}); response.Code == http.StatusOK {
		t.Fatal("tenant accepted an invalid support reason")
	}
	binding := cloudassert.AuthorizationBinding("shared-user_owner", "org_security", "session_support")
	if response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/sessions/revoke", body: `{"bindings":["` + binding + `"]}`}); response.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", response.Code)
	}
	rows, err := hostedPostgresAdmin(t, f.service).QueryContext(t.Context(), "SELECT event, actual_actor, effective_user, reason FROM hosted_audit WHERE organization_id=$1 AND event LIKE 'session_%' ORDER BY id", "org_security")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var event, actor, effective, reason string
		if err := rows.Scan(&event, &actor, &effective, &reason); err != nil {
			t.Fatal(err)
		}
		if actor != "support@example.test" || effective != "user_owner" || reason != "troubleshooting" {
			t.Fatalf("audit row = %s %s %s %s", event, actor, effective, reason)
		}
		events = append(events, event)
	}
	if strings.Join(events, ",") != "session_started,session_ended" {
		t.Fatalf("support audit events = %v", events)
	}
}

func TestHostedSharedOwnerBootstrap(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	identity := func(subject string) *hostedSecurityUser {
		return &hostedSecurityUser{identity: auth.Identity{Subject: subject, Email: subject + "@example.test", Hosted: &auth.HostedIdentity{Subject: subject}}}
	}
	if _, err := f.provider.CreateMembership(t.Context(), "user_member", "org_provider", "member"); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"user_creator", "user_second_owner"} {
		if _, err := f.provider.CreateMembership(t.Context(), subject, "org_provider", "owner"); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"organization_name":"Created organization"}`
	for _, test := range []struct {
		name    string
		subject string
		body    string
		status  int
	}{
		{"not a provider member", "user_stranger", body, http.StatusForbidden},
		{"provider member without ownership", "user_member", body, http.StatusForbidden},
		{"missing name", "user_creator", `{"organization_name":""}`, http.StatusUnprocessableEntity},
		{"verified owner", "user_creator", body, http.StatusNoContent},
		{"repeat", "user_creator", body, http.StatusNoContent},
		{"second owner claim", "user_second_owner", body, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(t, hostedSharedRequest{user: identity(test.subject), kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/owner/bootstrap", body: test.body})
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
	var name, role string
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		organization, err := r.AccessOrganization(t.Context())
		if err != nil {
			return err
		}
		member, err := r.AccessMember(t.Context(), "user_creator")
		name, role = organization.Name, member.Role
		return err
	}); err != nil || name != "Created organization" || role != "owner" {
		t.Fatalf("bootstrap result = %q %q %v", name, role, err)
	}
	if response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/health", body: `{}`}); response.Code != http.StatusOK {
		t.Fatalf("health = %d", response.Code)
	}
}

func reopenHostedSharedPostgres(source, target Config) Config {
	target.DatabaseBackend, target.Postgres = source.DatabaseBackend, source.Postgres
	target.SecretKeys, target.Content, target.RunDetails = source.SecretKeys, source.Content, source.RunDetails
	target.TenantRepository, target.AccessRepository, target.BillingRepository = nil, nil, nil
	target.NativeRunEvents, target.ProjectParticipants, target.ConversationDispatch, target.ChangeVersionAuthority = nil, nil, nil, nil
	return target
}
