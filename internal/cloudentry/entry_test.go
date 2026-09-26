package cloudentry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

const (
	testPublicURL = "http://127.0.0.1:18017"
	testAdminKey  = "detent_test_admin_token_0123456789abcdefghijklmnop"
)

type fakeProvider struct {
	organizations  map[string]auth.Organization
	createFailures int
	creates        int
	mu             sync.Mutex
	users          map[string]string
	sessions       map[string]auth.HostedIdentity
	memberships    map[string]auth.Membership
	invitations    map[string]auth.Invitation
	revoked        []string
	sequence       int
	authorizeBase  string
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{users: map[string]string{}, sessions: map[string]auth.HostedIdentity{}, memberships: map[string]auth.Membership{}, invitations: map[string]auth.Invitation{}}
}

func (p *fakeProvider) member(user, organization, role string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	membership := auth.Membership{ID: "om_" + user + "_" + organization, UserID: user, OrganizationID: organization, Status: "active"}
	membership.Role.Slug = role
	p.memberships[membership.ID] = membership
}

func (p *fakeProvider) removeMember(user, organization string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.memberships, "om_"+user+"_"+organization)
}

func (p *fakeProvider) AuthorizationURL(state, _, verifier string) string {
	if p.authorizeBase != "" {
		return p.authorizeBase + "/__preview/authorize?" + url.Values{"state": {state}}.Encode()
	}
	return "https://identity.example.test/authorize?" + url.Values{"state": {state}, "verifier": {verifier}}.Encode()
}

func (p *fakeProvider) Exchange(_ context.Context, code, _, _ string) (auth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if parts := strings.Split(code, "|"); len(parts) == 5 && parts[0] == "support" {
		p.sequence++
		now := time.Now().UTC().Truncate(time.Second)
		hosted := auth.HostedIdentity{Subject: parts[2], OrganizationID: parts[3], SessionID: "support_session_" + string(rune('a'+p.sequence)), CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), SupportActor: parts[1], SupportReason: parts[4]}
		p.sessions[hosted.SessionID] = hosted
		return auth.Identity{Subject: parts[2], Email: p.users[parts[2]], EmailVerified: true, Hosted: &hosted}, nil
	}
	user, organization, _ := strings.Cut(code, ":")
	email, ok := p.users[user]
	if !ok {
		return auth.Identity{}, auth.ErrHostedIdentity
	}
	if organization != "" {
		if _, ok := p.memberships["om_"+user+"_"+organization]; !ok {
			return auth.Identity{}, auth.ErrHostedIdentity
		}
	}
	p.sequence++
	now := time.Now().UTC().Truncate(time.Second)
	hosted := auth.HostedIdentity{Subject: user, OrganizationID: organization, SessionID: "session_" + user + "_" + string(rune('a'+p.sequence)), CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)}
	p.sessions[hosted.SessionID] = hosted
	return auth.Identity{Subject: user, Email: email, EmailVerified: true, Hosted: &hosted}, nil
}

func (p *fakeProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.sessions[identity.SessionID]
	if !ok || current.Subject != identity.Subject || !current.CreatedAt.Equal(identity.CreatedAt) {
		return auth.HostedIdentity{}, auth.ErrHostedIdentity
	}
	return current, nil
}

func (p *fakeProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result []auth.Membership
	for _, membership := range p.memberships {
		if (user == "" || membership.UserID == user) && (organization == "" || membership.OrganizationID == organization) {
			result = append(result, membership)
		}
	}
	return result, nil
}

func (*fakeProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	return auth.Organization{ID: id, Name: id}, nil
}

func (p *fakeProvider) CreateOrganization(_ context.Context, external, name string) (auth.Organization, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.createFailures > 0 {
		p.createFailures--
		return auth.Organization{}, auth.ErrHostedIdentity
	}
	if p.organizations == nil {
		p.organizations = map[string]auth.Organization{}
	}
	if existing, ok := p.organizations[external]; ok {
		return existing, nil
	}
	p.creates++
	organization := auth.Organization{ID: "porg_" + external, ExternalID: external, Name: name}
	p.organizations[external] = organization
	return organization, nil
}

func (p *fakeProvider) CreateMembership(_ context.Context, user, organization, role string) (auth.Membership, error) {
	p.member(user, organization, role)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.memberships["om_"+user+"_"+organization], nil
}

