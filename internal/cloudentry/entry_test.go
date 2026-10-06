package cloudentry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/testenv"
)

const (
	testPublicURL = "http://127.0.0.1:18017"
	testAdminKey  = "detent_test_admin_token_0123456789abcdefghijklmnop"
)

type fakeProvider struct {
	organizations   map[string]auth.Organization
	createFailures  int
	creates         int
	mu              sync.Mutex
	users           map[string]string
	sessions        map[string]auth.HostedIdentity
	memberships     map[string]auth.Membership
	invitations     map[string]auth.Invitation
	revokeErr       error
	revoked         []string
	sequence        int
	authorizeBase   string
	verifications   int
	membershipLists int
	refreshes       int
	refreshDelay    time.Duration
	keysDown        bool
	access          map[string]fakeAccess
	refresh         map[string]string
}

type fakeAccess struct {
	access  auth.HostedAccess
	expires time.Time
}

var hubDatabasePath = testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
	return hubserver.Open(ctx, hubserver.Config{DatabasePath: path, GitHubDisabled: true, Logger: slog.New(slog.DiscardHandler)})
})

func newFakeProvider() *fakeProvider {
	return &fakeProvider{users: map[string]string{}, sessions: map[string]auth.HostedIdentity{}, memberships: map[string]auth.Membership{}, invitations: map[string]auth.Invitation{}, access: map[string]fakeAccess{}, refresh: map[string]string{}}
}

// providerCalls counts the provider API calls a request can make; local
// access-token verification is not one.
func (p *fakeProvider) providerCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifications + p.membershipLists + p.refreshes
}

// issueLocked mints an access and refresh token for a provider session, with
// the organization and role its current membership grants.
func (p *fakeProvider) issueLocked(session auth.HostedIdentity) auth.HostedTokens {
	p.sequence++
	suffix := session.SessionID + "_" + strconv.Itoa(p.sequence)
	access := auth.HostedAccess{Subject: session.Subject, SessionID: session.SessionID, SupportActor: session.SupportActor}
	if membership, ok := p.memberships["om_"+session.Subject+"_"+session.OrganizationID]; ok && session.OrganizationID != "" {
		access.OrganizationID, access.Role = session.OrganizationID, membership.Role.Slug
	}
	expires := time.Now().Add(5 * time.Minute)
	access.ExpiresAt = expires
	p.access["access_"+suffix] = fakeAccess{access: access, expires: expires}
	p.refresh["refresh_"+suffix] = session.SessionID
	return auth.HostedTokens{AccessToken: "access_" + suffix, RefreshToken: "refresh_" + suffix}
}

// expireAccess ends the lifetime of every issued access token, as the
// passage of one access-token lifetime would.
func (p *fakeProvider) expireAccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for token, issued := range p.access {
		issued.expires = time.Now().Add(-time.Second)
		p.access[token] = issued
	}
}

func (p *fakeProvider) VerifyAccess(_ context.Context, token string) (auth.HostedAccess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keysDown {
		return auth.HostedAccess{}, &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}
	}
	issued, ok := p.access[token]
	if !ok {
		return auth.HostedAccess{}, &auth.HostedIdentityError{Reason: auth.HostedReasonTokenInvalid}
	}
	if !issued.expires.After(time.Now()) {
		return auth.HostedAccess{}, auth.ErrAccessExpired
	}
	return issued.access, nil
}

func (p *fakeProvider) RefreshAccess(_ context.Context, token string) (auth.HostedAccess, auth.HostedTokens, error) {
	p.mu.Lock()
	p.refreshes++
	delay := p.refreshDelay
	p.mu.Unlock()
	time.Sleep(delay)
	p.mu.Lock()
	defer p.mu.Unlock()
	sessionID, ok := p.refresh[token]
	delete(p.refresh, token)
	session, active := p.sessions[sessionID]
	if !ok || !active {
		return auth.HostedAccess{}, auth.HostedTokens{}, &auth.HostedIdentityError{Reason: auth.HostedReasonProviderRejected, Status: http.StatusBadRequest}
	}
	tokens := p.issueLocked(session)
	return p.access[tokens.AccessToken].access, tokens, nil
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
		return auth.Identity{Subject: parts[2], Email: p.users[parts[2]], EmailVerified: true, Hosted: &hosted, Tokens: p.issueLocked(hosted)}, nil
	}
	if reason, ok := strings.CutPrefix(code, "deny:"); ok {
		return auth.Identity{}, &auth.HostedIdentityError{Reason: reason, TokenIssuer: "https://api.workos.com"}
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
	return auth.Identity{Subject: user, Email: email, EmailVerified: true, Hosted: &hosted, Tokens: p.issueLocked(hosted)}, nil
}

