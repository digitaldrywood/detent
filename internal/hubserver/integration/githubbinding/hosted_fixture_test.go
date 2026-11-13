package githubbinding_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type bindingProvider struct {
	auth.HostedProvider
	members map[string]auth.Membership
}

func (p *bindingProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	member, ok := p.members[user]
	if !ok || member.OrganizationID != organization {
		return nil, nil
	}
	return []auth.Membership{member}, nil
}
func (p *bindingProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	return identity, nil
}

type browserBindingFixture struct {
	bindingFixture
	project string
	cookies map[string]*http.Cookie
}

func newBrowserBindingFixture(t *testing.T) *browserBindingFixture {
	t.Helper()
	provider := &bindingProvider{members: map[string]auth.Membership{}}
	plans := hubserver.HostedPlansConfig{Base: hubserver.PlanReference{ID: "pilot_free", Version: 1}, WindowSeconds: 3600, RetentionWindows: 24, ConnectedSeconds: 90, InvitationSeconds: 86400, Plans: []hubserver.HostedPlan{{PlanReference: hubserver.PlanReference{ID: "pilot_free", Version: 1}, Features: []string{"collaboration", "native_execution", "github_integration"}, Allowances: map[string]int64{"members": 10, "projects": 10, "repositories": 10, "registered_runners": 10, "connected_runners": 10, "concurrent_work": 5, "api_mutations": 1000000, "ingested_events": 1000000, "collaboration_bytes": 64 << 20, "history_records": 10000}}}}
	fixture := newBindingFixture(t, hubserver.Config{Hosted: &hubserver.HostedConfig{OrganizationID: "org_browser_preview", WorkOSOrganizationID: "provider_org_browser_preview", PublicURL: "https://browser.example.test", Provider: provider, Plans: &plans}})
	f := &browserBindingFixture{bindingFixture: fixture, cookies: map[string]*http.Cookie{}}
	project := hubtest.Project(t, f.repository, f.organization, hubtest.ProjectOptions{Name: "Browser collaboration"})
	f.project = string(project.ID)
	hubtest.Policy(t, f.repository, f.config.RunDetails, project, hubtest.PolicyOptions{})
	for _, account := range []struct {
		name, role string
		write      bool
	}{{"owner", "owner", true}, {"viewer", "viewer", false}} {
		member := hubtest.Member(t, f.repository, f.organization, hubtest.MemberOptions{Role: account.role, Projects: []tracker.ProjectID{project.ID}, ReadOnly: !account.write, ManageRunners: account.write})
		membership := auth.Membership{ID: member.Member.MembershipID, UserID: member.Member.UserID, OrganizationID: f.config.Hosted.WorkOSOrganizationID, Status: "active"}
		membership.Role.Slug = account.role
		provider.members[member.Member.UserID] = membership
		now := time.Now().UTC().Truncate(time.Microsecond)
		token := hubapi.NewID("session")
		if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: f.organization, Hash: apikey.HashToken(token), Email: member.Member.Email, Subject: member.Member.UserID, ProviderOrganizationID: f.config.Hosted.WorkOSOrganizationID, SessionID: hubapi.NewID("provider_session"), IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour), CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}
		f.cookies[account.name] = &http.Cookie{Name: "detent_hosted_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	}
	return f
}

func (f *browserBindingFixture) setupRequest(t *testing.T, account, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, f.config.Hosted.PublicURL+path, strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.config.Hosted.PublicURL)
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
		digest := sha256.Sum256([]byte("detent-hosted-csrf:" + cookie.Value))
		request.Header.Set("X-CSRF-Token", hex.EncodeToString(digest[:]))
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

type appTestRunner struct {
	runnerauth.Binding
	Credential string
}

func enrollAppRunner(t *testing.T, f *browserBindingFixture, name, version string) appTestRunner {
	t.Helper()
	binding := runnerauth.NewBinding()
	request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project)}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 900}
	response := f.setupRequest(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments", request)
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: strings.ToLower(name) + ".example.test", DisplayName: name, Capacity: 1, Version: version}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
	return appTestRunner{Binding: binding, Credential: credential}
}