func (*fakeProvider) SetMembershipRole(context.Context, string, string) error { return nil }
func (*fakeProvider) RevokeMembership(context.Context, string) error          { return nil }

func (p *fakeProvider) Invite(_ context.Context, organization, email, _, _ string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation := auth.Invitation{ID: "inv_" + strings.ReplaceAll(strings.Split(email, "@")[0], ".", "_"), Email: email, OrganizationID: organization, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
	p.invitations[invitation.ID] = invitation
	return invitation, nil
}

func (p *fakeProvider) Invitation(_ context.Context, token string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok {
		return auth.Invitation{}, auth.ErrHostedIdentity
	}
	return invitation, nil
}

func (p *fakeProvider) AcceptInvitation(_ context.Context, token, user string) error {
	p.mu.Lock()
	invitation := p.invitations[token]
	invitation.State, invitation.AcceptedUserID = "accepted", user
	p.invitations[token] = invitation
	p.mu.Unlock()
	p.member(user, invitation.OrganizationID, "member")
	return nil
}

func (p *fakeProvider) RevokeSession(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
	p.revoked = append(p.revoked, id)
	return nil
}

type browser struct {
	t     *testing.T
	entry http.Handler
	jar   *cookiejar.Jar
}

type page struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func newBrowser(t *testing.T, entry http.Handler) *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{t: t, entry: entry, jar: jar}
}

func (b *browser) do(method, target string, form url.Values, headers map[string]string) page {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, testPublicURL+target, body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method != http.MethodGet {
		request.Header.Set("Origin", testPublicURL)
	}
	for key, value := range headers {
		if value == "" {
			request.Header.Del(key)
		} else {
			request.Header.Set(key, value)
		}
	}
	base, _ := url.Parse(testPublicURL)
	for _, cookie := range b.jar.Cookies(base) {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	b.entry.ServeHTTP(recorder, request)
	b.jar.SetCookies(base, (&http.Response{Header: recorder.Header()}).Cookies())
	return page{StatusCode: recorder.Code, Header: recorder.Header(), Body: recorder.Body.String()}
}

func (b *browser) get(target string) (page, string) {
	b.t.Helper()
	response := b.do(http.MethodGet, target, nil, nil)
	return response, response.Body
}

func (b *browser) login(target, code string) string {
	b.t.Helper()
	response, body := b.get(target)
	if response.StatusCode != http.StatusSeeOther {
		b.t.Fatalf("GET %s status = %d: %s", target, response.StatusCode, body)
	}
	location := response.Header.Get("Location")
	if strings.HasPrefix(location, "/auth/oidc/start") {
		response, _ = b.get(location)
		location = response.Header.Get("Location")
	}
	provider, err := url.Parse(location)
	if err != nil || provider.Host != "identity.example.test" {
		b.t.Fatalf("login redirect = %q", location)
	}
	callback, _ := b.get("/auth/oidc/callback?" + url.Values{"code": {code}, "state": {provider.Query().Get("state")}}.Encode())
	if callback.StatusCode != http.StatusSeeOther {
		b.t.Fatalf("callback status = %d", callback.StatusCode)
	}
	return callback.Header.Get("Location")
}

type tenantFixture struct {
	id, provider string
	path         string
	config       hubserver.Config
}

func originLogin(t *testing.T, handler http.Handler, provider *fakeProvider, user, organization string) string {
	t.Helper()
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/auth/oidc/start", nil))
	location, _ := url.Parse(start.Header().Get("Location"))
	request := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?"+url.Values{"code": {user + ":" + organization}, "state": {location.Query().Get("state")}}.Encode(), nil)
	for _, cookie := range start.Result().Cookies() {
		request.AddCookie(cookie)
	}
	callback := httptest.NewRecorder()
	handler.ServeHTTP(callback, request)
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == "detent_hosted_session" && cookie.Value != "" {
			return cookie.Value
		}
	}
	t.Fatalf("origin login failed: %d %s", callback.Code, callback.Body.String())
	return ""
}

func originPost(t *testing.T, handler http.Handler, token, target string, form url.Values) {
	t.Helper()
	sum := sha256.Sum256([]byte("detent-hosted-csrf:" + token))
	form.Set("csrf", hex.EncodeToString(sum[:]))
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "detent_hosted_session", Value: token})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("origin POST %s status = %d: %s", target, recorder.Code, recorder.Body.String())
	}
}

