package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type browserHostedProvider struct {
	mu                 sync.Mutex
	base               string
	organization       auth.Organization
	members            map[string]auth.Membership
	sessions           map[string]auth.HostedIdentity
	invitations        map[string]auth.Invitation
	inviteRoles        map[string]string
	authorizations     map[string]string
	codes              map[string]auth.Identity
	emails             map[string]bool
	users              map[string]auth.HostedUser
	sequence           int
	invitationDelivery func(context.Context, string, string) error
}

func (p *browserHostedProvider) RevokeInvitation(ctx context.Context, id string) error {
	return p.deliverInvitation(ctx, id, "revoke")
}

func (p *browserHostedProvider) ResendInvitation(ctx context.Context, id string) error {
	return p.deliverInvitation(ctx, id, "resend")
}

func (p *browserHostedProvider) deliverInvitation(ctx context.Context, id, action string) error {
	if p.invitationDelivery != nil {
		if err := p.invitationDelivery(ctx, id, action); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[id]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	if action == "revoke" {
		invitation.State = "revoked"
		p.invitations[id] = invitation
	}
	return nil
}

func (p *browserHostedProvider) AuthorizationURL(state, _ string, verifier string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorizations[state] = verifier
	return p.base + "/__preview/authorize?state=" + url.QueryEscape(state)
}

func (p *browserHostedProvider) Exchange(_ context.Context, code, verifier, nonce string) (auth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	identity, ok := p.codes[code]
	if !ok || verifier == "" || p.authorizations[nonce] != verifier {
		return auth.Identity{}, auth.ErrHostedIdentity
	}
	delete(p.codes, code)
	delete(p.authorizations, nonce)
	return identity, nil
}

func (p *browserHostedProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.sessions[identity.SessionID]
	if !ok || !current.ExpiresAt.After(time.Now()) {
		return auth.HostedIdentity{}, auth.ErrHostedIdentity
	}
	return current, nil
}

func (p *browserHostedProvider) User(_ context.Context, id string) (auth.HostedUser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	user, ok := p.users[id]
	if !ok {
		return auth.HostedUser{}, auth.ErrHostedIdentity
	}
	return user, nil
}

func (p *browserHostedProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var members []auth.Membership
	for _, member := range p.members {
		if (user == "" || member.UserID == user) && (organization == "" || member.OrganizationID == organization) {
			members = append(members, member)
		}
	}
	return members, nil
}

func (p *browserHostedProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id != p.organization.ID {
		return auth.Organization{}, auth.ErrHostedIdentity
	}
	return p.organization, nil
}

func (p *browserHostedProvider) CreateOrganization(_ context.Context, externalID, name string) (auth.Organization, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.organization = auth.Organization{ID: "org_browser_provider", ExternalID: externalID, Name: name}
	return p.organization, nil
}

func (p *browserHostedProvider) CreateMembership(_ context.Context, user, organization, role string) (auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: organization, Status: "active"}
	member.Role.Slug = role
	p.members[member.ID] = member
	return member, nil
}

func (p *browserHostedProvider) SetMembershipRole(_ context.Context, id, role string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	member, ok := p.members[id]
	if !ok {
		return auth.ErrHostedIdentity
	}
	member.Role.Slug = role
	p.members[id] = member
	return nil
}

func (p *browserHostedProvider) RevokeMembership(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.members, id)
	return nil
}

func (p *browserHostedProvider) Invite(_ context.Context, organization, email, role, _ string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	invitation := auth.Invitation{ID: fmt.Sprintf("invitation_browser_%d", p.sequence), Email: email, OrganizationID: organization, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
	p.invitations[invitation.ID], p.inviteRoles[invitation.ID] = invitation, role
	return invitation, nil
}

func (p *browserHostedProvider) Invitation(_ context.Context, token string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok {
		return auth.Invitation{}, auth.ErrHostedIdentity
	}
	return invitation, nil
}

func (p *browserHostedProvider) AcceptInvitation(_ context.Context, token, user string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	invitation.State, invitation.AcceptedUserID = "accepted", user
	p.invitations[token] = invitation
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: invitation.OrganizationID, Status: "active"}
	member.Role.Slug = p.inviteRoles[token]
	p.members[member.ID] = member
	return nil
}

func (p *browserHostedProvider) InvitationByID(ctx context.Context, id string) (auth.Invitation, error) {
	return p.Invitation(ctx, id)
}

func (p *browserHostedProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	return p.AcceptInvitation(ctx, id, user)
}

func (p *browserHostedProvider) RevokeSession(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
	return nil
}

