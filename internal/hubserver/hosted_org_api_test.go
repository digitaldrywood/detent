package hubserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestHostedMemberManagement covers the section 12 membership endpoints: the
// role and grant changes the organization screen makes, and the last-owner
// protection the removed forms carried.
func TestHostedMemberManagement(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
	base := browserHostedOrganizationBase + "/members/"
	t.Run("member identities", func(t *testing.T) {
		for _, test := range []struct {
			name, query           string
			neverSignedIn, hidden bool
		}{
			{name: "signed in member"},
			{name: "provider member without local row", neverSignedIn: true},
			{name: "signed in member with missing email", query: "UPDATE hosted_members SET email='' WHERE user_id='user_browser_viewer'"},
			{name: "locally removed member stays hidden", query: "UPDATE hosted_members SET active=0 WHERE user_id='user_browser_viewer'", hidden: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
				user := "user_browser_viewer"
				if test.neverSignedIn {
					user = "user_browser_unsigned"
					if _, err := f.provider.CreateMembership(t.Context(), user, f.provider.organization.ID, "member"); err != nil {
						t.Fatal(err)
					}
				}
				f.provider.users[user] = auth.HostedUser{ID: user, Email: "viewer@example.test", Name: "Provider Member"}
				if test.query != "" {
					if _, err := f.service.database.db.ExecContext(t.Context(), test.query); err != nil {
						t.Fatal(err)
					}
				}
				var response hostedMembersResponse
				browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &response)
				var found bool
				for _, member := range response.Members {
					if member.UserID != user {
						continue
					}
					found = true
					if member.Email != "viewer@example.test" || member.NeverSignedIn != test.neverSignedIn {
						t.Fatalf("member identity = %#v", member)
					}
					if (test.query != "" || test.neverSignedIn) && member.Name != "Provider Member" {
						t.Fatalf("provider name = %q", member.Name)
					}
				}
				if found == test.hidden {
					t.Fatalf("member found = %v, hidden = %v", found, test.hidden)
				}
				if test.neverSignedIn {
					var changed hostedMemberView
					browserHostedDecode(t, f.api(t, "owner", http.MethodPut, base+"membership_"+user+"/role", map[string]any{"role": "viewer"}, http.StatusOK), &changed)
					if changed.Email != "viewer@example.test" || changed.Name != "Provider Member" || !changed.NeverSignedIn {
						t.Fatalf("changed member identity = %#v", changed)
					}
					f.api(t, "owner", http.MethodDelete, base+"membership_"+user, nil, http.StatusNoContent)
					browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &response)
					for _, member := range response.Members {
						if member.UserID == user {
							t.Fatalf("unsigned member remains after removal: %#v", member)
						}
					}
				}
			})
		}
	})
	t.Run("owner reads the organization", func(t *testing.T) {
		var members hostedMembersResponse
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &members)
		byUser := map[string]hostedMemberView{}
		for _, member := range members.Members {
			byUser[member.UserID] = member
		}
		owner, ok := byUser["user_browser_owner"]
		if !ok || owner.Role != "owner" || owner.Email != "owner@example.test" || len(owner.Grants) != 2 {
			t.Fatalf("owner membership = %#v", owner)
		}
		viewer, ok := byUser["user_browser_viewer"]
		if !ok || viewer.Role != "viewer" || len(viewer.Grants) != 1 || viewer.Grants[0].Write {
			t.Fatalf("viewer membership = %#v", viewer)
		}
	})
	t.Run("grant round trip", func(t *testing.T) {
		var member hostedMemberView
		browserHostedDecode(t, f.api(t, "owner", http.MethodPut, base+"membership_user_browser_viewer/grants", map[string]any{
			"idempotency_key": "grant-write", "project_id": f.project, "write": true, "runner": true,
		}, http.StatusOK), &member)
		if len(member.Grants) != 1 || !member.Grants[0].Write || !member.Grants[0].Runner {
			t.Fatalf("granted member = %#v", member)
		}
		browserHostedDecode(t, f.api(t, "owner", http.MethodPut, base+"membership_user_browser_viewer/grants", map[string]any{
			"idempotency_key": "grant-revoke", "project_id": f.project, "revoke": true,
		}, http.StatusOK), &member)
		if len(member.Grants) != 0 {
			t.Fatalf("revoked member = %#v", member)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		for _, test := range []struct {
			name, account, method, path string
			payload                     any
			status                      int
			message                     string
		}{
			{name: "viewer cannot change a role", account: "viewer", method: http.MethodPut, path: "membership_user_browser_owner/role", payload: map[string]any{"role": "member"}, status: http.StatusForbidden},
			{name: "unknown member", account: "owner", method: http.MethodPut, path: "membership_unknown/role", payload: map[string]any{"role": "member"}, status: http.StatusForbidden},
			{name: "invalid role", account: "owner", method: http.MethodPut, path: "membership_user_browser_viewer/role", payload: map[string]any{"role": "superuser"}, status: http.StatusForbidden},
			{name: "last owner keeps the organization", account: "owner", method: http.MethodDelete, path: "membership_user_browser_owner", status: http.StatusForbidden, message: "This member could not be removed; the organization must retain an owner"},
			{name: "unknown member grant", account: "owner", method: http.MethodPut, path: "membership_unknown/grants", payload: map[string]any{"project_id": "prj_x"}, status: http.StatusNotFound},
			{name: "viewer cannot grant", account: "viewer", method: http.MethodPut, path: "membership_user_browser_viewer/grants", payload: map[string]any{"project_id": "prj_x"}, status: http.StatusForbidden},
		} {
			t.Run(test.name, func(t *testing.T) {
				response := f.api(t, test.account, test.method, base+test.path, test.payload, test.status)
				var failure apiErrorResponse
				browserHostedDecode(t, response, &failure)
				if failure.Code == "" || failure.Message == "" {
					t.Fatalf("error shape = %#v", failure)
				}
				if test.message != "" && failure.Message != test.message {
					t.Fatalf("error message = %q, want %q", failure.Message, test.message)
				}
			})
		}
	})
	t.Run("role change and removal", func(t *testing.T) {
		var member hostedMemberView
		browserHostedDecode(t, f.api(t, "owner", http.MethodPut, base+"membership_user_browser_viewer/role", map[string]any{"role": "member"}, http.StatusOK), &member)
		if member.Role != "member" {
			t.Fatalf("member = %#v", member)
		}
		f.api(t, "owner", http.MethodDelete, base+"membership_user_browser_viewer", nil, http.StatusNoContent)
		var members hostedMembersResponse
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &members)
		for _, member := range members.Members {
			if member.UserID == "user_browser_viewer" {
				t.Fatalf("removed member is still listed: %#v", member)
			}
		}
	})
}