func newTenant(t *testing.T, provider *fakeProvider, key ed25519.PrivateKey, id, providerID, owner, project string) (tenantFixture, http.Handler) {
	t.Helper()
	path := filepath.Join(t.TempDir(), id+".db")
	hosted := func(shared bool) *hubserver.HostedConfig {
		config := &hubserver.HostedConfig{OrganizationID: id, WorkOSOrganizationID: providerID, BootstrapSubject: owner, PublicURL: map[string]string{"org_alpha": "http://127.0.0.1:19001", "org_beta": "http://127.0.0.1:19002"}[id], Provider: provider, StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}}
		if shared {
			config.PublicURL = testPublicURL
			config.SharedEntry = &hubserver.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
		}
		return config
	}
	logger := slog.New(slog.DiscardHandler)
	origin, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: path, GitHubDisabled: true, Hosted: hosted(false), Logger: logger, InitialAdminToken: []byte(testAdminKey)})
	if err != nil {
		t.Fatal(err)
	}
	token := originLogin(t, origin.Handler(), provider, owner, providerID)
	originPost(t, origin.Handler(), token, "/projects", url.Values{"name": {project}, "grant_access": {"true"}})
	if err := origin.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := tenantFixture{id: id, provider: providerID, path: path, config: hubserver.Config{DatabasePath: path, GitHubDisabled: true, Hosted: hosted(true), Logger: logger}}
	if _, err := hubserver.MigrateHostedSharedOrigin(t.Context(), fixture.config); err != nil {
		t.Fatal(err)
	}
	shared, err := hubserver.Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	return fixture, shared.Handler()
}

type entryFixture struct {
	service  *Service
	provider *fakeProvider
	tenants  map[string]http.Handler
}