func (p *browserHostedProvider) identity(user, email, organization, support string) auth.Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	if p.users == nil {
		p.users = map[string]auth.HostedUser{}
	}
	p.users[user] = auth.HostedUser{ID: user, Email: email}
	now := time.Now().UTC()
	hosted := auth.HostedIdentity{Subject: user, OrganizationID: organization, SessionID: fmt.Sprintf("session_browser_%d", p.sequence), CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(browserHostedPreviewLifetime), SupportActor: support}
	if support != "" {
		hosted.SupportReason = "customer-request"
	}
	p.sessions[hosted.SessionID] = hosted
	return auth.Identity{Subject: user, Email: email, EmailVerified: true, Hosted: &hosted}
}

type browserHostedFixture struct {
	service        *Service
	server         *httptest.Server
	provider       *browserHostedProvider
	cookies        map[string]*http.Cookie
	project        string
	privateProject string
	conversation   string
	workItem       string
	workerChat     string
	workerItem     string
	stop           chan struct{}
	stopOnce       sync.Once
}

// browserHostedPreviewLifetime bounds a preview the browser suite never
// stopped. It outlasts the browser job so a long serial spec ends through
// POST /__preview/stop, not this timer.
const browserHostedPreviewLifetime = 20 * time.Minute

func newBrowserHostedFixture(t *testing.T, allocated bool) *browserHostedFixture {
	t.Helper()
	return newBrowserHostedOrganizationFixture(t, allocated, "org_browser_preview")
}

func newBrowserHostedOrganizationFixture(t *testing.T, allocated bool, organization string, configure ...func(*Config)) *browserHostedFixture {
	t.Helper()
	return newBrowserHostedFixtureServing(t, allocated, organization, true, configure...)
}