func (p *fakeProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifications++
	current, ok := p.sessions[identity.SessionID]
	if !ok || current.Subject != identity.Subject || !current.CreatedAt.Equal(identity.CreatedAt) {
		return auth.HostedIdentity{}, auth.ErrHostedIdentity
	}
	return current, nil
}

func (p *fakeProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.membershipLists++
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
	p.revoked = append(p.revoked, id)
	if p.revokeErr != nil {
		return p.revokeErr
	}
	delete(p.sessions, id)
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
	path := hubDatabasePath(t)
	hosted := func(shared bool) *hubserver.HostedConfig {
		plans := pilotPlans()
		plans.Plans[0].Allowances["projects"] = 10
		config := &hubserver.HostedConfig{Plans: plans, OrganizationID: id, WorkOSOrganizationID: providerID, BootstrapSubject: owner, PublicURL: map[string]string{"org_alpha": "http://127.0.0.1:19001", "org_beta": "http://127.0.0.1:19002"}[id], Provider: provider, StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}}
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
	return newEntryFixtureWithLogger(t, slog.New(slog.DiscardHandler))
}

func newEntryFixtureWithLogger(t *testing.T, logger *slog.Logger) entryFixture {
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
	tenants := map[string]http.Handler{testSocketEndpoint("alpha.sock"): alphaHandler, testSocketEndpoint("beta.sock"): betaHandler}
	service, err := Open(t.Context(), Config{
		PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: key, Provider: provider, StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"}, StateDir: t.TempDir(),
		Logger: logger, clientFS: fstest.MapFS{},
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
	}{{alpha, testSocketEndpoint("alpha.sock"), "Alpha"}, {beta, testSocketEndpoint("beta.sock"), "Beta"}} {
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	f.provider.expireAccess()
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

func TestSharedEntryLogoutRevokesLocalAndProviderSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name       string
		wantStatus int
	}{
		{name: "customer", wantStatus: http.StatusSeeOther},
		{name: "support", wantStatus: http.StatusSeeOther},
		{name: "expired local session", wantStatus: http.StatusSeeOther},
		{name: "provider failure", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newEntryFixture(t)
			client := newBrowser(t, f.service.Handler())
			if tt.name == "support" {
				client.login("/organizations", "user_support:")
				_, page := client.get("/support")
				client.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "reason": {"customer-request"}, "csrf": {csrfFrom(t, page)}}, nil)
				callback, _ := client.get("/auth/oidc/callback?code=" + url.QueryEscape("support|support@example.test|user_alice|porg_alpha|customer-request"))
				if callback.StatusCode != http.StatusSeeOther {
					t.Fatalf("support callback = %d %s", callback.StatusCode, callback.Body)
				}
			} else {
				client.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			}
			_, page := client.get("/organizations/org_alpha/organization")
			var hash string
			if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT token_hash FROM sessions WHERE revoked_at IS NULL").Scan(&hash); err != nil {
				t.Fatal(err)
			}
			session, err := f.service.auth.session(t.Context(), hash)
			if err != nil {
				t.Fatal(err)
			}
			authorization, err := f.service.auth.authorization(t.Context(), session, "org_alpha")
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "expired local session" {
				if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE sessions SET expires_at = ? WHERE token_hash = ?", formatTime(time.Now().Add(-time.Minute)), hash); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.auth.session(t.Context(), hash); !errors.Is(err, errNoSession) {
					t.Fatalf("expired session authenticated before logout: %v", err)
				}
			}
			if tt.name == "provider failure" {
				f.provider.revokeErr = errors.New("private provider credential")
			}
			response := client.do(http.MethodPost, "/organizations/org_alpha/logout?return=https%3A%2F%2Fattacker.example.test", url.Values{"csrf": {csrfFrom(t, page)}, "redirect": {"https://attacker.example.test"}}, nil)
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("logout status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
			wantLocation := "https://detent.build"
			if tt.wantStatus == http.StatusServiceUnavailable {
				wantLocation = ""
				if !strings.Contains(response.Body, "Provider sign-out could not be confirmed") {
					t.Fatal("provider failure did not retain the sign-out error page")
				}
			}
			if location := response.Header.Get("Location"); location != wantLocation {
				t.Fatalf("logout location = %q, want %q", location, wantLocation)
			}
			if strings.Contains(response.Body, "private provider credential") {
				t.Fatal("logout exposed provider error")
			}
			cleared := false
			for _, cookie := range (&http.Response{Header: response.Header}).Cookies() {
				if cookie.Name == f.service.cookieName("session") && cookie.Value == "" && cookie.MaxAge == -1 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatal("logout did not clear browser session")
			}
			var revoked bool
			if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT revoked_at IS NOT NULL FROM sessions WHERE token_hash = ?", hash).Scan(&revoked); err != nil || !revoked {
				t.Fatalf("local session not revoked: %v", err)
			}
			if _, err := f.service.auth.authorization(t.Context(), session, "org_alpha"); !errors.Is(err, errNoSession) {
				t.Fatalf("logged-out organization authorization remains active: %v", err)
			}
			for _, id := range []string{session.Identity.SessionID, authorization.Identity.SessionID} {
				found := false
				for _, revokedID := range f.provider.revoked {
					found = found || revokedID == id
				}
				if !found {
					t.Fatalf("provider session %q was not revoked: %v", id, f.provider.revoked)
				}
			}
		})
	}
}