func newEntryFixture(t *testing.T) entryFixture {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 42
	key := ed25519.NewKeyFromSeed(seed)
	provider := newFakeProvider()
	provider.users["user_alice"] = "alice@example.test"
	provider.users["user_bob"] = "bob@example.test"
	provider.users["user_carol"] = "carol@example.test"
	provider.users["user_support"] = "support@example.test"
	provider.member("user_alice", "porg_alpha", "owner")
	provider.member("user_alice", "porg_beta", "owner")
	provider.member("user_bob", "porg_beta", "member")
	alpha, alphaHandler := newTenant(t, provider, key, "org_alpha", "porg_alpha", "user_alice", "Alpha secret project")
	beta, betaHandler := newTenant(t, provider, key, "org_beta", "porg_beta", "user_alice", "Beta secret project")
	tenants := map[string]http.Handler{"unix:/tenants/alpha.sock": alphaHandler, "unix:/tenants/beta.sock": betaHandler}
	service, err := Open(t.Context(), Config{
		PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: key, Provider: provider, StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}, StateDir: t.TempDir(),
		Logger: slog.New(slog.DiscardHandler),
		transport: func(organization Organization) (http.RoundTripper, error) {
			return handlerTransport{tenants[organization.Endpoint]}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	for _, tenant := range []struct {
		fixture  tenantFixture
		endpoint string
		name     string
	}{{alpha, "unix:/tenants/alpha.sock", "Alpha"}, {beta, "unix:/tenants/beta.sock", "Beta"}} {
		if _, err := service.Registry().Register(t.Context(), Organization{ID: tenant.fixture.id, ProviderID: tenant.fixture.provider, Name: tenant.name, Endpoint: tenant.endpoint, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return entryFixture{service: service, provider: provider, tenants: tenants}
}

type handlerTransport struct {
	handler http.Handler
}

func (h handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	server := httptest.NewRequest(request.Method, request.URL.RequestURI(), request.Body)
	server.Header = request.Header.Clone()
	server = server.WithContext(request.Context())
	h.handler.ServeHTTP(recorder, server)
	return recorder.Result(), nil
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `name="csrf" value="`)
	if !ok {
		t.Fatalf("page has no CSRF token: %s", body)
	}
	value, _, _ := strings.Cut(rest, `"`)
	return value
}

func TestSharedEntryTwoOrganizationsOneOrigin(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	if location := alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha"); location != "/organizations/org_alpha/organization" {
		t.Fatalf("post-login location = %q", location)
	}
	response, alphaPage := alice.get("/organizations/org_alpha/organization")
	if response.StatusCode != http.StatusOK || !strings.Contains(alphaPage, "Alpha secret project") || strings.Contains(alphaPage, "Beta secret project") {
		t.Fatalf("alpha page status = %d body = %s", response.StatusCode, alphaPage)
	}
	if !strings.Contains(alphaPage, `href="/organizations/org_alpha/projects/`) || response.Header.Get("Content-Security-Policy") == "" || len(response.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("alpha page is not scoped or hardened: %v", response.Header)
	}
	unauthorized, _ := alice.get("/organizations/org_beta/organization")
	if unauthorized.StatusCode != http.StatusSeeOther || !strings.HasPrefix(unauthorized.Header.Get("Location"), "/auth/oidc/start?organization=org_beta") {
		t.Fatalf("unauthorized beta = %d %q", unauthorized.StatusCode, unauthorized.Header.Get("Location"))
	}
	base, _ := url.Parse(testPublicURL)
	stale := newBrowser(t, f.service.Handler())
	stale.jar.SetCookies(base, alice.jar.Cookies(base))
	alice.login("/organizations/org_beta/organization", "user_alice:porg_beta")
	if response, _ := stale.get("/organizations/org_alpha/organization"); response.StatusCode == http.StatusOK {
		t.Fatal("the pre-authentication session cookie survived re-authentication")
	}
	_, betaPage := alice.get("/organizations/org_beta/organization")
	if !strings.Contains(betaPage, "Beta secret project") || strings.Contains(betaPage, "Alpha secret project") {
		t.Fatalf("beta page = %s", betaPage)
	}
	response, alphaAgain := alice.get("/organizations/org_alpha/organization")
	if response.StatusCode != http.StatusOK || !strings.Contains(alphaAgain, "Alpha secret project") {
		t.Fatal("authorizing beta replaced the alpha authorization")
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
		{"scoped token", alphaCSRF, testPublicURL, http.StatusSeeOther},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := alice.do(http.MethodPost, "/organizations/org_alpha/projects", url.Values{"name": {"Created " + test.name}, "grant_access": {"true"}, "csrf": {test.csrf}}, map[string]string{"Origin": test.origin})
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
		})
	}
	_, alphaAfter := alice.get("/organizations/org_alpha/organization")
	_, betaAfter := alice.get("/organizations/org_beta/organization")
	if !strings.Contains(alphaAfter, "Created scoped token") || strings.Contains(betaAfter, "Created scoped token") {
		t.Fatal("scoped mutation reached the wrong organization")
	}
	_, chooser := alice.get("/organizations")
	if strings.Contains(chooser, `href="/organization"`) {
		t.Fatal("entry pages link to the unscoped tenant route")
	}
	if !strings.Contains(chooser, `href="/organizations/org_alpha/organization"`) || !strings.Contains(chooser, `href="/organizations/org_beta/organization"`) {
		t.Fatalf("chooser = %s", chooser)
	}
	f.provider.removeMember("user_alice", "porg_beta")
	if response, _ := alice.get("/organizations/org_beta/organization"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("removed membership status = %d", response.StatusCode)
	}
	if response, _ := alice.get("/organizations/org_alpha/organization"); response.StatusCode != http.StatusOK {
		t.Fatal("removing beta membership affected alpha")
	}
	logout := alice.do(http.MethodPost, "/organizations/org_alpha/logout", url.Values{"csrf": {alphaCSRF}}, nil)
	if logout.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d", logout.StatusCode)
	}
	if response, _ := alice.get("/organizations/org_alpha/organization"); response.StatusCode != http.StatusSeeOther {
		t.Fatal("signed-out browser still reads alpha")
	}
	if len(f.provider.revoked) < 2 {
		t.Fatalf("provider revoked sessions = %v", f.provider.revoked)
	}
}

func TestSharedEntryBoundaries(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
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
		{"open redirect", alice, "/auth/oidc/start?return=%2F%2Fattacker.example.test", nil, http.StatusBadRequest},
		{"cross-organization return", alice, "/auth/oidc/start?organization=org_alpha&return=%2Forganizations%2Forg_beta%2Forganization", nil, http.StatusBadRequest},
		{"legacy unscoped route", alice, "/organization", nil, http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := test.client.do(http.MethodGet, test.target, nil, test.header)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, test.status, response.Body)
			}
			if strings.Contains(response.Body, "Alpha secret project") {
				t.Fatal("boundary response exposed tenant content")
			}
		})
	}
	metadata := stranger.do(http.MethodGet, "/organizations/org_alpha/api/cloud/metadata", nil, map[string]string{"Authorization": "Bearer " + testAdminKey})
	if metadata.StatusCode != http.StatusOK {
		t.Fatalf("machine metadata status = %d", metadata.StatusCode)
	}
}