func newBrowserHostedFixtureServing(t *testing.T, allocated bool, organization string, listen bool, configure ...func(*Config)) *browserHostedFixture {
	t.Helper()
	server := &httptest.Server{URL: "https://browser.example.test"}
	base := server.URL
	if listen {
		server = httptest.NewUnstartedServer(http.NotFoundHandler())
		base = "http://" + server.Listener.Addr().String()
	}
	provider := &browserHostedProvider{
		base: base, organization: auth.Organization{ID: "org_browser_provider", ExternalID: organization, Name: "Browser organization"},
		members: make(map[string]auth.Membership), sessions: make(map[string]auth.HostedIdentity), invitations: make(map[string]auth.Invitation), inviteRoles: make(map[string]string), authorizations: make(map[string]string), codes: make(map[string]auth.Identity),
	}
	legacyPlans := pilotHostedPlans()
	cfg := Config{InitialAdminToken: []byte(testHubAdminToken), DatabasePath: filepath.Join(t.TempDir(), "hosted-browser.db"), GitHubDisabled: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Hosted: &HostedConfig{
		Plans: &legacyPlans, OrganizationID: organization, BootstrapSubject: "user_browser_owner", PublicURL: base, Provider: provider,
		StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"},
		Directory: []HostedDestination{{OrganizationID: organization, WorkOSOrganizationID: "org_browser_provider", PublicURL: base}},
	}}
	if allocated {
		cfg.Hosted.WorkOSOrganizationID = provider.organization.ID
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	seedHubDatabaseTemplate(t, cfg.DatabasePath)
	service, err := Open(t.Context(), cfg)
	if err != nil {
		if listen {
			server.Close()
		}
		t.Fatal(err)
	}
	fixture := &browserHostedFixture{service: service, server: server, provider: provider, cookies: make(map[string]*http.Cookie), stop: make(chan struct{})}
	accounts := make(map[string]auth.Identity)
	t.Cleanup(func() {
		if listen {
			fixture.server.CloseClientConnections()
			fixture.server.Close()
		}
		if err := fixture.service.CloseContext(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	for _, account := range []struct {
		name, user, email, role, support string
	}{
		{name: "owner", user: "user_browser_owner", email: "owner@example.test", role: "owner"},
		{name: "viewer", user: "user_browser_viewer", email: "viewer@example.test", role: "viewer"},
		{name: "staff", user: "user_browser_staff", email: "staff@example.test"},
		{name: "support-staff", user: "user_browser_support", email: "support@example.test"},
		{name: "support-viewer", user: "user_browser_viewer", email: "viewer@example.test", support: "support@example.test"},
		{name: "invitee", user: "user_browser_invitee", email: "invitee@example.test"},
		{name: "wrong-organization", user: "user_browser_owner", email: "owner@example.test"},
		{name: "revoked", user: "user_browser_viewer", email: "viewer@example.test"},
		{name: "expired", user: "user_browser_viewer", email: "viewer@example.test"},
	} {
		organization := ""
		if allocated && (account.role != "" || account.support != "" || account.name == "revoked" || account.name == "expired") {
			organization = provider.organization.ID
		}
		if account.name == "wrong-organization" {
			organization = "org_browser_other"
		}
		identity := provider.identity(account.user, account.email, organization, account.support)
		accounts[account.name] = identity
		if provider.emails == nil {
			provider.emails = map[string]bool{}
		}
		if account.name != "invitee" {
			provider.emails[account.email] = true
		}
		if allocated && account.role != "" {
			membership, err := provider.CreateMembership(t.Context(), account.user, organization, account.role)
			if err != nil {
				t.Fatal(err)
			}
			if account.name == "owner" {
				err = service.bootstrapHostedMember(t.Context(), identity)
			} else {
				err = service.addHostedMember(t.Context(), identity, membership)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		token, session, err := service.hostedSessions.CreateIdentitySession(t.Context(), identity)
		if err != nil {
			t.Fatal(err)
		}
		fixture.cookies[account.name] = &http.Cookie{Name: hostedCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt}
		if account.name == "revoked" {
			if err := provider.RevokeSession(t.Context(), identity.Hosted.SessionID); err != nil {
				t.Fatal(err)
			}
		}
		if account.name == "expired" {
			provider.mu.Lock()
			identity.Hosted.ExpiresAt = time.Now().Add(-time.Second)
			provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
			provider.mu.Unlock()
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__preview/account/{account}", func(w http.ResponseWriter, r *http.Request) {
		cookie, ok := fixture.cookies[r.PathValue("account")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, cookie)
		http.SetCookie(w, &http.Cookie{Name: "detent_preview_account", Value: r.PathValue("account"), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/organization", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /__preview/authorize", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		provider.mu.Lock()
		organization := provider.organization.ID
		provider.mu.Unlock()
		account := "owner"
		if cookie, err := r.Cookie("detent_preview_account"); err == nil {
			account = cookie.Value
		}
		selected, ok := accounts[account]
		if !ok || selected.Hosted == nil {
			http.NotFound(w, r)
			return
		}
		identity := provider.identity(selected.Subject, selected.Email, organization, selected.Hosted.SupportActor)
		provider.mu.Lock()
		code := "code_" + identity.Hosted.SessionID
		provider.codes[code] = identity
		provider.mu.Unlock()
		http.Redirect(w, r, "/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusSeeOther)
	})
	mux.HandleFunc("POST /__preview/stop", func(w http.ResponseWriter, _ *http.Request) {
		fixture.stopOnce.Do(func() { close(fixture.stop) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /__preview/invitations", func(w http.ResponseWriter, _ *http.Request) {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(provider.invitations); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("POST /__preview/invite/{state}", func(w http.ResponseWriter, r *http.Request) {
		state := r.PathValue("state")
		invited := accounts["invitee"]
		invitation, err := provider.Invite(r.Context(), provider.organization.ID, invited.Email, "member", accounts["owner"].Subject)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		provider.mu.Lock()
		provider.emails[invited.Email] = state != "new"
		switch state {
		case "expired":
			invitation.ExpiresAt = time.Now().Add(-time.Hour)
		case "used":
			invitation.State, invitation.AcceptedUserID = "accepted", invited.Subject
		}
		provider.invitations[invitation.ID] = invitation
		provider.mu.Unlock()
		if _, err := service.database.db.ExecContext(r.Context(), "INSERT INTO hosted_invitations(id,email,organization_id,role,created_at) VALUES (?,?,?,?,?)", invitation.ID, invited.Email, service.config.Hosted.OrganizationID, "member", formatHubTime(time.Now())); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"url": base + "/invite?invitation_token=" + invitation.ID}); err != nil {
			t.Error(err)
		}
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.service.Handler().ServeHTTP(w, r)
	}))
	if listen {
		server.Config.Handler = mux
		server.Start()
	}
	if allocated {
		fixture.project = fixture.createProject(t, "Browser collaboration")
		fixture.privateProject = fixture.createProject(t, "Owner private project")
		response := fixture.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_viewer"}, "project": {fixture.project}})
		browserHostedStatus(t, response, http.StatusSeeOther)
		payload, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "browser-preview-issue"}, Title: "Review the invitation flow", Body: "Browser fixture private body", State: "Todo"})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, base+"/api/v2/organizations/"+organization+"/projects/"+fixture.project+"/work-items", strings.NewReader(string(payload)))
		request.Header.Set("Content-Type", "application/json")
		cookie := fixture.cookies["owner"]
		if cookie == nil {
			t.Fatal("owner session cookie is missing")
		}
		request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
		request.AddCookie(cookie)
		response = httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		browserHostedStatus(t, response, http.StatusOK)
	}
	return fixture
}

func (f *browserHostedFixture) form(t *testing.T, account, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	cookie := f.cookies[account]
	if cookie == nil {
		t.Fatal("account session cookie is missing")
	}
	values.Set("csrf", hostedCSRF(cookie.Value))
	request := httptest.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", f.server.URL)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func (f *browserHostedFixture) page(t *testing.T, account, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, f.server.URL+path, nil)
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func (f *browserHostedFixture) createProject(t *testing.T, name string) string {
	t.Helper()
	response := f.form(t, "owner", "/projects", url.Values{"name": {name}, "grant_access": {"true"}})
	browserHostedStatus(t, response, http.StatusSeeOther)
	project := strings.TrimPrefix(response.Header().Get("Location"), "/projects/")
	if !strings.HasPrefix(project, "prj_") {
		t.Fatal("project form returned an invalid project location")
	}
	return project
}

func browserHostedStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
}

func TestHostedBrowserHTTPPages(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	tests := []struct {
		name, account, path, contains, excludes string
		status                                  int
	}{
		{name: "login", path: "/login", status: http.StatusOK, contains: "<div id=\"root\"></div>"},
		{name: "owner organization", account: "owner", path: "/organization", status: http.StatusOK, contains: "Members and invitations"},
		{name: "viewer organization", account: "viewer", path: "/organization", status: http.StatusOK, contains: "Browser collaboration", excludes: "Owner private project"},
		{name: "ordinary staff", account: "staff", path: "/organization", status: http.StatusOK, contains: "Staff access is limited", excludes: "Browser collaboration"},
		{name: "support viewer", account: "support-viewer", path: "/organization", status: http.StatusOK, contains: "Exit support session"},
		{name: "wrong organization", account: "wrong-organization", path: "/organization", status: http.StatusOK, contains: "Create or join an organization", excludes: "Browser collaboration"},
		{name: "support entry", account: "support-staff", path: "/support", status: http.StatusOK, contains: "Start support sign-in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := f.page(t, tt.account, tt.path)
			browserHostedStatus(t, response, tt.status)
			html := response.Body.String()
			if tt.contains != "" && !strings.Contains(html, tt.contains) {
				t.Errorf("page missing %q", tt.contains)
			}
			if tt.excludes != "" && strings.Contains(html, tt.excludes) {
				t.Errorf("page contains forbidden %q", tt.excludes)
			}
			for _, forbidden := range []string{`sse-connect`, `hx-get`, `/api/v1/`, `chat-panel`} {
				if strings.Contains(html, forbidden) {
					t.Errorf("hosted response includes %q", forbidden)
				}
			}
			wantCache := "no-store"
			if tt.name == "login" {
				wantCache = "no-cache"
			}
			if response.Header().Get("Cache-Control") != wantCache {
				t.Error("hosted page is cacheable")
			}
		})
	}
}

func TestHostedBrowserHTTPForms(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	tests := []struct {
		name, account, path string
		values              url.Values
		status              int
	}{
		{name: "project requires explicit access", account: "owner", path: "/projects", values: url.Values{"name": {"Unapproved project"}}, status: http.StatusUnprocessableEntity},
		{name: "viewer cannot create", account: "viewer", path: "/projects", values: url.Values{"name": {"Viewer project"}, "grant_access": {"true"}}, status: http.StatusForbidden},
		{name: "owner creates project", account: "owner", path: "/projects", values: url.Values{"name": {"Form project"}, "grant_access": {"true"}}, status: http.StatusSeeOther},
		{name: "owner invites member", account: "owner", path: "/organization/invite", values: url.Values{"email": {"invitee@example.test"}, "role": {"viewer"}}, status: http.StatusSeeOther},
		{name: "staff cannot invite", account: "staff", path: "/organization/invite", values: url.Values{"email": {"other@example.test"}, "role": {"member"}}, status: http.StatusForbidden},
		{name: "wrong organization cannot switch", account: "viewer", path: "/organization/switch", values: url.Values{"organization": {"org_unknown"}}, status: http.StatusForbidden},
		{name: "authorized support starts", account: "support-staff", path: "/support/start", values: url.Values{}, status: http.StatusOK},
		{name: "ordinary staff cannot impersonate", account: "staff", path: "/support/start", values: url.Values{}, status: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := f.form(t, tt.account, tt.path, tt.values)
			browserHostedStatus(t, response, tt.status)
		})
	}
}

func TestHostedBrowserFirstOrganization(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, false)
	response := f.page(t, "owner", "/organization")
	browserHostedStatus(t, response, http.StatusOK)
	for _, expected := range []string{`action="/organization/create"`, `Ask an owner to send you an invitation.`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("onboarding missing %q", expected)
		}
	}
	response = f.form(t, "owner", "/organization/create", url.Values{"name": {"New browser organization"}})
	browserHostedStatus(t, response, http.StatusSeeOther)
	var organization, role string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT provider_id FROM hosted_tenant").Scan(&organization); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT role FROM hosted_members WHERE user_id = 'user_browser_owner'").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if organization != "org_browser_provider" || role != "owner" || response.Header().Get("Location") != "/auth/oidc/start" {
		t.Fatalf("organization creation = organization %q, role %q, location %q", organization, role, response.Header().Get("Location"))
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(base, []*http.Cookie{f.cookies["owner"]})
	client := f.server.Client()
	client.Jar = jar
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/auth/oidc/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(result.Body)
	closeErr := result.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read sign-in response: %v; close: %v", readErr, closeErr)
	}
	if result.StatusCode != http.StatusOK || result.Request.URL.Path != "/organization" || !strings.Contains(string(body), "New browser organization") || !strings.Contains(string(body), `action="/projects"`) {
		t.Fatalf("first sign-in after organization creation failed: status %d, path %s, body %s", result.StatusCode, result.Request.URL.Path, body)
	}
	for _, cookie := range jar.Cookies(base) {
		if cookie.Name == hostedCookie {
			f.cookies["owner"] = cookie
		}
	}
	project := f.createProject(t, "First browser project")
	response = f.page(t, "owner", "/api/v2/organizations/org_browser_preview/projects")
	browserHostedStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), `"id":"`+project+`"`) {
		t.Error("new project is not listed for its creator")
	}
}

