package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *browserHostedFixture) rawAPI(t *testing.T, account, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func TestHostedTenantAPIAuthorization(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	owner := f.cookies["owner"]
	csrf := map[string]string{"X-CSRF-Token": hostedCSRF(owner.Value)}
	invite := `{"email":"boundary@example.test","role":"member","idempotency_key":"boundary"}`
	project := `{"name":"Boundary project","grant_access":true,"idempotency_key":"boundary"}`
	for _, test := range []struct {
		name, account, method, path, body string
		headers                           map[string]string
		status                            int
		code                              string
	}{
		{name: "anonymous members", method: http.MethodGet, path: "/members", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "anonymous fleet", method: http.MethodGet, path: "/fleet", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "anonymous projects", method: http.MethodGet, path: "/projects", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "staff members", account: "staff", method: http.MethodGet, path: "/members", status: http.StatusForbidden, code: "forbidden"},
		{name: "staff projects", account: "staff", method: http.MethodGet, path: "/projects", status: http.StatusForbidden, code: "forbidden"},
		{name: "wrong organization session", account: "wrong-organization", method: http.MethodGet, path: "/members", status: http.StatusForbidden, code: "forbidden"},
		{name: "revoked session", account: "revoked", method: http.MethodGet, path: "/fleet", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "invite without CSRF", account: "owner", method: http.MethodPost, path: "/members/invitations", body: invite, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "invite with a foreign CSRF", account: "owner", method: http.MethodPost, path: "/members/invitations", body: invite, headers: map[string]string{"X-CSRF-Token": hostedCSRF("other")}, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "role change without CSRF", account: "owner", method: http.MethodPut, path: "/members/membership_user_browser_viewer/role", body: `{"role":"member"}`, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "project without CSRF", account: "owner", method: http.MethodPost, path: "/projects", body: project, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "bearer on the session API", method: http.MethodPost, path: "/members/invitations", body: invite, headers: map[string]string{"Authorization": "Bearer " + testHubAdminToken}, status: http.StatusForbidden, code: "forbidden"},
		{name: "bearer member read", method: http.MethodGet, path: "/members", headers: map[string]string{"Authorization": "Bearer " + testHubAdminToken}, status: http.StatusForbidden, code: "forbidden"},
		{name: "viewer invites", account: "viewer", method: http.MethodPost, path: "/members/invitations", body: invite, headers: map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies["viewer"].Value)}, status: http.StatusForbidden, code: "forbidden"},
		{name: "viewer revokes an invitation", account: "viewer", method: http.MethodDelete, path: "/members/invitations/invitation_browser_1", headers: map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies["viewer"].Value)}, status: http.StatusForbidden, code: "forbidden"},
		{name: "viewer creates a project", account: "viewer", method: http.MethodPost, path: "/projects", body: project, headers: map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies["viewer"].Value)}, status: http.StatusNotFound, code: "not_found"},
		{name: "invite without idempotency key", account: "owner", method: http.MethodPost, path: "/members/invitations", body: `{"email":"key@example.test","role":"member"}`, headers: csrf, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "invite with unknown field", account: "owner", method: http.MethodPost, path: "/members/invitations", body: `{"email":"key@example.test","role":"member","idempotency_key":"k","admin":true}`, headers: csrf, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "invite staff address", account: "owner", method: http.MethodPost, path: "/members/invitations", body: `{"email":"staff@example.test","role":"member","idempotency_key":"staff"}`, headers: csrf, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "project without explicit access", account: "owner", method: http.MethodPost, path: "/projects", body: `{"name":"Unapproved","grant_access":false,"idempotency_key":"unapproved"}`, headers: csrf, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "project without idempotency key", account: "owner", method: http.MethodPost, path: "/projects", body: `{"name":"Keyless","grant_access":true}`, headers: csrf, status: http.StatusUnprocessableEntity},
		{name: "unknown invitation", account: "owner", method: http.MethodDelete, path: "/members/invitations/invitation_missing", headers: csrf, status: http.StatusNotFound, code: "not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.rawAPI(t, test.account, test.method, browserHostedOrganizationBase+test.path, test.body, test.headers)
			browserHostedStatus(t, response, test.status)
			if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
			}
			var failure apiErrorResponse
			browserHostedDecode(t, response, &failure)
			if test.code != "" && failure.Code != test.code || failure.Message == "" {
				t.Fatalf("error = %#v, want code %q", failure, test.code)
			}
		})
	}
	t.Run("another organization is not found", func(t *testing.T) {
		for _, path := range []string{"/members", "/fleet", "/projects", "/members/invitations"} {
			method := http.MethodGet
			if path == "/members/invitations" {
				method = http.MethodPost
			}
			response := f.rawAPI(t, "owner", method, "/api/v2/organizations/org_other"+path, invite, csrf)
			browserHostedStatus(t, response, http.StatusNotFound)
		}
	})
}