func TestSharedEntryBoundaries(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	f := newEntryFixture(t)
	f.service.config.Build = buildinfo.Info{Version: "1.2.4", Commit: strings.Repeat("a", 40)}
	health := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"version":"1.2.4"`) || !strings.Contains(health.Body.String(), f.service.config.Build.Commit) {
		t.Fatalf("release health identity=%d %s", health.Code, health.Body)
	}

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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	for _, tt := range []struct{ name, query, organization, screenHint string }{
		{name: "sign in"},
		{name: "create account", query: "screen_hint=sign-up", screenHint: "sign-up"},
		{name: "organization create account", query: "organization=org_alpha&screen_hint=sign-up", organization: "porg_alpha", screenHint: "sign-up"},
		{name: "unknown hint", query: "screen_hint=unexpected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response, _ := alice.get("/auth/oidc/start?" + tt.query)
			location, err := url.Parse(response.Header.Get("Location"))
			if response.StatusCode != http.StatusSeeOther || err != nil {
				t.Fatalf("authorization redirect = %d %q: %v", response.StatusCode, response.Header.Get("Location"), err)
			}
			query := location.Query()
			if query.Get("organization_id") != tt.organization || query.Get("screen_hint") != tt.screenHint || query.Has("screen_hint") != (tt.screenHint != "") {
				t.Fatalf("authorization query = %v", query)
			}
			if query.Get("state") == "" || query.Get("verifier") == "" {
				t.Fatal("authorization lost state or PKCE")
			}
		})
	}

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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name, message           string
		entryCode, callbackCode int
	}{
		{name: "new user sign-up", entryCode: 303, callbackCode: 303},
		{name: "existing user sign-in", entryCode: 303, callbackCode: 303},
		{name: "legacy token alias", entryCode: 303, callbackCode: 303},
		{name: "wrong account", entryCode: 303, callbackCode: 403, message: "different account at example.test"},
		{name: "expired", entryCode: 403, message: "has expired"},
		{name: "used", entryCode: 403, message: "already been used"},
		{name: "provider rejected expired invitation", entryCode: 303, callbackCode: 403, message: "has expired"},
		{name: "expired during login", entryCode: 303, callbackCode: 403, message: "has expired"},
		{name: "unknown", entryCode: 403, message: "unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newEntryFixture(t)
			alice := newBrowser(t, f.service.Handler())
			alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			_, page := alice.get("/organizations/org_alpha/organization")
			invite := alice.do(http.MethodPost, "/organizations/org_alpha/organization/invite", url.Values{"email": {"carol@example.test"}, "role": {"member"}, "csrf": {csrfFrom(t, page)}}, nil)
			if invite.StatusCode != http.StatusSeeOther {
				t.Fatalf("invite = %d: %s", invite.StatusCode, invite.Body)
			}
			invitation := f.provider.invitations["inv_carol"]
			switch tt.name {
			case "new user sign-up":
				delete(f.provider.users, "user_carol")
			case "expired":
				invitation.ExpiresAt = time.Now().Add(-time.Hour)
			case "used":
				invitation.State, invitation.AcceptedUserID = "accepted", "user_carol"
			}
			f.provider.invitations["inv_carol"] = invitation
			browser := newBrowser(t, f.service.Handler())
			path := "/invite?invitation_token=inv_carol"
			if tt.name == "legacy token alias" {
				path = "/invite?token=inv_carol"
			}
			if tt.name == "unknown" {
				path = "/invite?invitation_token=inv_unknown"
			}
			start, body := browser.get(path)
			if start.StatusCode != tt.entryCode {
				t.Fatalf("entry = %d, want %d: %s", start.StatusCode, tt.entryCode, body)
			}
			assertExplanation := func(body string) {
				t.Helper()
				for _, want := range []string{tt.message, "Ask the person who invited you for a new invitation", `href="https://detent.build"`} {
					if !strings.Contains(body, want) {
						t.Errorf("explanation missing %q", want)
					}
				}
				if strings.Contains(body, "carol@example.test") || strings.Contains(body, "inv_carol") {
					t.Fatal("explanation leaked recipient or token")
				}
			}
			if start.StatusCode != http.StatusSeeOther {
				assertExplanation(body)
				return
			}
			authorization, _ := url.Parse(start.Header.Get("Location"))
			wantHint := ""
			if tt.name == "new user sign-up" {
				wantHint = "sign-up"
				f.provider.users["user_carol"] = "carol@example.test"
			}
			if authorization.Query().Get("invitation_token") != "inv_carol" || authorization.Query().Get("screen_hint") != wantHint {
				t.Fatalf("AuthKit invitation URL = %s", authorization)
			}
			if tt.name == "expired during login" || tt.name == "provider rejected expired invitation" {
				invitation.ExpiresAt = time.Now().Add(-time.Hour)
				f.provider.invitations["inv_carol"] = invitation
			}
			code := "user_carol:"
			if tt.name == "wrong account" {
				code = "user_bob:"
			}
			query := url.Values{"code": {code}, "state": {stateOf(t, start)}, "invitation_token": {"attacker_token"}}
			if tt.name == "provider rejected expired invitation" {
				query.Set("error", "access_denied")
			}
			accepted, body := browser.get("/auth/oidc/callback?" + query.Encode())
			if accepted.StatusCode != tt.callbackCode {
				t.Fatalf("callback = %d, want %d: %s", accepted.StatusCode, tt.callbackCode, body)
			}
			if accepted.StatusCode != http.StatusSeeOther {
				assertExplanation(body)
				memberships, _ := f.provider.Memberships(t.Context(), "user_carol", "porg_alpha")
				if len(memberships) != 0 {
					t.Fatal("failed invitation granted membership")
				}
				return
			}
			if !strings.HasPrefix(accepted.Header.Get("Location"), "/auth/oidc/start?organization=org_alpha") {
				t.Fatalf("next = %s", accepted.Header.Get("Location"))
			}
			browser.login(accepted.Header.Get("Location"), "user_carol:porg_alpha")
			response, body := browser.get("/organizations/org_alpha/organization")
			if response.StatusCode != http.StatusOK || strings.Contains(body, "Alpha secret project") {
				t.Fatalf("member page = %d (project access must require explicit grant)", response.StatusCode)
			}
			if response, _ := browser.get("/organizations/org_beta/organization"); response.StatusCode != http.StatusSeeOther {
				t.Fatalf("member reached other organization = %d", response.StatusCode)
			}
			if response, body := browser.get(path); response.StatusCode != http.StatusForbidden || !strings.Contains(body, "already been used") {
				t.Fatalf("used link = %d: %s", response.StatusCode, body)
			}
		})
	}
}

func TestRegistryRegister(t *testing.T) {
	t.Parallel()
	registry, err := OpenRegistry(t.Context(), filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	base := Organization{ID: "org_a", ProviderID: "porg_a", Name: "A", Endpoint: testSocketEndpoint("a.sock"), Generation: 1}
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
		{"same generation move", func(o *Organization) { o.Name = "A renamed"; o.Endpoint = testSocketEndpoint("b.sock") }, false, true},
		{"generation move", func(o *Organization) {
			o.Name = "A renamed"
			o.Endpoint = testSocketEndpoint("b.sock")
			o.Generation = 2
		}, true, false},
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	start := support.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "reason": {"customer-request"}, "csrf": {csrfFrom(t, page)}}, nil)
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
		{"reason differs from the started request", "support|support@example.test|user_alice|porg_alpha|troubleshooting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, page := support.get("/support")
			support.do(http.MethodPost, "/support/start", url.Values{"organization": {"org_alpha"}, "reason": {"customer-request"}, "csrf": {csrfFrom(t, page)}}, nil)
			if response, _ := support.get("/auth/oidc/callback?code=" + url.QueryEscape(test.code)); response.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
}

func TestStoreDSN(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, tt := range []struct{ path, want, escaped string }{
		{"/tmp/registry.db", "/tmp/registry.db", "/tmp/registry.db"},
		{"/tmp/cloud entry/café #?%.db", "/tmp/cloud entry/café #?%.db", "/tmp/cloud%20entry/caf%C3%A9%20%23%3F%25.db"},
		{"C:/entry/registry.db", "/C:/entry/registry.db", "/C:/entry/registry.db"},
		{"c:/entry/a #?.db", "/c:/entry/a #?.db", "/c:/entry/a%20%23%3F.db"},
		{"C:/cloud entry/数据库 %23.db", "/C:/cloud entry/数据库 %23.db", "/C:/cloud%20entry/%E6%95%B0%E6%8D%AE%E5%BA%93%20%2523.db"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			if tt.want != tt.path {
				// The former URL construction makes the drive letter an authority.
				// SQLite rejects it at the first query, before touching the file.
				legacy := &url.URL{Scheme: "file", Path: tt.path}
				db, err := sql.Open("sqlite", legacy.String())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				})
				var applicationID int64
				if err := db.QueryRowContext(t.Context(), "PRAGMA application_id").Scan(&applicationID); err == nil || !strings.Contains(err.Error(), "invalid uri authority: "+tt.path[:2]) {
					t.Fatalf("legacy store query = %v; want invalid drive authority", err)
				}
			}
			parsed, err := url.Parse(storeDSN(tt.path))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Scheme != "file" || parsed.Host != "" || parsed.Path != tt.want || parsed.Fragment != "" || parsed.EscapedPath() != tt.escaped {
				t.Fatalf("store URI = %v; want local path %q escaped as %q", parsed, tt.want, tt.escaped)
			}
			wantPragmas := []string{"busy_timeout(5000)", "foreign_keys(1)", "locking_mode(EXCLUSIVE)", "synchronous(FULL)"}
			if len(parsed.Query()) != 1 || !slices.Equal(parsed.Query()["_pragma"], wantPragmas) {
				t.Fatalf("pragmas = %v", parsed.Query())
			}
		})
	}
}

func testSocketEndpoint(name string) string {
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	return "unix:" + filepath.Join(root, "tenants", name)
}

func pilotPlans() *hubserver.HostedPlansConfig {
	free := map[string]int64{
		"members": 10, "projects": 1, "repositories": 10, "registered_runners": 4, "connected_runners": 4, "concurrent_work": 2,
		"api_mutations": 10000, "ingested_events": 10000, "collaboration_bytes": 64 << 20, "history_records": 10000,
	}
	plus := make(map[string]int64, len(free))
	for name, limit := range free {
		plus[name] = limit
	}
	plus["projects"] = 5
	features := []string{"collaboration", "native_execution"}
	return &hubserver.HostedPlansConfig{
		Base: hubserver.PlanReference{ID: "pilot_free", Version: 1}, WindowSeconds: 3600, RetentionWindows: 24, ConnectedSeconds: 90, InvitationSeconds: 86400,
		Plans: []hubserver.HostedPlan{
			{PlanReference: hubserver.PlanReference{ID: "pilot_free", Version: 1}, Features: features, Allowances: free},
			{PlanReference: hubserver.PlanReference{ID: "pilot_plus", Version: 1}, Features: features, Allowances: plus},
		},
	}
}

func (p *fakeProvider) HasUser(_ context.Context, email string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, existing := range p.users {
		if strings.EqualFold(existing, email) {
			return true, nil
		}
	}
	return false, nil
}