func TestHostedBrowserProviderAccountSelection(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	base, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"owner", "viewer"} {
		t.Run(account, func(t *testing.T) {
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := f.server.Client()
			client.Jar = jar
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/__preview/account/"+account, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(base, []*http.Cookie{{Name: hostedCookie, Value: "invalid-preview-session", Path: "/"}})
			request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/auth/oidc/start", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err = client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("sign-in status = %d", response.StatusCode)
			}
			for _, cookie := range jar.Cookies(base) {
				if cookie.Name != hostedCookie {
					continue
				}
				session, err := f.service.hostedSessions.Authenticate(t.Context(), cookie.Value)
				if err != nil || session.Identity == nil || session.Identity.Subject != "user_browser_"+account {
					t.Fatalf("provider selected another account: %+v, %v", session, err)
				}
				return
			}
			t.Fatal("sign-in cookie missing")
		})
	}
}

const browserHostedOwnerEmail = "owner@example.test"

func browserPreviewConfig(cfg *Config) {
	cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Model: genkitbackend.Model, ReasoningEffort: "low"}
	cfg.Usage = &UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{
		"gpt-6-astra":   {Input: 1.25, CachedInput: 0.125, Output: 10},
		"claude-opus-5": {Input: 5, CachedInput: 0.5, Output: 25},
	}}
}