// TestHostedFleetVisibility covers GET /fleet: every member reads it, and a
// lease is only described for a project the member can read.
func TestHostedFleetVisibility(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	for _, project := range []string{f.project, f.privateProject} {
		f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/members/membership_user_browser_owner/grants", map[string]any{
			"project_id": project, "write": true, "runner": true, "idempotency_key": "fleet-runner-" + project,
		}, http.StatusOK)
	}
	wantNames := map[string]hostedRunnerName{}
	var activeID, removedID string
	for _, name := range []string{"Active runner", "Retired runner"} {
		binding := runnerauth.NewBinding()
		request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.privateProject)}, Operations: []string{runnerauth.Read}, TTLSeconds: 60}
		var enrollment runnerauth.Enrollment
		browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments", request, http.StatusCreated), &enrollment)
		credential, err := apikey.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: name, Capacity: 2, Version: "test"}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
		wantNames[binding.RunnerID] = hostedRunnerName{DisplayName: name, Hostname: redemption.Hostname}
		if name == "Retired runner" {
			removedID = binding.RunnerID
			f.api(t, "owner", http.MethodDelete, browserHostedOrganizationBase+"/runners/"+removedID, nil, http.StatusNoContent)
		} else {
			activeID = binding.RunnerID
		}
	}
	now := f.service.config.now().UTC()
	summary := testHostSummary(now)
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO runner_host_hours(organization_id,runner_id,hour,summary_json,segment_ids_json) VALUES(?,?,?,?,'[]')", f.service.config.Hosted.OrganizationID, activeID, formatHubTime(summary.Hour), string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"owner", "viewer"} {
		path := browserHostedOrganizationBase + "/fleet?include=host_metrics&runner_id=" + activeID
		if account == "viewer" {
			f.api(t, account, http.MethodGet, path, nil, http.StatusNotFound)
			continue
		}
		var history []runnerHostHour
		browserHostedDecode(t, f.api(t, account, http.MethodGet, path, nil, http.StatusOK), &history)
		if len(history) != 1 || history[0].MemoryAvailableAverageBytes == nil || *history[0].MemoryAvailableAverageBytes != 200 || history[0].CPUBusyAveragePercent == nil || *history[0].CPUBusyAveragePercent != 40 {
			t.Fatalf("host history: %+v", history)
		}
		for _, runner := range []string{removedID, "runner_other_organization"} {
			f.api(t, account, http.MethodGet, browserHostedOrganizationBase+"/fleet?include=host_metrics&runner_id="+runner, nil, http.StatusNotFound)
		}
		for _, query := range []string{
			"&from=invalid", "&from=" + url.QueryEscape(now.Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Add(721*time.Hour).Format(time.RFC3339)),
		} {
			f.api(t, account, http.MethodGet, path+query, nil, http.StatusUnprocessableEntity)
		}
	}
	for _, account := range []string{"owner", "viewer"} {
		t.Run(account, func(t *testing.T) {
			var fleet hostedFleetResponse
			browserHostedDecode(t, f.api(t, account, http.MethodGet, browserHostedOrganizationBase+"/fleet", nil, http.StatusOK), &fleet)
			if fleet.Runners == nil {
				t.Fatal("fleet omitted its runner list")
			}
			if fleet.Spend != nil {
				t.Fatalf("spend = %#v without a metering source", fleet.Spend)
			}
			if fleet.Usage.Allowances == nil {
				t.Fatal("fleet omitted its allowances")
			}
			if len(fleet.Runners) != 1 || fleet.Runners[0].ID != activeID || fleet.RunnerNames[removedID] != wantNames[removedID] {
				t.Fatalf("full fleet names = %#v, %#v", fleet.Runners, fleet.RunnerNames)
			}
			response := f.api(t, account, http.MethodGet, browserHostedOrganizationBase+"/fleet?include=names", nil, http.StatusOK)
			var payload map[string]json.RawMessage
			browserHostedDecode(t, response, &payload)
			if len(payload) != 1 || payload["runner_names"] == nil {
				t.Fatalf("names projection includes unrelated fleet fields: %#v", payload)
			}
			var names map[string]hostedRunnerName
			if err := json.Unmarshal(payload["runner_names"], &names); err != nil {
				t.Fatal(err)
			}
			if len(names) != len(wantNames) {
				t.Fatalf("names = %#v, want %#v", names, wantNames)
			}
			for id, want := range wantNames {
				if names[id] != want {
					t.Fatalf("name %s = %#v, want %#v", id, names[id], want)
				}
			}
		})
	}
	for _, path := range []string{"/fleet", "/fleet?include=names", "/fleet?include=host_metrics&runner_id=" + activeID} {
		f.api(t, "staff", http.MethodGet, browserHostedOrganizationBase+path, nil, http.StatusForbidden)
		f.api(t, "revoked", http.MethodGet, browserHostedOrganizationBase+path, nil, http.StatusUnauthorized)
		f.api(t, "owner", http.MethodGet, "/api/v2/organizations/org_other"+path, nil, http.StatusNotFound)
		browserHostedStatus(t, f.page(t, "", browserHostedOrganizationBase+path), http.StatusUnauthorized)
	}
	t.Run("host history requires all project reads", func(t *testing.T) {
		path := browserHostedOrganizationBase + "/fleet?include=host_metrics&runner_id=" + activeID
		for _, test := range []struct {
			name    string
			project string
			revoke  bool
			status  int
		}{
			{name: "all projects", project: f.privateProject, status: http.StatusOK},
			{name: "only runner project", project: f.project, revoke: true, status: http.StatusNotFound},
			{name: "no projects", project: f.privateProject, revoke: true, status: http.StatusNotFound},
		} {
			t.Run(test.name, func(t *testing.T) {
				f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/members/membership_user_browser_viewer/grants", map[string]any{
					"project_id": test.project, "revoke": test.revoke, "idempotency_key": "fleet-viewer-" + test.name,
				}, http.StatusOK)
				response := f.api(t, "viewer", http.MethodGet, path, nil, test.status)
				if test.status == http.StatusOK {
					var history []runnerHostHour
					browserHostedDecode(t, response, &history)
					if len(history) != 1 || history[0].CPUBusyAveragePercent == nil || *history[0].CPUBusyAveragePercent != 40 {
						t.Fatalf("fully authorized viewer history: %+v", history)
					}
				}
			})
		}
	})
	t.Run("names do not read retained collaboration quotas", func(t *testing.T) {
		if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TABLE hosted_usage_counters"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.hostedFleetUsage(t.Context()); err == nil || !strings.Contains(err.Error(), "hosted_usage_counters") {
			t.Fatalf("full fleet quota read = %v, want unavailable collaboration accounting", err)
		}
		f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/fleet", nil, http.StatusInternalServerError)
		for _, account := range []string{"owner", "viewer"} {
			var names hostedRunnerNamesResponse
			browserHostedDecode(t, f.api(t, account, http.MethodGet, browserHostedOrganizationBase+"/fleet?include=names", nil, http.StatusOK), &names)
			if len(names.RunnerNames) != len(wantNames) {
				t.Fatalf("names require quota accounting: %#v", names)
			}
		}
	})
}