func TestHostedInvitationLifecycle(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	var created hostedInvitationView
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/members/invitations", map[string]any{
		"email": "Pending@Example.test", "role": "member", "idempotency_key": "pending",
	}, http.StatusCreated), &created)
	if created.ID == "" || created.Email != "pending@example.test" || created.Role != "member" || created.CreatedAt == "" {
		t.Fatalf("invitation = %#v", created)
	}
	for _, test := range []struct {
		account     string
		invitations int
		members     []string
	}{
		{account: "owner", invitations: 1, members: []string{"user_browser_owner", "user_browser_viewer"}},
		{account: "viewer", members: []string{"user_browser_viewer"}},
		{account: "support-viewer", members: []string{"user_browser_viewer"}},
	} {
		t.Run("list as "+test.account, func(t *testing.T) {
			var members hostedMembersResponse
			browserHostedDecode(t, f.api(t, test.account, http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &members)
			users := []string{}
			for _, member := range members.Members {
				users = append(users, member.UserID)
			}
			if !slices.Equal(users, test.members) || len(members.Invitations) != test.invitations {
				t.Fatalf("members = %v, invitations = %#v", users, members.Invitations)
			}
			if test.invitations == 1 && (members.Invitations[0].ID != created.ID || members.Invitations[0].ExpiresAt == "") {
				t.Fatalf("pending invitation = %#v", members.Invitations[0])
			}
		})
	}
	var seats int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_member_reservations WHERE email = 'pending@example.test'").Scan(&seats); err != nil || seats != 1 {
		t.Fatalf("reserved seats = %d: %v", seats, err)
	}
	path := browserHostedOrganizationBase + "/members/invitations/" + url.PathEscape(created.ID)
	response := f.api(t, "owner", http.MethodDelete, path, map[string]any{"idempotency_key": "revoke"}, http.StatusNoContent)
	if response.Body.Len() != 0 {
		t.Fatalf("revocation body = %q", response.Body.String())
	}
	f.api(t, "owner", http.MethodDelete, path, map[string]any{"idempotency_key": "revoke"}, http.StatusNotFound)
	var members hostedMembersResponse
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &members)
	if len(members.Invitations) != 0 {
		t.Fatalf("revoked invitation is still pending: %#v", members.Invitations)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_member_reservations WHERE email = 'pending@example.test'").Scan(&seats); err != nil || seats != 0 {
		t.Fatalf("revocation kept %d reserved seats: %v", seats, err)
	}
	if response := f.page(t, "", "/invite?invitation_token="+url.QueryEscape(created.ID)); response.Code != http.StatusForbidden {
		t.Fatalf("revoked invitation link status = %d", response.Code)
	}
}

