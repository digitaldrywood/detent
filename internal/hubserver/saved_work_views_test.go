package hubserver

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestSavedWorkViews(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	alice := f.user(t, "alice", "viewer", "alice@example.test", "read", "")
	bob := f.user(t, "bob", "member", "bob@example.test", "read", "")
	base := "/api/v2/organizations/org_security/work-views"
	state := savedWorkState{View: "list", Tab: "closed", Q: "a search", Sort: "updated", CompletedWindow: "7d", Archived: true, CreatedBy: "me", State: []string{"Done"}, Type: []string{"bug"}, Label: []string{"label-a", "label-b"}, Assignee: []string{"alice", "bob"}, Priority: []string{"1", "2"}, Lanes: []string{}, Collapsed: []string{"Todo", "Backlog"}}
	create := func(user hostedSecurityUser, name, scope string, project *string) savedWorkView {
		t.Helper()
		response := f.request(t, user, http.MethodPost, base, savedWorkView{Name: name, Scope: scope, ProjectID: project, Favorite: true, State: state})
		requireNativeStatus(t, response, http.StatusOK)
		var v savedWorkView
		decodeHubResponse(t, response, &v)
		if !v.CanEdit || !reflect.DeepEqual(v.State, state) || v.OwnerID != user.identity.Subject || !v.Favorite || v.Name != name {
			t.Fatalf("created view = %+v", v)
		}
		return v
	}
	list := func(user hostedSecurityUser) []savedWorkView {
		t.Helper()
		response := f.request(t, user, http.MethodGet, base, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var result struct {
			Views []savedWorkView `json:"views"`
		}
		decodeHubResponse(t, response, &result)
		return result.Views
	}
	personal := create(alice, "Personal", "personal", nil)
	other := create(bob, "Other", "personal", nil)
	if got := list(alice); len(got) != 1 || got[0].ID != personal.ID {
		t.Fatalf("personal visibility = %+v", got)
	}
	if got := list(bob); len(got) != 1 || got[0].ID != other.ID {
		t.Fatalf("other visibility = %+v", got)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		requireNativeStatus(t, f.request(t, bob, method, base+"/"+personal.ID, personal), http.StatusNotFound)
	}
	personal.Name = "Shared renamed"
	personal.Scope = "organization"
	response := f.request(t, alice, http.MethodPut, base+"/"+personal.ID, personal)
	requireNativeStatus(t, response, http.StatusOK)
	if got := list(bob); len(got) != 2 {
		t.Fatalf("shared list = %+v", got)
	}
	response = f.request(t, bob, http.MethodGet, base+"/"+personal.ID, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var shared savedWorkView
	decodeHubResponse(t, response, &shared)
	if shared.Favorite || shared.CanEdit || shared.Name != "Shared renamed" || !reflect.DeepEqual(shared.State, state) {
		t.Fatalf("shared view = %+v", shared)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		requireNativeStatus(t, f.request(t, bob, method, base+"/"+personal.ID, personal), http.StatusNotFound)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodPut, base+"/"+personal.ID+"/favorite", map[string]bool{"favorite": true}), http.StatusOK)
	requireNativeStatus(t, f.request(t, alice, http.MethodPut, base+"/"+personal.ID+"/favorite", map[string]bool{"favorite": false}), http.StatusOK)
	response = f.request(t, bob, http.MethodGet, base+"/"+personal.ID, nil)
	decodeHubResponse(t, response, &shared)
	if !shared.Favorite {
		t.Fatal("another user's favorite was removed")
	}
	project := string(f.project)
	projectView := create(alice, "Project", "organization", &project)
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=?", bob.identity.Subject); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodGet, base+"/"+projectView.ID, nil), http.StatusNotFound)
	if got := list(bob); len(got) != 2 {
		t.Fatalf("grantless shared list = %+v", got)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodPost, base, savedWorkView{Name: "Denied", Scope: "personal", ProjectID: &project, State: state}), http.StatusNotFound)
	provider := newHostedSecurityProvider()
	second := hostedSecurityFixture{provider: provider, service: openTestService(t, Config{
		DatabasePath: hostedTestDatabasePath(t), GitHubDisabled: true,
		Hosted: &HostedConfig{OrganizationID: "org_other", WorkOSOrganizationID: "org_provider", PublicURL: "http://127.0.0.1:7777", Provider: provider},
	})}
	charlie := second.user(t, "charlie", "member", "charlie@example.test", "", "")
	secondBase := "/api/v2/organizations/org_other/work-views"
	response = second.request(t, charlie, http.MethodPost, secondBase, savedWorkView{Name: "Other organization", Scope: "organization", State: state})
	requireNativeStatus(t, response, http.StatusOK)
	var secondView savedWorkView
	decodeHubResponse(t, response, &secondView)
	requireNativeStatus(t, second.request(t, charlie, http.MethodGet, secondBase+"/"+personal.ID, nil), http.StatusNotFound)
	requireNativeStatus(t, f.request(t, alice, http.MethodGet, base+"/"+secondView.ID, nil), http.StatusNotFound)
	requireNativeStatus(t, second.request(t, alice, http.MethodGet, secondBase, nil), http.StatusUnauthorized)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		requireNativeStatus(t, f.request(t, alice, method, strings.Replace(base, "org_security", "org_other", 1)+"/"+personal.ID, personal), http.StatusNotFound)
	}
	personal.State.Lanes = nil
	personal.State.Label = []string{}
	personal.Scope = "personal"
	response = f.request(t, alice, http.MethodPut, base+"/"+personal.ID, personal)
	requireNativeStatus(t, response, http.StatusOK)
	var updated savedWorkView
	decodeHubResponse(t, response, &updated)
	if updated.State.Lanes != nil || len(updated.State.Label) != 0 {
		t.Fatalf("update retained old values: %+v", updated.State)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodGet, base+"/"+personal.ID, nil), http.StatusNotFound)
	requireNativeStatus(t, f.request(t, alice, http.MethodDelete, base+"/"+personal.ID, nil), http.StatusNoContent)
	requireNativeStatus(t, f.request(t, alice, http.MethodGet, base+"/"+personal.ID, nil), http.StatusNotFound)
	if got := list(alice); len(got) != 1 || got[0].ID != projectView.ID {
		t.Fatalf("delete affected another view: %+v", got)
	}
	for _, query := range []string{"SELECT count(*) FROM hosted_work_view_values WHERE view_id=?", "SELECT count(*) FROM hosted_work_view_favorites WHERE view_id=?"} {
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), query, personal.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("delete %s: count=%d err=%v", query, count, err)
		}
	}
}

