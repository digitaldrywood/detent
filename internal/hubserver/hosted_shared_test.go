package hubserver

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
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

func hostedSharedTestConfig(path string, provider auth.HostedProvider, key ed25519.PrivateKey, generation int64) Config {
	return Config{DatabasePath: path, GitHubDisabled: true, Hosted: &HostedConfig{
		OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", BootstrapSubject: "user_owner",
		PublicURL: "https://hub.example.test", StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}, Provider: provider,
		SharedEntry: &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: generation},
	}}
}

func newHostedSharedFixture(t *testing.T) hostedSharedFixture {
	t.Helper()
	key := hostedSharedKey(7)
	provider := newHostedSecurityProvider()
	service := openTestService(t, hostedSharedTestConfig(hostedTestDatabasePath(t), provider, key, 1))
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	raw, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	project := tracker.ProjectID("prj_security")
	now := formatHubTime(time.Now())
	if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) VALUES (?,?,'private-project-sentinel','native',?,?,0)", project, "org_security", string(raw), now); err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES (?,?,?,?,?,?,?)", project, state.Name, state.Name, state.Terminal, state.Dispatchable, now, now); err != nil {
			t.Fatal(err)
		}
	}
	return hostedSharedFixture{hostedSecurityFixture: hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/org_security/projects/" + string(project)}, key: key}
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
		claims.Binding = cloudassert.AuthorizationBinding("shared-"+hosted.Subject, "org_security", hosted.SessionID)
		claims.CSRF = cloudassert.CSRFToken("shared-"+hosted.Subject, "org_security")
	}
	if user != nil && kind == cloudassert.KindService {
		claims.Subject, claims.Email, claims.ProviderSession = user.identity.Subject, user.identity.Email, user.identity.Hosted.SessionID
	}
	return claims
}

type hostedSharedRequest struct {
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

func (f hostedSharedFixture) member(t *testing.T, name, role, grant string) hostedSecurityUser {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	identity := auth.Identity{Subject: "user_" + name, Email: name + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{
		Subject: "user_" + name, OrganizationID: "org_provider", SessionID: "session_" + name, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
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
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	csrf := cloudassert.CSRFToken("shared-user_owner", "org_security")
	page := f.serve(t, hostedSharedRequest{user: &owner, target: "/organizations/org_security/organization"})
	if page.Code != http.StatusOK {
		t.Fatalf("organization page status = %d: %s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{`href="/organizations/org_security/projects/prj_security"`, `action="/organizations/org_security/logout"`, `action="/organizations/org_security/organization/invite"`, `value="` + csrf + `"`, `href="/organizations"`} {
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
}

func TestHostedSharedEntryRevokesSessionsAndChecksProvider(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	viewer := f.member(t, "viewer", "viewer", "read")
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
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO hosted_invitations(id,email,organization_id,role,created_at) VALUES (?,?,?,?,?)", invitation.ID, "invitee@example.test", "org_security", "member", formatHubTime(time.Now())); err != nil {
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
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_members WHERE user_id = 'user_invitee' AND active = 1").Scan(&members); err != nil || members != 1 {
		t.Fatalf("invitee members = %d, %v", members, err)
	}
}

func TestHostedSharedBindingRequiresExplicitMigration(t *testing.T) {
	t.Parallel()
	key := hostedSharedKey(7)
	provider := newHostedSecurityProvider()
	originPath := hostedTestDatabasePath(t)
	origin := Config{DatabasePath: originPath, GitHubDisabled: true, Hosted: &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", BootstrapSubject: "user_owner", PublicURL: "https://hub.example.test", Provider: provider}}
	service := openTestService(t, origin)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), hostedSharedTestConfig(originPath, provider, key, 1)); !errors.Is(err, ErrHostedDatabaseBinding) {
		t.Fatalf("origin database opened in shared mode: %v", err)
	}
	sharedPath := hostedTestDatabasePath(t)
	shared := openTestService(t, hostedSharedTestConfig(sharedPath, provider, key, 1))
	if _, err := shared.database.db.ExecContext(t.Context(), "UPDATE hosted_tenant SET allocation_generation = 2"); err == nil {
		t.Fatal("generation changed without a migration record")
	}
	if _, err := shared.database.db.ExecContext(t.Context(), "UPDATE hosted_tenant SET deployment = 'origin'"); err == nil {
		t.Fatal("deployment changed without a migration record")
	}
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]Config{
		"origin reopen":    func() Config { c := origin; c.DatabasePath = sharedPath; return c }(),
		"stale generation": hostedSharedTestConfig(sharedPath, provider, key, 2),
		"local reopen":     {DatabasePath: sharedPath, GitHubDisabled: true},
		"different shared host": func() Config {
			c := hostedSharedTestConfig(sharedPath, provider, key, 1)
			c.Hosted.PublicURL = "https://other.example.test"
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			service, err := Open(t.Context(), cfg)
			if err == nil {
				_ = service.Close()
				t.Fatal("database opened with a mismatched binding")
			}
		})
	}
}

func TestHostedSharedConfigValidation(t *testing.T) {
	t.Parallel()
	key := hostedSharedKey(7).Public().(ed25519.PublicKey)
	valid := func() *HostedConfig {
		return &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider(),
			SharedEntry: &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key}, Generation: 1}}
	}
	tests := []struct {
		name   string
		mutate func(*HostedConfig)
		ok     bool
	}{
		{"valid", func(*HostedConfig) {}, true},
		{"missing issuer", func(c *HostedConfig) { c.SharedEntry.Issuer = "" }, false},
		{"zero generation", func(c *HostedConfig) { c.SharedEntry.Generation = 0 }, false},
		{"no keys", func(c *HostedConfig) { c.SharedEntry.PublicKeys = nil }, false},
		{"short key", func(c *HostedConfig) { c.SharedEntry.PublicKeys = []ed25519.PublicKey{key[:5]} }, false},
		{"directory", func(c *HostedConfig) {
			c.Directory = []HostedDestination{{OrganizationID: "org_b", WorkOSOrganizationID: "org_provider_b", PublicURL: "https://b.example.test"}}
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid()
			test.mutate(cfg)
			if err := cfg.validate(); (err == nil) != test.ok {
				t.Fatalf("validate error = %v, want ok %v", err, test.ok)
			}
		})
	}
}