func TestSharedEntryLoginTransactions(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	first, _ := alice.get("/auth/oidc/start?organization=org_alpha")
	second, _ := alice.get("/auth/oidc/start?organization=org_beta")
	firstState := stateOf(t, first)
	secondState := stateOf(t, second)
	if response, _ := alice.get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_beta"}, "state": {secondState}}.Encode()); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("second tab callback = %d", response.StatusCode)
	}
	if response, _ := alice.get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_alpha"}, "state": {firstState}}.Encode()); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("first tab callback = %d", response.StatusCode)
	}
	if response, _ := alice.get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_alpha"}, "state": {firstState}}.Encode()); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed callback = %d", response.StatusCode)
	}
	wrong, _ := alice.get("/auth/oidc/start?organization=org_alpha")
	if response, _ := alice.get("/auth/oidc/callback?" + url.Values{"code": {"user_alice:porg_beta"}, "state": {stateOf(t, wrong)}}.Encode()); response.StatusCode != http.StatusForbidden {
		t.Fatalf("organization mismatch = %d", response.StatusCode)
	}
	for _, organization := range []string{"org_alpha", "org_beta"} {
		if response, _ := alice.get("/organizations/" + organization + "/organization"); response.StatusCode != http.StatusOK {
			t.Fatalf("%s after concurrent logins = %d", organization, response.StatusCode)
		}
	}
	other := newBrowser(t, f.service.Handler())
	stolen, _ := alice.get("/auth/oidc/start")
	if response, _ := other.get("/auth/oidc/callback?" + url.Values{"code": {"user_bob:"}, "state": {stateOf(t, stolen)}}.Encode()); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback without the initiating browser = %d", response.StatusCode)
	}
}

func stateOf(t *testing.T, response page) string {
	t.Helper()
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil || location.Query().Get("state") == "" {
		t.Fatalf("no provider redirect: %d %q", response.StatusCode, response.Header.Get("Location"))
	}
	return location.Query().Get("state")
}

func TestSharedEntryInvitation(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, page := alice.get("/organizations/org_alpha/organization")
	invite := alice.do(http.MethodPost, "/organizations/org_alpha/organization/invite", url.Values{"email": {"carol@example.test"}, "role": {"member"}, "csrf": {csrfFrom(t, page)}}, nil)
	if invite.StatusCode != http.StatusSeeOther {
		t.Fatalf("invite status = %d: %s", invite.StatusCode, invite.Body)
	}
	bob := newBrowser(t, f.service.Handler())
	start, _ := bob.get("/invite?invitation_token=inv_carol")
	if response, _ := bob.get("/auth/oidc/callback?" + url.Values{"code": {"user_bob:"}, "state": {stateOf(t, start)}}.Encode()); response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong recipient status = %d", response.StatusCode)
	}
	carol := newBrowser(t, f.service.Handler())
	start, _ = carol.get("/invite?invitation_token=inv_carol")
	accepted, _ := carol.get("/auth/oidc/callback?" + url.Values{"code": {"user_carol:"}, "state": {stateOf(t, start)}}.Encode())
	if accepted.StatusCode != http.StatusSeeOther || !strings.HasPrefix(accepted.Header.Get("Location"), "/auth/oidc/start?organization=org_alpha") {
		t.Fatalf("accepted invitation = %d %q", accepted.StatusCode, accepted.Header.Get("Location"))
	}
	carol.login(accepted.Header.Get("Location"), "user_carol:porg_alpha")
	response, body := carol.get("/organizations/org_alpha/organization")
	if response.StatusCode != http.StatusOK || strings.Contains(body, "Alpha secret project") {
		t.Fatalf("invited member page = %d (project content must need an explicit grant)", response.StatusCode)
	}
	if response, _ := carol.get("/organizations/org_beta/organization"); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("invited member reached another organization: %d", response.StatusCode)
	}
	if response, _ := bob.get("/invite?invitation_token=inv_unknown"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown invitation = %d", response.StatusCode)
	}
}