func (f *browserHostedFixture) seedPreview(t *testing.T) {
	t.Helper()
	if os.Getenv("DETENT_HOSTED_BROWSER_UNSIGNED_MEMBER") != "" {
		user := auth.HostedUser{ID: "user_browser_unsigned", Email: "unsigned@example.test", Name: "Unsigned Member"}
		f.provider.users[user.ID] = user
		if _, err := f.provider.CreateMembership(t.Context(), user.ID, f.provider.organization.ID, "member"); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []string{f.project, f.privateProject} {
		f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/members/membership_user_browser_owner/grants", map[string]any{
			"idempotency_key": "preview-owner-runner-" + project, "project_id": project, "write": true, "runner": true,
		}, http.StatusOK)
	}
	f.seedConversation(t)
}

func (f *browserHostedFixture) seedConversation(t *testing.T) {
	t.Helper()
	base := browserHostedOrganizationBase + "/projects/" + f.project
	var created struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/conversations", map[string]any{
		"key":           "browser-preview-conversation",
		"title":         "Lease renewal under load",
		"first_message": map[string]any{"key": "browser-preview-message", "text": "Why does the lease lapse under load?"},
	}, http.StatusCreated), &created)
	f.conversation = created.Conversation.ID
	if f.conversation == "" {
		t.Fatal("created conversation has no id")
	}
	var linked struct {
		Issue struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/conversations/"+f.conversation+"/link", map[string]any{
		"key":           "browser-preview-link",
		"share_history": true,
		"issue": map[string]any{
			"title":       "Renew the lease before the handoff completes",
			"description": "Move the lease renewal behind the handoff acknowledgement.",
		},
	}, http.StatusOK), &linked)
	f.workItem = linked.Issue.ID
	if f.workItem == "" {
		t.Fatal("linked issue has no work item id")
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_CHAT_ORIGIN") == "" {
		return
	}
	var issue tracker.NativeIssue
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "browser-worker-issue"}, Title: "Runner session isolation", State: "Todo",
	}, http.StatusOK), &issue)
	store := f.service.conversations.store
	owner, err := store.readConversationByID(t.Context(), f.service.database.db, f.conversation)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	worker, err := f.service.conversations.ensureWorkerConversation(t.Context(), tx, nativeScope{organization: owner.OrganizationID, project: owner.ProjectID, credential: apiCredential{ID: owner.OwnerPrincipalID}}, string(issue.WorkItemID), f.service.config.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f.workerChat, f.workerItem = worker.ID, worker.WorkItemID
}