func TestHostedProjectsAPI(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	for _, test := range []struct {
		account  string
		projects []string
		write    bool
	}{
		{account: "owner", projects: []string{f.project, f.privateProject}, write: true},
		{account: "viewer", projects: []string{f.project}},
		{account: "support-viewer", projects: []string{f.project}},
	} {
		t.Run(test.account, func(t *testing.T) {
			var projects []hostedProjectView
			browserHostedDecode(t, f.api(t, test.account, http.MethodGet, browserHostedOrganizationBase+"/projects", nil, http.StatusOK), &projects)
			ids := []string{}
			for _, project := range projects {
				ids = append(ids, project.ID)
				if project.CanWrite != test.write || project.Onboarding.Steps == nil || len(project.States) == 0 {
					t.Fatalf("project = %#v", project)
				}
			}
			slices.Sort(ids)
			want := slices.Clone(test.projects)
			slices.Sort(want)
			if !slices.Equal(ids, want) {
				t.Fatalf("projects = %v, want %v", ids, want)
			}
		})
	}
	request := map[string]any{"name": "Client project", "grant_access": true, "idempotency_key": "client-project"}
	var created tracker.NativeProject
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/projects", request, http.StatusCreated), &created)
	if !strings.HasPrefix(string(created.ID), "prj_") || created.Name != "Client project" || created.OrganizationID != "org_browser_preview" {
		t.Fatalf("created project = %#v", created)
	}
	var again tracker.NativeProject
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/projects", request, http.StatusCreated), &again)
	if again.ID != created.ID {
		t.Fatalf("retry created %s, first %s", again.ID, created.ID)
	}
	var projects []hostedProjectView
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/projects", nil, http.StatusOK), &projects)
	if !slices.ContainsFunc(projects, func(project hostedProjectView) bool { return project.ID == string(created.ID) && project.CanWrite }) {
		t.Fatalf("creator has no write grant on %s: %#v", created.ID, projects)
	}
	browserHostedDecode(t, f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/projects", nil, http.StatusOK), &projects)
	if slices.ContainsFunc(projects, func(project hostedProjectView) bool { return project.ID == string(created.ID) }) {
		t.Fatal("a new project was shared with a member who was not granted it")
	}
}

func TestHostedSharedTenantAPI(t *testing.T) {
	useAppClientFS(t, appClientBundle())
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	viewer := f.member(t, "viewer", "viewer", "read")
	staff := f.member(t, "staffer", "owner", "write")
	staff.identity.Email = "staff@example.test"
	ownerCSRF := cloudassert.CSRFToken("shared-user_owner", "org_security")
	api := "/api/v2/organizations/org_security"
	invite := `{"email":"shared@example.test","role":"member","idempotency_key":"shared"}`
	for _, test := range []struct {
		name    string
		request hostedSharedRequest
		status  int
		shell   bool
	}{
		{name: "owner members", request: hostedSharedRequest{user: &owner, target: api + "/members"}, status: http.StatusOK},
		{name: "viewer members", request: hostedSharedRequest{user: &viewer, target: api + "/members"}, status: http.StatusOK},
		{name: "viewer fleet", request: hostedSharedRequest{user: &viewer, target: api + "/fleet"}, status: http.StatusOK},
		{name: "viewer projects", request: hostedSharedRequest{user: &viewer, target: api + "/projects"}, status: http.StatusOK},
		{name: "invite without CSRF", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/members/invitations", body: invite}, status: http.StatusForbidden},
		{name: "invite with another organization's CSRF", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/members/invitations", body: invite, csrf: cloudassert.CSRFToken("shared-user_owner", "org_other")}, status: http.StatusForbidden},
		{name: "invite with the entry CSRF", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/members/invitations", body: invite, csrf: ownerCSRF}, status: http.StatusCreated},
		{name: "other organization API", request: hostedSharedRequest{user: &owner, target: "/api/v2/organizations/org_other/members"}, status: http.StatusNotFound},
		{name: "staff members", request: hostedSharedRequest{user: &staff, target: api + "/members"}, status: http.StatusForbidden},
		{name: "member root", request: hostedSharedRequest{user: &owner, target: "/organizations/org_security"}, status: http.StatusOK, shell: true},
		{name: "member project", request: hostedSharedRequest{user: &viewer, target: "/organizations/org_security/projects/prj_security"}, status: http.StatusOK, shell: true},
		{name: "member project issue", request: hostedSharedRequest{user: &viewer, target: "/organizations/org_security/projects/prj_security/issues/wi_any"}, status: http.StatusOK, shell: true},
		{name: "member project changes", request: hostedSharedRequest{user: &viewer, target: "/organizations/org_security/projects/prj_security/changes"}, status: http.StatusOK, shell: true},
		{name: "staff root keeps the organization page", request: hostedSharedRequest{user: &staff, target: "/organizations/org_security"}, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(t, test.request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if got := response.Body.String() == appShellWant("/organizations/org_security", "/organizations"); got != test.shell {
				t.Fatalf("shell served = %t, want %t: %s", got, test.shell, response.Body.String())
			}
			if test.status != http.StatusOK || test.shell || !strings.HasPrefix(test.request.target, api) {
				return
			}
			var payload any
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode %s: %v", response.Body.String(), err)
			}
		})
	}
	response := f.serve(t, hostedSharedRequest{user: &viewer, target: api + "/members"})
	var members hostedMembersResponse
	if err := json.Unmarshal(response.Body.Bytes(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Members) != 1 || members.Members[0].UserID != "user_viewer" || len(members.Invitations) != 0 {
		t.Fatalf("viewer members = %#v", members)
	}
	response = f.serve(t, hostedSharedRequest{user: &owner, target: api + "/members"})
	if err := json.Unmarshal(response.Body.Bytes(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members.Invitations) != 1 || members.Invitations[0].Email != "shared@example.test" {
		t.Fatalf("owner invitations = %#v", members.Invitations)
	}
	revoke := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodDelete, target: api + "/members/invitations/" + members.Invitations[0].ID, body: `{"idempotency_key":"revoke"}`, csrf: ownerCSRF})
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("shared revocation = %d: %s", revoke.Code, revoke.Body.String())
	}
}

func TestHostedTenantContractFixtures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		fixture  string
		target   func() any
		optional []string
	}{
		{fixture: "account-members.json", target: func() any { return &hostedMembersResponse{} }},
		{fixture: "account-member.json", target: func() any { return &hostedMemberView{} }},
		{fixture: "account-invitation.json", target: func() any { return &hostedInvitationView{} }},
		{fixture: "account-projects.json", target: func() any { return &[]hostedProjectView{} }},
		{fixture: "account-fleet.json", target: func() any { return &hostedFleetResponse{} }, optional: []string{"runners[].provider_capacity[].reason"}},
		{fixture: "account-fleet-empty.json", target: func() any { return &hostedFleetResponse{} }},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join(conversationFixtureDirectory, test.fixture))
			if err != nil {
				t.Skipf("shared contract fixture is not available: %v", err)
			}
			value := test.target()
			if err := json.Unmarshal(raw, value); err != nil {
				t.Fatalf("decode fixture into the Go payload: %v", err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			compareConversationShape(t, "", want, got, conversationPathSet(nil), conversationPathSet(test.optional))
		})
	}
}

