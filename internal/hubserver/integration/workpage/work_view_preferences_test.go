package workpage_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestWorkViewPreferences(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	alice := f.User(t, "alice", "viewer", "alice@example.test", "read", "")
	bob := f.User(t, "bob", "member", "bob@example.test", "read", "")
	fresh := alice
	fresh.Token = f.Session(t, alice.Identity)
	other := f.SeedProject(t, "prj_other", "Other project")
	f.Grant(t, alice, other.ID, false, false)
	org := hubfixture.OrganizationBase(f.Primary) + "/work-view-preference"
	project := f.Base + "/work-view-preference"
	otherProject := hubfixture.ProjectBase(f.Primary, other.ID) + "/work-view-preference"
	query := "view=list&tab=closed&collapsed=Backlog%2CTodo&completed=7d&archived=true"
	for _, test := range []struct {
		name         string
		user         hubfixture.HostedUser
		method, path string
		body         any
		want         *string
	}{
		{name: "missing", user: alice, method: http.MethodGet, path: project},
		{name: "write", user: alice, method: http.MethodPut, path: project, body: map[string]string{"query": query}, want: &query},
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
			response := f.Request(t, test.user, test.method, test.path, test.body)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var got struct {
				Query *string `json:"query"`
			}
			hubfixture.Decode(t, response, &got)
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
	f := hubfixture.NewHostedSecurity(t)
	project := f.Base + "/work-view-preference"
	ungranted := f.User(t, "ungranted", "member", "ungranted@example.test", "read", "")
	if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), ungranted.Identity.Subject, f.Project)
	}); err != nil {
		t.Fatal(err)
	}
	member := f.User(t, "revoked", "member", "revoked@example.test", "read", "")
	if err := f.Provider.RevokeMembership(t.Context(), "membership_"+member.Identity.Subject); err != nil {
		t.Fatal(err)
	}
	session := f.User(t, "session", "member", "session@example.test", "read", "")
	if err := f.Provider.RevokeSession(t.Context(), session.Identity.Hosted.SessionID); err != nil {
		t.Fatal(err)
	}
	foreign := f.User(t, "foreign", "member", "foreign@example.test", "read", "")
	for _, test := range []struct {
		name    string
		user    hubfixture.HostedUser
		path    string
		methods []string
	}{
		{"no grant", ungranted, project, []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
		{"revoked member", member, project, []string{http.MethodGet}},
		{"revoked session", session, project, []string{http.MethodGet}},
		{"wrong organization", foreign, hubfixture.ProjectBase("org_other", f.Project) + "/work-view-preference", []string{http.MethodGet}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, method := range test.methods {
				response := f.Request(t, test.user, method, test.path, map[string]string{"query": "view=list"})
				if response.Code == http.StatusOK {
					t.Fatalf("%s allowed: %s", method, response.Body.String())
				}
			}
		})
	}
}

func TestWorkViewPreferenceValidation(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	user := f.User(t, "member", "member", "member@example.test", "read", "")
	for _, test := range []struct {
		name string
		body any
	}{
		{"missing query", map[string]string{}},
		{"oversized", map[string]string{"query": strings.Repeat("x", 8193)}},
		{"malformed", map[string]string{"query": "q=%invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.Request(t, user, http.MethodPut, f.Base+"/work-view-preference", test.body)
			hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
		})
	}
}
