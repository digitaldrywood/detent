package hubserver

import (
	"net/http"
	"testing"
)

// TestHostedMemberManagement covers the section 12 membership endpoints: the
// role and grant changes the organization screen makes, and the last-owner
// protection the removed forms carried.
func TestHostedMemberManagement(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	base := browserHostedOrganizationBase + "/members/"
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
		}{
			{name: "viewer cannot change a role", account: "viewer", method: http.MethodPut, path: "membership_user_browser_owner/role", payload: map[string]any{"role": "member"}, status: http.StatusForbidden},
			{name: "unknown member", account: "owner", method: http.MethodPut, path: "membership_unknown/role", payload: map[string]any{"role": "member"}, status: http.StatusForbidden},
			{name: "invalid role", account: "owner", method: http.MethodPut, path: "membership_user_browser_viewer/role", payload: map[string]any{"role": "superuser"}, status: http.StatusForbidden},
			{name: "last owner keeps the organization", account: "owner", method: http.MethodDelete, path: "membership_user_browser_owner", status: http.StatusForbidden},
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
		})
	}
	f.api(t, "staff", http.MethodGet, browserHostedOrganizationBase+"/fleet", nil, http.StatusForbidden)
	browserHostedStatus(t, f.page(t, "", browserHostedOrganizationBase+"/fleet"), http.StatusUnauthorized)
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
