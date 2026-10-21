package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/mutation"
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
	viewer := f.cookies["viewer"]
	if owner == nil || viewer == nil {
		t.Fatal("owner or viewer session cookie is missing")
	}
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
		{name: "viewer invites", account: "viewer", method: http.MethodPost, path: "/members/invitations", body: invite, headers: map[string]string{"X-CSRF-Token": hostedCSRF(viewer.Value)}, status: http.StatusForbidden, code: "forbidden"},
		{name: "viewer revokes an invitation", account: "viewer", method: http.MethodDelete, path: "/members/invitations/invitation_browser_1", headers: map[string]string{"X-CSRF-Token": hostedCSRF(viewer.Value)}, status: http.StatusForbidden, code: "forbidden"},
		{name: "viewer creates a project", account: "viewer", method: http.MethodPost, path: "/projects", body: project, headers: map[string]string{"X-CSRF-Token": hostedCSRF(viewer.Value)}, status: http.StatusNotFound, code: "not_found"},
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

func TestHostedInvitationExpiryIndependentOfSeat(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
			service, provider := f.service, f.provider
			var created []hostedInvitationView
			for _, email := range []string{"older@example.test", "newest@example.test"} {
				var view hostedInvitationView
				browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/members/invitations", map[string]any{
					"email": email, "role": "member", "idempotency_key": email,
				}, http.StatusCreated), &view)
				issued, err := provider.Invitation(t.Context(), view.ID)
				if err != nil || view.ExpiresAt != formatHubTime(issued.ExpiresAt) {
					t.Fatalf("response expiry = %q, provider expiry = %v: %v", view.ExpiresAt, issued.ExpiresAt, err)
				}
				var storedExpiry string
				if err := service.database.db.QueryRowContext(t.Context(), "SELECT expires_at FROM hosted_invitations WHERE id=?", view.ID).Scan(&storedExpiry); err != nil || storedExpiry != view.ExpiresAt {
					t.Fatalf("stored expiry = %q, response expiry = %q: %v", storedExpiry, view.ExpiresAt, err)
				}
				if legacy {
					if _, err := service.database.db.ExecContext(t.Context(), "UPDATE hosted_invitations SET expires_at='' WHERE id=?", view.ID); err != nil {
						t.Fatal(err)
					}
				}
				created = append(created, view)
			}
			for _, releaseSeat := range []bool{false, true} {
				if releaseSeat {
					if _, err := service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_member_reservations"); err != nil {
						t.Fatal(err)
					}
				}
				listed, err := service.hostedPendingInvitations(t.Context())
				if err != nil || len(listed) != len(created) {
					t.Fatalf("listed invitations = %#v: %v", listed, err)
				}
				for index, view := range listed {
					if !reflect.DeepEqual(view, created[len(created)-1-index]) {
						t.Fatalf("listed invitation = %#v, created = %#v", view, created[len(created)-1-index])
					}
				}
			}
		})
	}
}