type failingInviteProvider struct {
	*browserHostedProvider
}

func (failingInviteProvider) Invite(context.Context, string, string, string, string) (auth.Invitation, error) {
	return auth.Invitation{}, errors.New("provider unavailable")
}

func (f *browserHostedFixture) invitationSeats(t *testing.T, email string) int {
	t.Helper()
	var seats int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_member_reservations WHERE email = ?", email).Scan(&seats); err != nil {
		t.Fatal(err)
	}
	return seats
}

func (f *browserHostedFixture) providerInvitations() int {
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	return len(f.provider.invitations)
}

func TestHostedInvitationIdempotency(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	path := browserHostedOrganizationBase + "/members/invitations"
	var first hostedInvitationView
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, path, map[string]any{"email": "retry@example.test", "role": "member", "idempotency_key": "retry"}, http.StatusCreated), &first)
	for _, test := range []struct {
		name        string
		account     string
		body        map[string]any
		status      int
		sameID      bool
		invitations int
	}{
		{name: "retry replays the first invitation", account: "owner", body: map[string]any{"email": "retry@example.test", "role": "member", "idempotency_key": "retry"}, status: http.StatusCreated, sameID: true, invitations: 1},
		{name: "retry with different case replays", account: "owner", body: map[string]any{"email": " Retry@Example.test", "role": "member", "idempotency_key": "retry"}, status: http.StatusCreated, sameID: true, invitations: 1},
		{name: "same key for another address conflicts", account: "owner", body: map[string]any{"email": "other@example.test", "role": "member", "idempotency_key": "retry"}, status: http.StatusConflict, invitations: 1},
		{name: "same key for another role conflicts", account: "owner", body: map[string]any{"email": "retry@example.test", "role": "viewer", "idempotency_key": "retry"}, status: http.StatusConflict, invitations: 1},
		{name: "a new key is a new invitation", account: "owner", body: map[string]any{"email": "second@example.test", "role": "member", "idempotency_key": "second"}, status: http.StatusCreated, invitations: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.api(t, test.account, http.MethodPost, path, test.body, test.status)
			if test.status == http.StatusCreated {
				var view hostedInvitationView
				browserHostedDecode(t, response, &view)
				if (view.ID == first.ID) != test.sameID {
					t.Fatalf("invitation %s, first %s, want same = %t", view.ID, first.ID, test.sameID)
				}
			}
			if got := f.providerInvitations(); got != test.invitations {
				t.Fatalf("provider invitations = %d, want %d", got, test.invitations)
			}
		})
	}
}