func TestMigrateHostedSharedOrigin(t *testing.T) {
	t.Parallel()
	key := hostedSharedKey(7)
	provider := newHostedSecurityProvider()
	path := hostedTestDatabasePath(t)
	origin := Config{DatabasePath: path, GitHubDisabled: true, Hosted: &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", BootstrapSubject: "user_owner", PublicURL: "https://org.example.test", Provider: provider}}
	service := openTestService(t, origin)
	fixture := hostedSharedFixture{hostedSecurityFixture: hostedSecurityFixture{service: service, provider: provider}, key: key}
	owner := fixture.member(t, "owner", "owner", "")
	if _, _, err := service.hostedSessions.CreateIdentitySession(t.Context(), owner.identity); err != nil {
		t.Fatal(err)
	}
	hostedLoginExec(t, service, "INSERT INTO hosted_transactions(token_hash,state,verifier,organization_id,expires_at) VALUES ('tx','state','verifier','org_provider',?)", formatHubTime(time.Now().Add(time.Minute)))
	running := hostedSharedTestConfig(path, provider, key, 1)
	if _, err := MigrateHostedSharedOrigin(t.Context(), running); err == nil {
		t.Fatal("migration ran while the tenant owned its database")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	mismatched := hostedSharedTestConfig(path, provider, key, 1)
	mismatched.Hosted.OrganizationID = "org_other"
	if _, err := MigrateHostedSharedOrigin(t.Context(), mismatched); !errors.Is(err, ErrHostedMigrationMismatch) {
		t.Fatalf("mismatched organization error = %v", err)
	}
	unallocated := hostedSharedTestConfig(path, provider, key, 1)
	unallocated.Hosted.WorkOSOrganizationID = "org_provider_other"
	if _, err := MigrateHostedSharedOrigin(t.Context(), unallocated); !errors.Is(err, ErrHostedMigrationMismatch) {
		t.Fatalf("mismatched provider error = %v", err)
	}
	if _, err := MigrateHostedSharedOrigin(t.Context(), origin); err == nil {
		t.Fatal("migration accepted a target without shared_entry")
	}
	result, err := MigrateHostedSharedOrigin(t.Context(), running)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlreadyMigrated || result.FromDeployment != "origin" || result.FromPublicURL != "https://org.example.test" || result.Generation != 1 || result.Members != 1 || result.SessionsRevoked != 1 || result.TransactionsClosed != 1 {
		t.Fatalf("migration result = %+v", result)
	}
	repeated, err := MigrateHostedSharedOrigin(t.Context(), running)
	if err != nil || !repeated.AlreadyMigrated {
		t.Fatalf("repeated migration = %+v, %v", repeated, err)
	}
	if _, err := Open(t.Context(), origin); !errors.Is(err, ErrHostedDatabaseBinding) {
		t.Fatalf("migrated database reopened on its old origin: %v", err)
	}
	moved := hostedSharedTestConfig(path, provider, key, 1)
	moved.Hosted.PublicURL = "https://moved.example.test"
	if _, err := MigrateHostedSharedOrigin(t.Context(), moved); !errors.Is(err, ErrHostedMigrationMismatch) {
		t.Fatalf("same-generation move error = %v", err)
	}
	rebound := hostedSharedTestConfig(path, provider, key, 2)
	if _, err := MigrateHostedSharedOrigin(t.Context(), rebound); !errors.Is(err, ErrHostedMigrationMismatch) {
		t.Fatalf("shared rebind error = %v", err)
	}
	shared := openTestService(t, running)
	var members, revoked int
	if err := shared.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_members WHERE user_id = 'user_owner' AND active = 1), (SELECT count(*) FROM hosted_sessions WHERE revoked_at IS NOT NULL)").Scan(&members, &revoked); err != nil || members != 1 || revoked != 1 {
		t.Fatalf("members = %d revoked = %d err = %v", members, revoked, err)
	}
	if _, err := shared.database.db.ExecContext(t.Context(), "DELETE FROM hosted_binding_migrations"); err == nil {
		t.Fatal("migration history was deleted")
	}
	if _, err := shared.database.db.ExecContext(t.Context(), "UPDATE hosted_binding_migrations SET applied = 0"); err == nil {
		t.Fatal("applied migration was reopened")
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
	unlistedHosted.SupportActor, unlistedHosted.SessionID = "outsider@example.test", "session_outsider"
	unlisted.identity.Hosted = &unlistedHosted
	f.provider.mu.Lock()
	f.provider.sessions["session_outsider"] = unlistedHosted
	f.provider.mu.Unlock()
	if response := f.serve(t, hostedSharedRequest{user: &unlisted, target: "/organizations/org_security/organization"}); response.Code == http.StatusOK {
		t.Fatal("tenant accepted a support actor it does not list")
	}
	binding := cloudassert.AuthorizationBinding("shared-user_owner", "org_security", "session_support")
	if response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/sessions/revoke", body: `{"bindings":["` + binding + `"]}`}); response.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", response.Code)
	}
	rows, err := f.service.database.db.QueryContext(t.Context(), "SELECT event, actual_actor, effective_user, reason FROM hosted_audit WHERE event LIKE 'session_%' ORDER BY id")
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