func TestHostedInvitationLifecycle(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
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
			if test.invitations == 1 && (members.Invitations[0].ID != created.ID || members.Invitations[0].ExpiresAt != created.ExpiresAt) {
				t.Fatalf("pending invitation = %#v", members.Invitations[0])
			}
		})
	}
	var seats int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_member_reservations WHERE email = 'pending@example.test'").Scan(&seats); err != nil || seats != 1 {
		t.Fatalf("reserved seats = %d: %v", seats, err)
	}
	path := browserHostedOrganizationBase + "/members/invitations/" + url.PathEscape(created.ID)
	for _, action := range []string{"resend", "revoke"} {
		t.Run(action+" refusal preserves invitation and seat", func(t *testing.T) {
			method, target := http.MethodDelete, path
			if action == "resend" {
				method, target = http.MethodPost, path+"/resend"
			}
			f.api(t, "viewer", method, target, map[string]any{"idempotency_key": action + "-viewer"}, http.StatusForbidden)
			calls := 0
			f.provider.invitationDelivery = func(ctx context.Context, id, gotAction string) error {
				calls++
				if id != created.ID || gotAction != action {
					t.Fatalf("provider invitation action = %s %s", gotAction, id)
				}
				var pending, held int
				if err := f.service.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_invitations WHERE id=? AND accepted_user_id=''", id).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_member_reservations WHERE email=?", created.Email).Scan(&held); err != nil || pending != 1 || held != 1 {
					t.Fatalf("provider called after local removal: pending=%d held=%d err=%v", pending, held, err)
				}
				return auth.ErrHostedIdentity
			}
			defer func() { f.provider.invitationDelivery = nil }()
			f.api(t, "owner", method, target, map[string]any{"idempotency_key": action + "-failure"}, http.StatusServiceUnavailable)
			var members hostedMembersResponse
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &members)
			if calls != 1 || len(members.Invitations) != 1 || members.Invitations[0].ID != created.ID {
				t.Fatalf("failed provider action: calls=%d invitations=%#v", calls, members.Invitations)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_member_reservations WHERE email=?", created.Email).Scan(&seats); err != nil || seats != 1 {
				t.Fatalf("failed provider action released seat: %d %v", seats, err)
			}
		})
	}
	f.api(t, "owner", http.MethodPost, path+"/resend", map[string]any{"idempotency_key": "resend"}, http.StatusNoContent)
	called := false
	f.provider.invitationDelivery = func(ctx context.Context, id, action string) error {
		called = true
		var pending int
		if err := f.service.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_invitations WHERE id=?", id).Scan(&pending); err != nil || pending != 1 || action != "revoke" {
			t.Fatalf("revoke called after local deletion: pending=%d action=%s err=%v", pending, action, err)
		}
		return nil
	}
	response := f.api(t, "owner", http.MethodDelete, path, map[string]any{"idempotency_key": "revoke"}, http.StatusNoContent)
	f.provider.invitationDelivery = nil
	providerInvitation, err := f.provider.Invitation(t.Context(), created.ID)
	if err != nil || !called || providerInvitation.State != "revoked" {
		t.Fatalf("provider revoke: called=%t invitation=%#v err=%v", called, providerInvitation, err)
	}
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
	guard := installWriterReadGuard(t, f.service)
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
	var manageRunner bool
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT manage_runner FROM hosted_project_grants WHERE project_id = ?", created.ID).Scan(&manageRunner); err != nil || !manageRunner {
		t.Fatalf("owner-created project runner management = %v, %v; want granted", manageRunner, err)
	}
	browserHostedDecode(t, f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/projects", nil, http.StatusOK), &projects)
	if slices.ContainsFunc(projects, func(project hostedProjectView) bool { return project.ID == string(created.ID) }) {
		t.Fatal("a new project was shared with a member who was not granted it")
	}
	f.service.tokenUseWork.Wait()
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if len(guard.violations) > 0 {
		t.Fatalf("hosted GET writer reads: %v", guard.violations)
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
		name     string
		request  hostedSharedRequest
		status   int
		shell    bool
		location string
	}{
		{name: "owner members", request: hostedSharedRequest{user: &owner, target: api + "/members"}, status: http.StatusOK},
		{name: "viewer members", request: hostedSharedRequest{user: &viewer, target: api + "/members"}, status: http.StatusOK},
		{name: "viewer fleet", request: hostedSharedRequest{user: &viewer, target: api + "/fleet"}, status: http.StatusOK},
		{name: "viewer projects", request: hostedSharedRequest{user: &viewer, target: api + "/projects"}, status: http.StatusOK},
		{name: "invite without CSRF", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/members/invitations", body: invite}, status: http.StatusForbidden},
		{name: "invite with another organization's CSRF", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/members/invitations", body: invite, csrf: cloudassert.CSRFToken("shared-user_owner", "org_other")}, status: http.StatusForbidden},
		{name: "invite with the entry CSRF", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/members/invitations", body: invite, csrf: ownerCSRF}, status: http.StatusCreated},
		{name: "other organization API", request: hostedSharedRequest{user: &owner, target: "/api/v2/organizations/org_other/members"}, status: http.StatusNotFound},
		{name: "platform member uses tenant role", request: hostedSharedRequest{user: &staff, target: api + "/members"}, status: http.StatusOK},
		{name: "member root", request: hostedSharedRequest{user: &owner, target: "/organizations/org_security"}, status: http.StatusPermanentRedirect, location: "/local-organization/work"},
		{name: "member project", request: hostedSharedRequest{user: &viewer, target: "/organizations/org_security/projects/prj_security"}, status: http.StatusPermanentRedirect, location: "/local-organization/projects/prj_security"},
		{name: "member project issue", request: hostedSharedRequest{user: &viewer, target: "/organizations/org_security/projects/prj_security/issues/wi_any"}, status: http.StatusNotFound},
		{name: "member project changes", request: hostedSharedRequest{user: &viewer, target: "/organizations/org_security/projects/prj_security/changes"}, status: http.StatusPermanentRedirect, location: "/local-organization/projects/prj_security/changes"},
		{name: "platform member uses tenant shell", request: hostedSharedRequest{user: &staff, target: "/organizations/org_security"}, status: http.StatusPermanentRedirect, location: "/local-organization/work"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(t, test.request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if got := response.Header().Get("Location"); got != test.location {
				t.Fatalf("location = %q, want %q", got, test.location)
			}
			if got := response.Body.String() == appShellWant("/organizations/org_security", "/organizations?switch=1"); got != test.shell {
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
		extra    []string
	}{
		{fixture: "account-members.json", target: func() any { return &hostedMembersResponse{} }},
		{fixture: "account-member.json", target: func() any { return &hostedMemberView{} }},
		{fixture: "account-invitation.json", target: func() any { return &hostedInvitationView{} }},
		{fixture: "account-projects.json", target: func() any { return &[]hostedProjectView{} }},
		{fixture: "account-fleet.json", target: func() any { return &hostedFleetResponse{} }, optional: []string{"runners[].provider_capacity[].reason"}, extra: []string{"runners[].can_edit_projects"}},
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
			compareConversationShape(t, "", want, got, conversationPathSet(test.extra), conversationPathSet(test.optional))
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
	t.Run("provider sends before its response is lost", func(t *testing.T) {
		f.service.config.Hosted.Provider = sendAndLoseInviteProvider{f.provider}
		input := map[string]any{"email": "lost@example.test", "role": "member", "idempotency_key": "lost"}
		failure := f.api(t, "owner", http.MethodPost, path, input, http.StatusServiceUnavailable)
		if strings.Contains(failure.Body.String(), "credential-invitation-support-billing-sensitive-sentinel") {
			t.Fatal("provider secret in error")
		}
		f.service.config.Hosted.Provider = f.provider
		f.api(t, "owner", http.MethodPost, path, input, http.StatusConflict)
		if count := f.providerInvitations(); count != 3 {
			t.Fatalf("duplicate invitation after lost response: %d", count)
		}
	})
	t.Run("hosted audit consumes content-free context", func(t *testing.T) {
		session, err := f.service.hostedSessions.Authenticate(t.Context(), f.cookies["owner"].Value)
		if err != nil {
			t.Fatal(err)
		}
		m := mutation.Metadata{PrincipalID: "untrusted-actor", OrganizationID: "untrusted-org", ProjectID: f.project, ResourceID: first.ID, Action: "invite_member", Source: "mcp", Confirmation: "approved", CorrelationID: "audit-correlation"}
		m, err = m.Bind("secret-business-key", map[string]string{"body": "credential-invitation-support-billing-sensitive-sentinel"})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.hostedAudit(mutation.WithContext(t.Context(), m), session.Identity, "succeeded", "invite_member", f.project, http.StatusCreated); err != nil {
			t.Fatal(err)
		}
		var summary string
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT mutation_json FROM hosted_audit WHERE event='succeeded' ORDER BY id DESC LIMIT 1").Scan(&summary); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"untrusted-actor", "untrusted-org", "secret-business-key", "credential-invitation-support-billing-sensitive-sentinel"} {
			if strings.Contains(summary, secret) {
				t.Fatalf("audit leaked %s", secret)
			}
		}
		var got struct {
			mutation.Metadata
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(summary), &got); err != nil {
			t.Fatal(err)
		}
		if got.PrincipalID != session.Identity.Subject || got.OrganizationID != f.service.config.Hosted.OrganizationID || got.CorrelationID != "audit-correlation" || got.Confirmation != "approved" || got.Source != "mcp" {
			t.Fatalf("audit context=%+v; expected actor=%q organization=%q", got, session.Identity.Subject, f.service.config.Hosted.OrganizationID)
		}
	})

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
			f.api(t, "owner", http.MethodPost, path, map[string]any{"email": "seat@example.test", "role": "member", "idempotency_key": "failed"}, http.StatusConflict)
			var pending string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT response_json FROM native_commands WHERE command_key='failed'").Scan(&pending); err != nil || pending != hostedPendingCommand {
				t.Fatalf("uncertain invitation receipt=%q %v", pending, err)
			}
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
		{name: "no visible work", runners: []hostedFleetRunner{{HostUsed: 3, MachineID: "m1"}}, want: []int{0}},
		{name: "visible work on one runner", runners: []hostedFleetRunner{{HostUsed: 3, MachineID: "m1", Leases: []hostedFleetLease{lease}}}, want: []int{1}},
		{name: "runners sharing a host add up", runners: []hostedFleetRunner{
			{HostUsed: 4, MachineID: "m1", Leases: []hostedFleetLease{lease}},
			{HostUsed: 4, MachineID: "m1", Leases: []hostedFleetLease{lease, lease}},
			{HostUsed: 2, MachineID: "m2"},
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
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", f.service.config.Hosted.OrganizationID+"/"+f.project); err != nil {
		t.Fatal(err)
	}
	organization := browserHostedOrganizationBase
	for _, project := range []string{f.project, f.privateProject} {
		requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {project}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
	}
	base := organization + "/projects/" + f.privateProject
	descriptor := hubTestPolicy()
	descriptor.Gates.Kind, descriptor.Gates.AutomatedReview = "human_review", ""
	descriptor = descriptor.WithID()
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
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
	redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "shared-host", DisplayName: "Shared runner", Capacity: 2, Version: "test", OS: "linux", Architecture: "amd64"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, organization+"/runner-enrollments/redeem", issued.Token, redemption), http.StatusCreated)
	response = f.setupRequest(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "private-run"}, Title: "Private run", Body: issueContractTestSections, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/claims", credential, tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: binding.MachineID, SessionID: "private-run", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, response, http.StatusOK)
	response = f.setupRequest(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "second-run"}, Title: "Second run", Body: issueContractTestSections, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var second tracker.NativeIssue
	decodeHubResponse(t, response, &second)
	response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/claims", credential, tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: second.WorkItemID, MachineID: binding.MachineID, SessionID: "second-run", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, response, http.StatusOK)
	var reporterPrincipal string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id=?", "user_browser_viewer").Scan(&reporterPrincipal); err != nil {
		t.Fatal(err)
	}
	checks := runnerauth.LocalChecks{Checkout: "failed", Setup: "failed", Doctor: "pending", Provider: "pending", CheckoutMessage: "Cannot clone https://github.com/acme/private.git", CheckoutFix: "gh auth login"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/machines/"+string(binding.MachineID)+"/heartbeat", credential, map[string]any{"version": "test", "capacity": 2, "display_name": "Shared runner", "local_checks": checks}), http.StatusOK)
	for _, test := range []struct {
		name, account, role   string
		allProjects, ownIssue bool
		used, leases          int
	}{
		{name: "owner all projects", account: "owner", role: "owner", allProjects: true, used: 2, leases: 2},
		{name: "viewer restricted projects", account: "viewer", role: "viewer"},
		{name: "reporter restricted projects", account: "viewer", role: "reporter"},
		{name: "reporter all projects hidden work", account: "viewer", role: "reporter", allProjects: true},
		{name: "reporter all projects own and hidden work", account: "viewer", role: "reporter", allProjects: true, ownIssue: true, used: 1, leases: 1},
		{name: "member all projects", account: "viewer", role: "member", allProjects: true, used: 2, leases: 2},
		{name: "admin all projects", account: "viewer", role: "admin", allProjects: true, used: 2, leases: 2},
		{name: "viewer all projects", account: "viewer", role: "viewer", allProjects: true, used: 2, leases: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.account == "viewer" {
				if err := f.provider.SetMembershipRole(t.Context(), "membership_user_browser_viewer", test.role); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role=? WHERE user_id=?", test.role, "user_browser_viewer"); err != nil {
					t.Fatal(err)
				}
				if test.allProjects {
					requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_viewer"}, "project": {f.privateProject}}), http.StatusSeeOther)
				} else if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", "user_browser_viewer", f.privateProject); err != nil {
					t.Fatal(err)
				}
			}
			actor := second.Actor
			if test.ownIssue {
				actor.PrincipalID = reporterPrincipal
			}
			encoded, err := json.Marshal(actor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET actor_json=? WHERE native_id=?", string(encoded), second.WorkItemID); err != nil {
				t.Fatal(err)
			}

			var fleet hostedFleetResponse
			browserHostedDecode(t, f.api(t, test.account, http.MethodGet, organization+"/fleet", nil, http.StatusOK), &fleet)
			if len(fleet.Runners) != 1 || fleet.Runners[0].MachineID != string(binding.MachineID) || fleet.Runners[0].HostUsed != test.used || len(fleet.Runners[0].Leases) != test.leases {
				t.Fatalf("fleet = %#v", fleet.Runners)
			}
			if test.role == "reporter" {
				for _, lease := range fleet.Runners[0].Leases {
					if !test.ownIssue || lease.WorkItemID != second.WorkItemID {
						t.Fatalf("hidden lease=%+v", lease)
					}
				}
			}
			checkout, reported := fleet.Runners[0].ProjectCheckouts[tracker.ProjectID(f.privateProject)]
			if reported != test.allProjects || reported && (checkout.Status != "missing" || checkout.FixCommand != "gh auth login" || checkout.Message != checks.CheckoutMessage) {
				t.Fatalf("checkout guidance leaked or missing: %+v", fleet.Runners[0].ProjectCheckouts)
			}
		})
	}
}

func TestScopeProviderUsage(t *testing.T) {
	t.Parallel()
	codex := providercapacity.Report{Provider: "codex", AccountAlias: "team", SharedAccountAlias: "team", MaxConcurrent: 2, Availability: "available"}
	other := providercapacity.Report{Provider: "codex", AccountAlias: "solo", SharedAccountAlias: "solo", MaxConcurrent: 2}
	claude := providercapacity.Report{Provider: "claude", MaxConcurrent: 1}
	local := codex
	local.SharedAccountAlias = ""
	full := "Shared provider concurrency is fully reserved; wait for lease release or expiry"
	available := "Bounded concurrency available; quota is an observation, not transferable credit"
	for _, test := range []struct {
		name         string
		state        string
		local        bool
		reservations []scopedProviderAccount
		used         int
		reason       string
	}{
		{name: "local usage excludes same alias on another runner", local: true, state: "available", reservations: []scopedProviderAccount{{runnerID: "other", Report: local}}, used: 0, reason: available},
		{name: "local usage includes own runner", local: true, state: "available", reservations: []scopedProviderAccount{{runnerID: "runner", Report: local}}, used: 1, reason: available},
		{name: "no visible reservations", state: "available", used: 0, reason: available},
		{name: "one visible reservation", state: "available", reservations: []scopedProviderAccount{{runnerID: "other", Report: codex}}, used: 1, reason: available},
		{name: "other accounts and providers do not count", state: "available", reservations: []scopedProviderAccount{{runnerID: "runner", Report: other}, {runnerID: "runner", Report: claude}}, used: 0, reason: available},
		{name: "visible reservations fill the account", state: "available", reservations: []scopedProviderAccount{{runnerID: "other", Report: codex}, {runnerID: "runner", Report: codex}}, used: 2, reason: full},
		{name: "an exhausted account keeps its reason", state: "exhausted", reservations: []scopedProviderAccount{{runnerID: "other", Report: codex}}, used: 1, reason: "exhausted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report := codex
			if test.local {
				report.SharedAccountAlias = ""
			}
			runners := []hostedFleetRunner{{ID: "runner", ProviderCapacity: []providercapacity.View{{Report: report, Used: 2, State: test.state, Reason: test.state}}}}
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

func TestHostedInvitationKeyClaim(t *testing.T) {
	t.Parallel()
	path := browserHostedOrganizationBase + "/members/invitations"
	body := map[string]any{"email": "claim@example.test", "role": "member", "idempotency_key": "claim"}
	for _, test := range []struct {
		name        string
		prepare     func(t *testing.T, f *browserHostedFixture)
		status      int
		code        string
		invitations int
	}{
		{
			name:   "a pending claim left by an interrupted request answers a retryable conflict without inviting",
			status: http.StatusConflict, code: "idempotency_in_progress", invitations: 0,
			prepare: func(t *testing.T, f *browserHostedFixture) {
				var actor string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id = 'user_browser_owner'").Scan(&actor); err != nil {
					t.Fatal(err)
				}
				hash, err := hostedCommand{input: struct{ Email, Role string }{"claim@example.test", "member"}}.hash()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_commands (organization_id, actor_id, operation, command_key, request_hash, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, "org_browser_preview", actor, "POST "+path, "claim", hash, hostedPendingCommand, formatHubTime(time.Now())); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "a reservation failure before inviting releases the key so the same key succeeds",
			status: http.StatusCreated, invitations: 1,
			prepare: func(t *testing.T, f *browserHostedFixture) {
				if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER reject_invitation_seat BEFORE INSERT ON hosted_member_reservations BEGIN SELECT RAISE(ABORT, 'fixture reservation failure'); END`); err != nil {
					t.Fatal(err)
				}
				f.api(t, "owner", http.MethodPost, path, body, http.StatusServiceUnavailable)
				if got := f.providerInvitations(); got != 0 {
					t.Fatalf("reservation failure sent %d provider invitations", got)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `DROP TRIGGER reject_invitation_seat`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "a provider failure retains the key because the outcome is uncertain",
			status: http.StatusConflict, code: "idempotency_in_progress", invitations: 0,
			prepare: func(t *testing.T, f *browserHostedFixture) {
				provider := f.service.config.Hosted.Provider
				f.service.config.Hosted.Provider = failingInviteProvider{f.provider}
				f.api(t, "owner", http.MethodPost, path, body, http.StatusServiceUnavailable)
				f.service.config.Hosted.Provider = provider
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowserHostedFixture(t, true)
			test.prepare(t, f)
			response := f.api(t, "owner", http.MethodPost, path, body, test.status)
			if test.code != "" {
				var failure apiErrorResponse
				browserHostedDecode(t, response, &failure)
				if failure.Code != test.code {
					t.Fatalf("code = %q, want %q", failure.Code, test.code)
				}
			}
			if got := f.providerInvitations(); got != test.invitations {
				t.Fatalf("provider invitations = %d, want %d", got, test.invitations)
			}
		})
	}
}

func TestHostedInvitationConcurrentKeyInvitesOnce(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	owner := f.cookies["owner"]
	if owner == nil {
		t.Fatal("owner session cookie is missing")
	}
	path := browserHostedOrganizationBase + "/members/invitations"
	body := `{"email":"race@example.test","role":"member","idempotency_key":"race"}`
	headers := map[string]string{"X-CSRF-Token": hostedCSRF(owner.Value)}
	const requests = 8
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, requests)
	var group sync.WaitGroup
	for range requests {
		group.Go(func() {
			<-start
			responses <- f.rawAPI(t, "owner", http.MethodPost, path, body, headers)
		})
	}
	close(start)
	group.Wait()
	close(responses)
	ids := map[string]bool{}
	for response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		var view hostedInvitationView
		browserHostedDecode(t, response, &view)
		ids[view.ID] = true
	}
	if len(ids) != 1 || f.providerInvitations() != 1 {
		t.Fatalf("invitations %v, provider invitations %d", ids, f.providerInvitations())
	}
}

func TestClaimHostedCommandIsExclusive(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	var actor string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id = 'user_browser_owner'").Scan(&actor); err != nil {
		t.Fatal(err)
	}
	command := hostedCommand{actor: actor, operation: "POST /claim", key: "exclusive", input: "same"}
	const callers = 8
	results := make(chan bool, callers)
	codes := make(chan int, callers)
	var group sync.WaitGroup
	for range callers {
		group.Go(func() {
			recorder := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/claim", nil), recorder)
			claimed, err := f.service.claimHostedCommand(c, command, http.StatusCreated)
			if err != nil {
				t.Error(err)
			}
			results <- claimed
			if !claimed {
				codes <- recorder.Code
			}
		})
	}
	group.Wait()
	close(results)
	close(codes)
	owners := 0
	for claimed := range results {
		if claimed {
			owners++
		}
	}
	for code := range codes {
		if code != http.StatusConflict {
			t.Fatalf("losing claim answered %d, want a retryable conflict", code)
		}
	}
	if owners != 1 {
		t.Fatalf("%d callers claimed the key, want exactly one", owners)
	}
}

type sendAndLoseInviteProvider struct{ *browserHostedProvider }

func (p sendAndLoseInviteProvider) Invite(ctx context.Context, org, email, role, actor string) (auth.Invitation, error) {
	if _, err := p.browserHostedProvider.Invite(ctx, org, email, role, actor); err != nil {
		return auth.Invitation{}, err
	}
	return auth.Invitation{}, errors.New("credential-invitation-support-billing-sensitive-sentinel")
}