func TestSavedWorkViewValidation(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "alice", "member", "alice@example.test", "read", "")
	for _, test := range []struct {
		name   string
		change func(*savedWorkView)
	}{
		{"empty name", func(v *savedWorkView) { v.Name = " " }},
		{"long name", func(v *savedWorkView) { v.Name = strings.Repeat("x", 121) }},
		{"invalid scope", func(v *savedWorkView) { v.Scope = "public" }},
		{"invalid layout", func(v *savedWorkView) { v.State.View = "grid" }},
		{"invalid tab", func(v *savedWorkView) { v.State.Tab = "other" }},
		{"invalid sort", func(v *savedWorkView) { v.State.Sort = "other" }},
		{"invalid window", func(v *savedWorkView) { v.State.CompletedWindow = "forever" }},
		{"invalid author", func(v *savedWorkView) { v.State.CreatedBy = "other" }},
		{"oversized search", func(v *savedWorkView) { v.State.Q = strings.Repeat("x", 8193) }},
		{"oversized selection", func(v *savedWorkView) { v.State.Label = make([]string, 257) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := savedWorkView{Name: "View", Scope: "personal", State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
			test.change(&v)
			requireNativeStatus(t, f.request(t, user, http.MethodPost, "/api/v2/organizations/org_security/work-views", v), http.StatusUnprocessableEntity)
		})
	}
}
