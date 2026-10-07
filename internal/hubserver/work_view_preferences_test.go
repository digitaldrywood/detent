package hubserver

import (
	"net/http"
	"strings"
	"testing"
)

func TestWorkViewPreferences(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	alice := f.user(t, "alice", "viewer", "alice@example.test", "read", "")
	bob := f.user(t, "bob", "member", "bob@example.test", "read", "")
	fresh := alice
	token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), alice.identity)
	if err != nil {
		t.Fatal(err)
	}
	fresh.token = token
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO projects (id, organization_id, name, profile, created_at) VALUES ('prj_other', 'org_security', 'other', 'native', 'now')`); err != nil {
		t.Fatal(err)
	}
	other := f
	other.project = "prj_other"
	other.grant(t, alice, false, false)
	org := "/api/v2/organizations/org_security/work-view-preference"
	project := f.base + "/work-view-preference"
	otherProject := "/api/v2/organizations/org_security/projects/prj_other/work-view-preference"
	query := "view=list&tab=closed&collapsed=Backlog%2CTodo&completed=7d&archived=true"
	for _, test := range []struct {
		name         string
		user         hostedSecurityUser
		method, path string
		body         any
		want         *string
	}{
		{name: "missing", user: alice, method: http.MethodGet, path: project},
		{name: "write", user: alice, method: http.MethodPut, path: project, body: map[string]string{"query": query}, want: &query},
		{name: "read", user: alice, method: http.MethodGet, path: project, want: &query},
		{name: "fresh session same user", user: fresh, method: http.MethodGet, path: project, want: &query},
		{name: "other user", user: bob, method: http.MethodGet, path: project},
		{name: "other project", user: alice, method: http.MethodGet, path: otherProject},
		{name: "organization scope", user: alice, method: http.MethodGet, path: org},
		{name: "write organization", user: alice, method: http.MethodPut, path: org, body: map[string]string{"query": "sort=title"}, want: new("sort=title")},
		{name: "organization independent", user: alice, method: http.MethodGet, path: project, want: &query},
		{name: "update", user: alice, method: http.MethodPut, path: project, body: map[string]string{"query": "view=list"}, want: new("view=list")},
		{name: "delete", user: alice, method: http.MethodDelete, path: project},
		{name: "read deleted", user: alice, method: http.MethodGet, path: project},
		{name: "repeat delete", user: alice, method: http.MethodDelete, path: project},
		{name: "delete preserves other scope", user: alice, method: http.MethodGet, path: org, want: new("sort=title")},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, test.user, test.method, test.path, test.body)
			requireNativeStatus(t, response, http.StatusOK)
			var got workViewPreference
			decodeHubResponse(t, response, &got)
			if test.want == nil {
				if got.Query != nil {
					t.Fatalf("query = %q, want null", *got.Query)
				}
			} else if got.Query == nil || *got.Query != *test.want {
				t.Fatalf("preference = %s, want query %q", response.Body.String(), *test.want)
			}
		})
	}
}

func TestWorkViewPreferenceAccess(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, revoke string }{
		{name: "no grant", revoke: "grant"},
		{name: "revoked member", revoke: "member"},
		{name: "revoked session", revoke: "session"},
		{name: "wrong organization", revoke: "organization"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, "member", "member", "member@example.test", "read", "")
			path := f.base + "/work-view-preference"
			switch test.revoke {
			case "grant":
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id = ?", user.identity.Subject); err != nil {
					t.Fatal(err)
				}
			case "member":
				if err := f.provider.RevokeMembership(t.Context(), "membership_"+user.identity.Subject); err != nil {
					t.Fatal(err)
				}
			case "session":
				if err := f.provider.RevokeSession(t.Context(), user.identity.Hosted.SessionID); err != nil {
					t.Fatal(err)
				}
			case "organization":
				path = strings.Replace(path, "org_security", "org_other", 1)
			}
			for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
				response := f.request(t, user, method, path, map[string]string{"query": "view=list"})
				if response.Code == http.StatusOK {
					t.Fatalf("%s allowed: %s", method, response.Body.String())
				}
			}
		})
	}
}

func TestWorkViewPreferenceValidation(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "member", "member", "member@example.test", "read", "")
	for _, test := range []struct {
		name string
		body any
	}{
		{"missing query", map[string]string{}},
		{"null query", map[string]any{"query": nil}},
		{"oversized", map[string]string{"query": strings.Repeat("x", 8193)}},
		{"malformed", map[string]string{"query": "q=%invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, user, http.MethodPut, f.base+"/work-view-preference", test.body)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		})
	}
}