func (f *browserHostedFixture) sessionRefreshHandler(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	var mu sync.Mutex
	refreshed := make(map[string]*http.Cookie)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(hostedCookie)
		if r.Method == http.MethodPost && r.URL.Path == "/__preview/session/expire" {
			if err != nil {
				http.Error(w, "session cookie required", http.StatusUnauthorized)
				return
			}
			session, err := f.service.hostedSessions.Authenticate(r.Context(), cookie.Value)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			identity := auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}
			token, fresh, err := f.service.hostedSessions.CreateIdentitySession(r.Context(), identity)
			if err != nil {
				t.Error(err)
				http.Error(w, "session refresh failed", http.StatusInternalServerError)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if _, err := f.service.database.db.ExecContext(r.Context(), "UPDATE hosted_sessions SET expires_at = ? WHERE token_hash = ?", formatHubTime(time.Now().Add(-time.Second)), apikey.HashToken(cookie.Value)); err != nil {
				t.Error(err)
				http.Error(w, "session expiry failed", http.StatusInternalServerError)
				return
			}
			refreshed[cookie.Value] = &http.Cookie{Name: hostedCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: fresh.ExpiresAt}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err == nil {
			mu.Lock()
			fresh := refreshed[cookie.Value]
			mu.Unlock()
			if fresh != nil {
				cookies := r.Cookies()
				r.Header.Del("Cookie")
				for _, current := range cookies {
					if current.Name == hostedCookie {
						current = fresh
					}
					r.AddCookie(current)
				}
				http.SetCookie(w, fresh)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func TestHostedBrowserPreviewSeed(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", browserPreviewConfig)

	f.seedPreview(t)
	if os.Getenv("DETENT_HOSTED_BROWSER_ISSUE_ASK") != "" {
		f.seedIssueAsk(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_CHAT_ACTIONS") != "" {
		f.seedCoordinatorActions(t)
	}
	base := browserHostedOrganizationBase + "/projects/" + f.project
	tests := []struct {
		name, account, path string
		status              int
		contains            string
	}{
		{name: "owner reads the conversation", account: "owner", path: base + "/conversations/" + f.conversation, status: http.StatusOK, contains: f.workItem},
		{name: "owner lists project conversations", account: "owner", path: base + "/conversations", status: http.StatusOK, contains: f.conversation},
		{name: "viewer reads the linked work item", account: "viewer", path: base + "/work-items/" + f.workItem, status: http.StatusOK, contains: "Renew the lease"},
		{name: "owner reads runner updates", account: "owner", path: "/app/updates", status: http.StatusOK, contains: `"runners":[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := f.api(t, tt.account, http.MethodGet, tt.path, nil, tt.status)
			if !strings.Contains(response.Body.String(), tt.contains) {
				t.Errorf("response missing %q: %s", tt.contains, response.Body.String())
			}
		})
	}
}

func TestHostedBrowserPreview(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	if os.Getenv("DETENT_HOSTED_BROWSER_PREVIEW") == "" {
		t.Skip("set DETENT_HOSTED_BROWSER_PREVIEW=1 to run the isolated browser preview")
	}
	f := newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", browserPreviewConfig)
	f.server.Config.Handler = f.sessionRefreshHandler(t, f.server.Config.Handler)
	if os.Getenv("DETENT_HOSTED_BROWSER_SPRITES") != "" {
		f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
		f.service.config.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Authorization") != "Bearer "+spritesSecretSentinel {
				return spritesTestResponse("", http.StatusUnauthorized), nil
			}
			return spritesTestResponse(`{"sprites":[]}`, http.StatusOK), nil
		})}
	}
	f.seedPreview(t)
	if os.Getenv("DETENT_HOSTED_BROWSER_WORKFLOW_REVISIONS") != "" {
		f.seedWorkflowRevisions(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_ISSUE_ASK") != "" {
		f.seedIssueAsk(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_CHAT_ACTIONS") != "" {
		f.seedCoordinatorActions(t)
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_CAPACITY") != "" {
		if err := f.service.database.configureHostedPlans(t.Context(), &HostedConfig{}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_plan_assignments SET base_id='starter',base_version=1"); err != nil {
			t.Fatal(err)
		}
		provider := &hostedBillingProvider{snapshot: billing.Snapshot{Status: "free"}}
		cfg := f.service.config.Hosted
		cfg.Billing = &HostedBillingConfig{Mode: billing.ModeTest, AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_browser_capacity"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: provider}
		for _, id := range []string{"starter", "growth", "scale"} {
			cfg.Billing.Prices = append(cfg.Billing.Prices, HostedBillingPrice{PriceID: "price_" + id + "_test", Label: id, Plan: PlanReference{ID: id, Version: 1}})
		}
		if err := f.service.database.configureHostedBilling(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		close(done)
		f.service.billing = &hostedBillingWorker{service: f.service, cancel: func() {}, done: done}
	}
	if os.Getenv("DETENT_HOSTED_BROWSER_AI_CREDITS") != "" {
		plans := hostedTestPlans(t, f.service, map[string]int64{"projects": 2})
		base := &hostedBillingProvider{snapshot: billing.Snapshot{Status: "free"}}
		cfg := f.service.config.Hosted
		cfg.Plans = &plans
		cfg.Billing = &HostedBillingConfig{AccountID: "acct_fixture", CustomerID: "cus_fixture", PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_credits_browser"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: base, Prices: []HostedBillingPrice{{PriceID: "price_fixture", Label: "Extended pilot", Plan: plans.Plans[1].PlanReference}}}
		configureTestCredits(t, f, base)
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE ai_credit_accounts SET balance_micros=1250000,payment_method='pm_credit',failure='automatic payment failed; update the saved payment method'"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES('org_browser_preview','test','fixture-purchase',1250000,'purchase',?)", f.service.config.now().UnixMicro()); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		close(done)
		f.service.billing = &hostedBillingWorker{service: f.service, cancel: func() {}, done: done}
	}

	if os.Getenv("DETENT_HOSTED_BROWSER_CHAT_USAGE") != "" {
		err := f.service.database.RecordConversationUsage(t.Context(), ConversationUsage{OrganizationID: "org_browser_preview", ProjectID: tracker.ProjectID(f.project), ConversationID: f.conversation, TurnID: "preview-chat-usage", Provider: "openai", Model: "gpt-6-luna", Tokens: runner.AgentTokenCounts{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000, ReasoningOutputTokens: 30_000}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var problemRunner runnerauth.Binding
	var problemCredential string
	if os.Getenv("DETENT_HOSTED_BROWSER_RUNNER") != "" {
		base := browserHostedOrganizationBase
		for _, project := range []string{f.project, f.privateProject} {
			f.api(t, "owner", http.MethodPut, base+"/members/membership_user_browser_owner/grants", map[string]any{
				"project_id": project, "write": true, "runner": true, "idempotency_key": "preview-runner-" + project,
			}, http.StatusOK)
		}
		names := []string{"Settings runner"}
		if os.Getenv("DETENT_HOSTED_BROWSER_RUNNER_PROBLEMS") != "" {
			names = append(names, "Healthy runner")
		}
		for _, name := range names {
			binding := runnerauth.NewBinding()
			request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project), tracker.ProjectID(f.privateProject)}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, TTLSeconds: 900}
			response := f.api(t, "owner", http.MethodPost, base+"/runner-enrollments", request, http.StatusCreated)
			var enrollment runnerauth.Enrollment
			decodeHubResponse(t, response, &enrollment)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "test-host", DisplayName: name, Capacity: 2, Version: "test", OS: "linux", Architecture: "arm64"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
			if name == "Settings runner" && os.Getenv("DETENT_HOSTED_BROWSER_RUNNER_PROBLEMS") != "" {
				problemRunner, problemCredential = binding, credential
				heartbeat := map[string]any{"display_name": name, "capacity": 2, "version": "test", "backend_isolation": redemption.BackendIsolation, "problems": []runnerauth.Problem{runnerauth.NewProblem("tier_unavailable")}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/projects/"+f.project+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, heartbeat), http.StatusOK)
			}
		}
	}
	var restartURL string
	if os.Getenv("DETENT_HOSTED_BROWSER_RESTART") != "" {
		controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.URL.Path == "/block" {
				backend := f.service.conversations.config.Backend.(*fakeCoordinatorBackend)
				backend.setRun(blockingRun(make(chan struct{}), "Working before restart"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			cfg := f.service.config
			handler := f.server.Config.Handler
			address := f.server.Listener.Addr().String()
			ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			defer cancel()
			if err := f.service.Shutdown(ctx); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f.server.CloseClientConnections()
			f.server.Close()
			if err := f.service.CloseContext(ctx); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			cfg.Conversation.Backend = newFakeCoordinatorBackend()
			service, err := Open(t.Context(), cfg)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f.service = service
			var lc net.ListenConfig
			listener, err := lc.Listen(r.Context(), "tcp", address)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			server := httptest.NewUnstartedServer(handler)
			_ = server.Listener.Close()
			server.Listener = listener
			f.server = server
			server.Start()
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(controller.Close)
		restartURL = controller.URL
	}
	accounts := make(map[string]string, len(f.cookies))
	for account := range f.cookies {
		accounts[account] = f.server.URL + "/__preview/account/" + account
	}
	fixture := struct {
		URL               string             `json:"url"`
		Login             string             `json:"login"`
		Organization      string             `json:"organization"`
		Project           string             `json:"project"`
		PrivateProject    string             `json:"private_project"`
		Chat              string             `json:"chat"`
		ProjectID         string             `json:"project_id"`
		Conversation      string             `json:"conversation"`
		WorkItem          string             `json:"work_item"`
		WorkerChat        string             `json:"worker_conversation"`
		WorkerItem        string             `json:"worker_work_item"`
		OwnerEmail        string             `json:"owner_email"`
		Accounts          map[string]string  `json:"accounts"`
		Stop              string             `json:"stop"`
		Restart           string             `json:"restart,omitempty"`
		Expires           time.Time          `json:"expires"`
		ProblemRunner     runnerauth.Binding `json:"problem_runner"`
		ProblemCredential string             `json:"problem_credential,omitempty"`
	}{
		URL: f.server.URL, Login: f.server.URL + "/login", Organization: f.server.URL + "/organization",
		Project: f.server.URL + "/projects/" + f.project, PrivateProject: f.server.URL + "/projects/" + f.privateProject,
		Chat: f.server.URL + "/chat", ProjectID: f.project, Conversation: f.conversation, WorkItem: f.workItem,
		WorkerChat: f.workerChat, WorkerItem: f.workerItem,
		OwnerEmail: browserHostedOwnerEmail, Accounts: accounts, Stop: f.server.URL + "/__preview/stop", Expires: time.Now().Add(browserHostedPreviewLifetime),
		ProblemRunner: problemRunner, ProblemCredential: problemCredential, Restart: restartURL,
	}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hosted-browser-preview.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Hosted browser fixture: %s", path)
	t.Logf("Hosted browser URL: %s", f.server.URL)
	timer := time.NewTimer(browserHostedPreviewLifetime)
	defer timer.Stop()
	select {
	case <-f.stop:
	case <-timer.C:
	case <-t.Context().Done():
	}
}

func TestHostedBrowserPreviewStopClosesStreams(t *testing.T) {
	t.Parallel()
	parent := t
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	var streams []io.ReadCloser
	if !t.Run("stop with active streams", func(t *testing.T) {
		f := newBrowserHostedFixture(t, true)
		for range 2 {
			request, err := http.NewRequestWithContext(parent.Context(), http.MethodGet, f.server.URL+"/projects/"+f.project+"/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.AddCookie(f.cookies["owner"])
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			parent.Cleanup(func() {
				if err := response.Body.Close(); err != nil {
					parent.Error(err)
				}
			})
			streams = append(streams, response.Body)
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("stream response = %d %s", response.StatusCode, response.Header.Get("Content-Type"))
			}
		}
		request, err := http.NewRequestWithContext(parent.Context(), http.MethodPost, f.server.URL+"/__preview/stop", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("stop status = %d, want %d", response.StatusCode, http.StatusNoContent)
		}
		<-f.stop
	}) {
		return
	}
	for i, response := range streams {
		if _, err := io.Copy(io.Discard, response); err == nil {
			t.Errorf("stream %d ended without the fixture closing its connection", i)
		}
	}
}

func (p *browserHostedProvider) HasUser(_ context.Context, email string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.emails[email], nil
}