func TestRegistryRegister(t *testing.T) {
	t.Parallel()
	registry, err := OpenRegistry(t.Context(), filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	base := Organization{ID: "org_a", ProviderID: "porg_a", Name: "A", Endpoint: "unix:/run/a.sock", Generation: 1}
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
		{"same generation move", func(o *Organization) { o.Name = "A renamed"; o.Endpoint = "unix:/run/b.sock" }, false, true},
		{"generation move", func(o *Organization) { o.Name = "A renamed"; o.Endpoint = "unix:/run/b.sock"; o.Generation = 2 }, true, false},
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
	if _, err := registry.store.db.ExecContext(t.Context(), "DELETE FROM organizations"); err == nil {
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
		{"/admin", "", false},
	} {
		if got := validReturnPath(test.path, test.organization); got != test.ok {
			t.Errorf("validReturnPath(%q, %q) = %v, want %v", test.path, test.organization, got, test.ok)
		}
	}
}

func TestConfigRequiresLoopbackListener(t *testing.T) {
	t.Parallel()
	seed := make([]byte, ed25519.SeedSize)
	for _, test := range []struct {
		listen string
		ok     bool
	}{{"127.0.0.1:8017", true}, {"[::1]:8017", true}, {"localhost:8017", true}, {"0.0.0.0:8017", false}, {":8017", false}, {"10.0.0.5:8017", false}} {
		config := Config{PublicURL: "https://hub.example.test", ListenAddress: test.listen, Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(seed), Provider: newFakeProvider(), StateDir: "/state"}
		if err := config.validate(); (err == nil) != test.ok {
			t.Errorf("listen %q error = %v, want ok %v", test.listen, err, test.ok)
		}
	}
}

func TestSharedEntrySupportAccess(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	support := newBrowser(t, f.service.Handler())
	support.login("/organizations", "user_support:")
	if response, _ := support.get("/organizations/org_alpha/organization"); response.StatusCode == http.StatusOK {
		t.Fatal("staff session read customer content without support access")
	}
	response, page := support.get("/support")
	if response.StatusCode != http.StatusOK || !strings.Contains(page, `<option value="org_alpha">`) {
		t.Fatalf("support page = %d %s", response.StatusCode, page)
	}
	start := support.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "csrf": {csrfFrom(t, page)}}, nil)
	if start.StatusCode != http.StatusOK || !strings.Contains(start.Body, "impersonate") {
		t.Fatalf("support start = %d", start.StatusCode)
	}
	other := newBrowser(t, f.service.Handler())
	other.login("/organizations", "user_support:")
	if response, _ := other.get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request")); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("support callback in another browser = %d", response.StatusCode)
	}
	callback, _ := support.get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request"))
	if callback.StatusCode != http.StatusSeeOther || callback.Header.Get("Location") != "/organizations/org_alpha/organization" {
		t.Fatalf("support callback = %d %q %s", callback.StatusCode, callback.Header.Get("Location"), callback.Body)
	}
	response, body := support.get("/organizations/org_alpha/organization")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "support@example.test is acting as alice@example.test") || !strings.Contains(body, "Alpha secret project") {
		t.Fatalf("support page = %d %s", response.StatusCode, body)
	}
	if response, _ := support.get("/organizations/org_beta/organization"); response.StatusCode == http.StatusOK {
		t.Fatal("support access for alpha opened beta")
	}
	if replay, _ := support.get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request")); replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed support callback = %d", replay.StatusCode)
	}
	support.login("/auth/oidc/start", "user_support:")
	if response, _ := support.get("/organizations/org_alpha/organization"); response.StatusCode == http.StatusOK {
		t.Fatal("an ordinary sign-in carried support access into the new session")
	}
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations", "user_alice:")
	_, choices := alice.get("/support")
	if denied := alice.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "csrf": {csrfFrom(t, choices)}}, nil); denied.StatusCode != http.StatusForbidden {
		t.Fatalf("customer support start = %d", denied.StatusCode)
	}
	for _, test := range []struct{ name, code string }{
		{"invalid reason", "support|support@example.test|user_alice|porg_alpha|curiosity"},
		{"other organization", "support|support@example.test|user_alice|porg_beta|customer-request"},
		{"other actor", "support|staff@example.test|user_alice|porg_alpha|customer-request"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, page := support.get("/support")
			support.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "csrf": {csrfFrom(t, page)}}, nil)
			if response, _ := support.get("/auth/oidc/callback?code=" + url.QueryEscape(test.code)); response.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
}