// TestSortHostedMembers covers the one order the members list is served in.
//
// The provider answers Memberships from a map, so the handler receives them in
// Go's randomized map order; without this the same organization came back
// owner-first on one read and viewer-first on the next, and the client renders
// the rows in response order.
func TestSortHostedMembers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input []hostedMemberView
		want  []string
	}{
		{
			name: "owners lead, whatever order they arrive in",
			input: []hostedMemberView{
				{ID: "m_viewer", Email: "viewer@example.test", Role: "viewer"},
				{ID: "m_owner", Email: "owner@example.test", Role: "owner"},
			},
			want: []string{"m_owner", "m_viewer"},
		},
		{
			name: "already in order is left alone",
			input: []hostedMemberView{
				{ID: "m_owner", Email: "owner@example.test", Role: "owner"},
				{ID: "m_viewer", Email: "viewer@example.test", Role: "viewer"},
			},
			want: []string{"m_owner", "m_viewer"},
		},
		{
			name: "admins sit between owners and everybody else",
			input: []hostedMemberView{
				{ID: "m_member", Email: "a@example.test", Role: "member"},
				{ID: "m_admin", Email: "b@example.test", Role: "admin"},
				{ID: "m_owner", Email: "c@example.test", Role: "owner"},
			},
			want: []string{"m_owner", "m_admin", "m_member"},
		},
		{
			name: "one role orders by email",
			input: []hostedMemberView{
				{ID: "m_c", Email: "carol@example.test", Role: "viewer"},
				{ID: "m_a", Email: "alice@example.test", Role: "viewer"},
				{ID: "m_b", Email: "bob@example.test", Role: "viewer"},
			},
			want: []string{"m_a", "m_b", "m_c"},
		},
		{
			name: "two memberships on one address order by id",
			input: []hostedMemberView{
				{ID: "m_2", Email: "same@example.test", Role: "viewer"},
				{ID: "m_1", Email: "same@example.test", Role: "viewer"},
			},
			want: []string{"m_1", "m_2"},
		},
		{
			name:  "an empty list is an empty list",
			input: []hostedMemberView{},
			want:  []string{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sortHostedMembers(test.input)
			got := make([]string, 0, len(test.input))
			for _, member := range test.input {
				got = append(got, member.ID)
			}
			if len(got) != len(test.want) {
				t.Fatalf("order = %v, want %v", got, test.want)
			}
			for index, id := range test.want {
				if got[index] != id {
					t.Fatalf("order = %v, want %v", got, test.want)
				}
			}
		})
	}
}