func TestHostedInvitationFailureKeepsHeldSeat(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		pending bool
		seats   int
	}{
		{name: "a new address gives its seat back", seats: 0},
		{name: "a pending address keeps its seat", pending: true, seats: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowserHostedFixture(t, true)
			path := browserHostedOrganizationBase + "/members/invitations"
			if test.pending {
				f.api(t, "owner", http.MethodPost, path, map[string]any{"email": "seat@example.test", "role": "member", "idempotency_key": "first"}, http.StatusCreated)
			}
			f.service.config.Hosted.Provider = failingInviteProvider{f.provider}
			f.api(t, "owner", http.MethodPost, path, map[string]any{"email": "seat@example.test", "role": "member", "idempotency_key": "failed"}, http.StatusServiceUnavailable)
			if got := f.invitationSeats(t, "seat@example.test"); got != test.seats {
				t.Fatalf("seats = %d, want %d", got, test.seats)
			}
			form := f.form(t, "owner", "/organization/invite", url.Values{"email": {"seat@example.test"}, "role": {"member"}})
			browserHostedStatus(t, form, http.StatusServiceUnavailable)
			if got := f.invitationSeats(t, "seat@example.test"); got != test.seats {
				t.Fatalf("form failure left %d seats, want %d", got, test.seats)
			}
		})
	}
}

func TestScopeHostUsage(t *testing.T) {
	t.Parallel()
	lease := hostedFleetLease{LeaseID: "lease"}
	for _, test := range []struct {
		name    string
		runners []hostedFleetRunner
		want    []int
	}{
		{name: "no visible work", runners: []hostedFleetRunner{{HostUsed: 3, machine: "m1"}}, want: []int{0}},
		{name: "visible work on one runner", runners: []hostedFleetRunner{{HostUsed: 3, machine: "m1", Leases: []hostedFleetLease{lease}}}, want: []int{1}},
		{name: "runners sharing a host add up", runners: []hostedFleetRunner{
			{HostUsed: 4, machine: "m1", Leases: []hostedFleetLease{lease}},
			{HostUsed: 4, machine: "m1", Leases: []hostedFleetLease{lease, lease}},
			{HostUsed: 2, machine: "m2"},
		}, want: []int{3, 3, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scopeHostUsage(test.runners)
			for index, runner := range test.runners {
				if runner.HostUsed != test.want[index] {
					t.Fatalf("runner %d host used = %d, want %d", index, runner.HostUsed, test.want[index])
				}
			}
		})
	}
}