// TestListHostedMembersIsOrdered checks the served list itself, not only the
// comparison: the members endpoint answers in one order on every read.
func TestListHostedMembersIsOrdered(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	for attempt := range 16 {
		var members hostedMembersResponse
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &members)
		if len(members.Members) != 2 {
			t.Fatalf("members = %#v", members.Members)
		}
		if members.Members[0].Role != "owner" || members.Members[1].Role != "viewer" {
			t.Fatalf("attempt %d order = %s, %s", attempt, members.Members[0].Role, members.Members[1].Role)
		}
	}
}

func (f *browserHostedFixture) api(t *testing.T, account, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	cookie := f.cookies[account]
	if cookie == nil {
		t.Fatal("account session cookie is missing")
	}
	request := httptest.NewRequest(method, f.server.URL+path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.server.URL)
	request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	browserHostedStatus(t, response, status)
	return response
}

func browserHostedDecode(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
}

func TestHostedProjectRank(t *testing.T) {
	for _, scenario := range []struct {
		name, account string
		change        func(*projectRankChange)
		status        int
	}{
		{"owner reorders", "owner", func(*projectRankChange) {}, http.StatusOK},
		{"viewer cannot reorder", "viewer", func(*projectRankChange) {}, http.StatusForbidden},
		{"stale revision", "owner", func(c *projectRankChange) { c.ExpectedRevision-- }, http.StatusConflict},
		{"duplicate project", "owner", func(c *projectRankChange) { c.ProjectIDs[1] = c.ProjectIDs[0] }, http.StatusUnprocessableEntity},
		{"foreign project", "owner", func(c *projectRankChange) { c.ProjectIDs[0] = "prj_foreign" }, http.StatusUnprocessableEntity},
		{"incomplete list", "owner", func(c *projectRankChange) { c.ProjectIDs = c.ProjectIDs[:1] }, http.StatusUnprocessableEntity},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newBrowserHostedFixture(t, true)
			path := browserHostedOrganizationBase + "/project-rank"
			var before organizationProjectRank
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, path, nil, http.StatusOK), &before)
			ids := slices.Clone(before.ProjectIDs)
			slices.Reverse(ids)
			change := projectRankChange{ExpectedRevision: before.Revision, ProjectIDs: ids}
			scenario.change(&change)
			f.api(t, scenario.account, http.MethodPut, path, change, scenario.status)
			var after organizationProjectRank
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, path, nil, http.StatusOK), &after)
			expected, revision := before.ProjectIDs, before.Revision
			if scenario.status == http.StatusOK {
				expected, revision = ids, revision+1
			}
			if !slices.Equal(after.ProjectIDs, expected) || after.Revision != revision {
				t.Fatalf("rank changed incorrectly: %#v", after)
			}
		})
	}
}

func TestHostedRunnerRoutingAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, role                                                 string
		allGrants, shared, unassigned, addProject, running, revoke bool
		organizationScope, widenScope, narrowScope, runningGranted bool
		editable                                                   bool
	}{
		{name: "owner", role: "owner", shared: true, editable: true},
		{name: "admin", role: "admin", shared: true, editable: true},
		{name: "member all grants", role: "member", allGrants: true, shared: true, editable: true},
		{name: "member unrelated project without grant", role: "member", editable: true},
		{name: "member missing served project grant", role: "member", shared: true},
		{name: "viewer", role: "viewer"},
		{name: "member missing grant for running work", role: "member", running: true},
		{name: "member grant revoked after fleet read", role: "member", editable: true, revoke: true},
		{name: "member unassigned runner", role: "member", unassigned: true},
		{name: "owner unassigned runner", role: "owner", unassigned: true, editable: true},
		{name: "member cannot add ungranted project", role: "member", addProject: true, editable: true},
		{name: "member organization scope all grants", role: "member", organizationScope: true, allGrants: true, editable: true},
		{name: "member organization scope missing grant", role: "member", organizationScope: true},
		{name: "member organization scope with work on granted project", role: "member", organizationScope: true, running: true, runningGranted: true},
		{name: "member cannot widen to organization scope", role: "member", widenScope: true, editable: true},
		{name: "member can widen to organization scope with all grants", role: "member", widenScope: true, allGrants: true, editable: true},
		{name: "member can narrow organization scope with all grants", role: "member", organizationScope: true, narrowScope: true, allGrants: true, editable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
			account := "viewer"
			if test.role == "owner" {
				account = "owner"
			} else if test.role != "viewer" {
				f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/members/membership_user_browser_viewer/role", map[string]any{"role": test.role}, http.StatusOK)
			}
			for _, project := range []string{f.project, f.privateProject} {
				if project == f.privateProject && !test.allGrants {
					continue
				}
				f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/members/membership_user_browser_viewer/grants", map[string]any{
					"project_id": project, "write": true, "runner": true, "idempotency_key": "routing-grant-" + project,
				}, http.StatusOK)
			}
			binding := runnerauth.NewBinding()
			projects := []tracker.ProjectID{tracker.ProjectID(f.project)}
			if test.shared {
				projects = append(projects, tracker.ProjectID(f.privateProject))
			}
			var enrollment runnerauth.Enrollment
			browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments", runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: projects, Operations: []string{runnerauth.Read}, TTLSeconds: 60}, http.StatusCreated), &enrollment)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "routing-host", DisplayName: "Scoped runner", Capacity: 2, Version: "test"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
			var fleet hostedFleetResponse
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/fleet", nil, http.StatusOK), &fleet)
			change := runnerauth.RoutingChange{ExpectedRevision: fleet.Runners[0].Revision, Routing: *fleet.Runners[0].Routing}
			change.State = "draining"
			change.Tags = []string{"build", "mac"}
			if test.unassigned {
				change.ProjectIDs = []tracker.ProjectID{}
			}
			if test.organizationScope {
				change.Scope = "organization"
				change.ProjectIDs = []tracker.ProjectID{}
			}
			slices.Sort(change.ProjectIDs)
			var stored runnerauth.Runner
			browserHostedDecode(t, f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/runners/"+binding.RunnerID+"/routing", change, http.StatusOK), &stored)
			if test.running {
				var issue tracker.NativeIssue
				runningProject := f.privateProject
				if test.runningGranted {
					runningProject = f.project
				}
				browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/projects/"+runningProject+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "private-running"}, Title: "Running work", State: "Todo"}, http.StatusOK), &issue)
				now := f.service.config.now()
				stamp := formatHubTime(now)
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) SELECT 'routing-lease',id,?,'routing-session',?,?,?,?,? FROM issues WHERE native_id=?`, binding.MachineID, formatHubTime(now.Add(time.Hour)), stamp, stamp, stamp, stamp, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO lease_runners(lease_id,runner_id) VALUES('routing-lease',?)", binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if test.addProject || test.widenScope || test.narrowScope {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_enrollments SET created_by=(SELECT principal_id FROM hosted_members WHERE user_id='user_browser_viewer') WHERE id=?", enrollment.ID); err != nil {
					t.Fatal(err)
				}
			}
			browserHostedDecode(t, f.api(t, account, http.MethodGet, browserHostedOrganizationBase+"/fleet", nil, http.StatusOK), &fleet)
			view := fleet.Runners[0]
			if view.Editable != test.editable || view.Routing == nil || view.Routing.State != "draining" || !slices.Equal(view.Routing.Tags, []string{"build", "mac"}) || !slices.Equal(view.Routing.ProjectIDs, change.ProjectIDs) {
				t.Fatalf("fleet runner = %#v, routing = %#v, want editable %v", view, view.Routing, test.editable)
			}
			if !test.editable && (!strings.Contains(view.EditRefusalReason, "owner or admin") || !test.unassigned && !strings.Contains(view.EditRefusalReason, "manage_runner")) {
				t.Fatalf("missing authority explanation: %q", view.EditRefusalReason)
			}
			if (test.shared || test.organizationScope) && test.role == "member" && !test.allGrants && !strings.Contains(view.EditRefusalReason, f.privateProject) {
				t.Fatalf("refusal does not identify missing project grant: %q", view.EditRefusalReason)
			}
			if test.role == "member" && !test.allGrants && !test.shared && !test.unassigned && fleet.Editable {
				t.Fatal("partial member unexpectedly gained organization-wide authority")
			}
			change = runnerauth.RoutingChange{ExpectedRevision: view.Revision, Routing: *view.Routing}
			change.State = "active"
			change.CapacityLimit = 1
			status := http.StatusOK
			if !test.editable {
				status = http.StatusNotFound
			}
			if test.revoke {
				f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/members/membership_user_browser_viewer/grants", map[string]any{"project_id": f.project, "revoke": true, "idempotency_key": "routing-revoke"}, http.StatusOK)
				status = http.StatusNotFound
			}
			if test.addProject {
				change.ProjectIDs = append(change.ProjectIDs, tracker.ProjectID(f.privateProject))
				status = http.StatusForbidden
			}
			if test.widenScope {
				change.Scope = "organization"
				change.ProjectIDs = []tracker.ProjectID{}
				if !test.allGrants {
					status = http.StatusForbidden
				}
			}
			if test.narrowScope {
				change.Scope = "projects"
				change.ProjectIDs = []tracker.ProjectID{tracker.ProjectID(f.project)}
			}
			f.api(t, account, http.MethodPut, browserHostedOrganizationBase+"/runners/"+binding.RunnerID+"/routing", change, status)
			browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/fleet", nil, http.StatusOK), &fleet)
			wantState, wantCapacity := "draining", 2
			if status == http.StatusOK {
				wantState, wantCapacity = "active", 1
			}
			if fleet.Runners[0].Routing.State != wantState || fleet.Runners[0].Routing.CapacityLimit != wantCapacity {
				t.Fatalf("stored routing after mutation = %#v", fleet.Runners[0].Routing)
			}
			if status == http.StatusOK && (fleet.Runners[0].Routing.Scope != change.Scope || !slices.Equal(fleet.Runners[0].Routing.ProjectIDs, change.ProjectIDs)) {
				t.Fatalf("stored scope after mutation = %#v", fleet.Runners[0].Routing)
			}
		})
	}
}