func TestHostedFleetHostUsageScope(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	organization := browserHostedOrganizationBase
	for _, project := range []string{f.project, f.privateProject} {
		requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {project}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
	}
	base := organization + "/projects/" + f.privateProject
	descriptor := hubTestPolicy()
	descriptor.Gates.Kind, descriptor.Gates.AutomatedReview = "human_review", ""
	descriptor = descriptor.WithID()
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{Policy: descriptor}), http.StatusOK)
	binding := runnerauth.NewBinding()
	enrollment := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project), tracker.ProjectID(f.privateProject)}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 900}
	response := f.setupRequest(t, "owner", http.MethodPost, organization+"/runner-enrollments", enrollment)
	requireNativeStatus(t, response, http.StatusCreated)
	var issued runnerauth.Enrollment
	decodeHubResponse(t, response, &issued)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "shared-host", DisplayName: "Shared runner", Capacity: 2, Version: "test", OS: "linux", Architecture: "amd64"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, organization+"/runner-enrollments/redeem", issued.Token, redemption), http.StatusCreated)
	response = f.setupRequest(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "private-run"}, Title: "Private run", State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/claims", credential, tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: binding.MachineID, SessionID: "private-run", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, response, http.StatusOK)
	for _, test := range []struct {
		account string
		used    int
		leases  int
	}{
		{account: "owner", used: 1, leases: 1},
		{account: "viewer", used: 0, leases: 0},
	} {
		t.Run(test.account, func(t *testing.T) {
			var fleet hostedFleetResponse
			browserHostedDecode(t, f.api(t, test.account, http.MethodGet, organization+"/fleet", nil, http.StatusOK), &fleet)
			if len(fleet.Runners) != 1 || fleet.Runners[0].HostUsed != test.used || len(fleet.Runners[0].Leases) != test.leases {
				t.Fatalf("fleet = %#v", fleet.Runners)
			}
		})
	}
}

func TestScopeProviderUsage(t *testing.T) {
	t.Parallel()
	codex := providercapacity.Report{Provider: "codex", AccountAlias: "team", SharedAccountAlias: "team", MaxConcurrent: 2, Availability: "available"}
	other := providercapacity.Report{Provider: "codex", AccountAlias: "solo", SharedAccountAlias: "solo", MaxConcurrent: 2}
	claude := providercapacity.Report{Provider: "claude", MaxConcurrent: 1}
	full := "Shared provider concurrency is fully reserved; wait for lease release or expiry"
	available := "Bounded concurrency available; quota is an observation, not transferable credit"
	for _, test := range []struct {
		name         string
		state        string
		reservations []providercapacity.Report
		used         int
		reason       string
	}{
		{name: "no visible reservations", state: "available", used: 0, reason: available},
		{name: "one visible reservation", state: "available", reservations: []providercapacity.Report{codex}, used: 1, reason: available},
		{name: "other accounts and providers do not count", state: "available", reservations: []providercapacity.Report{other, claude}, used: 0, reason: available},
		{name: "visible reservations fill the account", state: "available", reservations: []providercapacity.Report{codex, codex}, used: 2, reason: full},
		{name: "an exhausted account keeps its reason", state: "exhausted", reservations: []providercapacity.Report{codex}, used: 1, reason: "exhausted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runners := []hostedFleetRunner{{ProviderCapacity: []providercapacity.View{{Report: codex, Used: 2, State: test.state, Reason: test.state}}}}
			if test.state != "exhausted" {
				runners[0].ProviderCapacity[0].Reason = full
			}
			scopeProviderUsage(runners, test.reservations)
			view := runners[0].ProviderCapacity[0]
			if view.Used != test.used || view.Reason != test.reason {
				t.Fatalf("view = used %d reason %q, want %d %q", view.Used, view.Reason, test.used, test.reason)
			}
		})
	}
}
