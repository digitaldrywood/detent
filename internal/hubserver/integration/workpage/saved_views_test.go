package workpage_test

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestSavedWorkViews(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	alice := f.User(t, "alice", "viewer", "alice@example.test", "read", "")
	bob := f.User(t, "bob", "member", "bob@example.test", "read", "")
	base := hubfixture.OrganizationBase(f.Primary) + "/work-views"
	state := boardview.SavedViewState{View: "list", Tab: "closed", Q: "a search", Sort: "updated", CompletedWindow: "7d", CompletedFrom: "2026-10-26T11:00:00Z", CompletedTo: "2026-10-27T11:00:00Z", Archived: true, CreatedBy: "me", State: []string{"Done"}, Type: []string{"bug"}, Label: []string{"label-a", "label-b"}, Assignee: []string{"alice", "bob"}, Priority: []string{"1", "2"}, Lanes: []string{}, Collapsed: []string{"Todo", "Backlog"}}
	create := func(user hubfixture.HostedUser, name, scope string, project *string) boardview.SavedView {
		t.Helper()
		ids := []string{}
		if project != nil {
			ids = append(ids, *project)
		}
		response := f.Request(t, user, http.MethodPost, base, boardview.SavedView{ProjectIDs: ids, Name: name, Scope: scope, ProjectID: project, Favorite: true, State: state})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var v boardview.SavedView
		hubfixture.Decode(t, response, &v)
		if !v.CanEdit || !reflect.DeepEqual(v.State, state) || v.OwnerID != user.Identity.Subject || !v.Favorite || v.Name != name {
			t.Fatalf("created view = %+v", v)
		}
		return v
	}
	requireVisible := func(user hubfixture.HostedUser, views ...boardview.SavedView) {
		t.Helper()
		response := f.Request(t, user, http.MethodGet, base, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page boardview.SavedViewPage
		hubfixture.Decode(t, response, &page)
		got := make([]string, 0, len(page.Views))
		for _, view := range page.Views {
			got = append(got, view.ID)
		}
		want := make([]string, 0, len(views))
		for _, view := range views {
			want = append(want, view.ID)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("visible views = %v, want %v", got, want)
		}
	}
	personal := create(alice, "Personal", "personal", nil)
	other := create(bob, "Other", "personal", nil)
	requireVisible(alice, personal)
	requireVisible(bob, other)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		hubfixture.RequireStatus(t, f.Request(t, bob, method, base+"/"+personal.ID, personal), http.StatusNotFound)
	}
	personal.Name = "Shared renamed"
	personal.Scope = "organization"
	response := f.Request(t, alice, http.MethodPut, base+"/"+personal.ID, personal)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	requireVisible(bob, personal, other)
	response = f.Request(t, bob, http.MethodGet, base+"/"+personal.ID, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var shared boardview.SavedView
	hubfixture.Decode(t, response, &shared)
	if shared.Favorite || shared.CanEdit || shared.Name != "Shared renamed" || !reflect.DeepEqual(shared.State, state) {
		t.Fatalf("shared view = %+v", shared)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		hubfixture.RequireStatus(t, f.Request(t, bob, method, base+"/"+personal.ID, personal), http.StatusNotFound)
	}
	hubfixture.RequireStatus(t, f.Request(t, bob, http.MethodPut, base+"/"+personal.ID+"/favorite", map[string]bool{"favorite": true}), http.StatusOK)
	hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodPut, base+"/"+personal.ID+"/favorite", map[string]bool{"favorite": false}), http.StatusOK)
	response = f.Request(t, bob, http.MethodGet, base+"/"+personal.ID, nil)
	hubfixture.Decode(t, response, &shared)
	if !shared.Favorite {
		t.Fatal("another user's favorite was removed")
	}
	project := string(f.Project)
	projectView := create(alice, "Project", "organization", &project)
	if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), bob.Identity.Subject, f.Project)
	}); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, f.Request(t, bob, http.MethodGet, base+"/"+projectView.ID, nil), http.StatusNotFound)
	requireVisible(bob, personal, other)
	hubfixture.RequireStatus(t, f.Request(t, bob, http.MethodPost, base, boardview.SavedView{ProjectIDs: []string{project}, Name: "Denied", Scope: "personal", ProjectID: &project, State: state}), http.StatusNotFound)
	config := hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider())
	config.Hosted.OrganizationID = "org_other"
	second := hubfixture.OpenHosted(t, config)
	charlie := second.User(t, "charlie", "member", "charlie@example.test", "", "")
	secondBase := hubfixture.OrganizationBase(second.Primary) + "/work-views"
	response = second.Request(t, charlie, http.MethodPost, secondBase, boardview.SavedView{ProjectIDs: []string{}, Name: "Other organization", Scope: "organization", State: state})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var secondView boardview.SavedView
	hubfixture.Decode(t, response, &secondView)
	hubfixture.RequireStatus(t, second.Request(t, charlie, http.MethodGet, secondBase+"/"+personal.ID, nil), http.StatusNotFound)
	hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodGet, base+"/"+secondView.ID, nil), http.StatusNotFound)
	hubfixture.RequireStatus(t, second.Request(t, alice, http.MethodGet, secondBase, nil), http.StatusForbidden)
	hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodGet, secondBase+"/"+personal.ID, nil), http.StatusNotFound)
	personal.State.Lanes = nil
	personal.State.Label = []string{}
	personal.Scope = "personal"
	response = f.Request(t, alice, http.MethodPut, base+"/"+personal.ID, personal)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var updated boardview.SavedView
	hubfixture.Decode(t, response, &updated)
	if updated.State.Lanes != nil || len(updated.State.Label) != 0 {
		t.Fatalf("update retained old values: %+v", updated.State)
	}
	hubfixture.RequireStatus(t, f.Request(t, bob, http.MethodGet, base+"/"+personal.ID, nil), http.StatusNotFound)
	hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodDelete, base+"/"+personal.ID, nil), http.StatusNoContent)
	hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodGet, base+"/"+personal.ID, nil), http.StatusNotFound)
	requireVisible(alice, projectView)

}

func TestSavedWorkViewValidation(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	user := f.User(t, "alice", "member", "alice@example.test", "read", "")
	for _, test := range []struct {
		name   string
		change func(*boardview.SavedView)
	}{
		{"empty name", func(v *boardview.SavedView) { v.Name = " " }},
		{"long name", func(v *boardview.SavedView) { v.Name = strings.Repeat("x", 121) }},
		{"missing working set", func(v *boardview.SavedView) { v.ProjectIDs = nil }},
		{"duplicate project", func(v *boardview.SavedView) { v.ProjectIDs = []string{"one", "one"} }},
		{"empty project", func(v *boardview.SavedView) { v.ProjectIDs = []string{""} }},
		{"focus outside working set", func(v *boardview.SavedView) { v.ProjectID = new("outside") }},
		{"oversized working set", func(v *boardview.SavedView) { v.ProjectIDs = make([]string, 101) }},
		{"invalid scope", func(v *boardview.SavedView) { v.Scope = "public" }},
		{"invalid layout", func(v *boardview.SavedView) { v.State.View = "grid" }},
		{"invalid tab", func(v *boardview.SavedView) { v.State.Tab = "other" }},
		{"invalid sort", func(v *boardview.SavedView) { v.State.Sort = "other" }},
		{"invalid window", func(v *boardview.SavedView) { v.State.CompletedWindow = "forever" }},
		{"invalid completed from", func(v *boardview.SavedView) { v.State.CompletedFrom = "invalid" }},
		{"invalid completed to", func(v *boardview.SavedView) { v.State.CompletedTo = "invalid" }},
		{"invalid author", func(v *boardview.SavedView) { v.State.CreatedBy = "other" }},
		{"oversized search", func(v *boardview.SavedView) { v.State.Q = strings.Repeat("x", 8193) }},
		{"oversized selection", func(v *boardview.SavedView) { v.State.Label = make([]string, 257) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := boardview.SavedView{ProjectIDs: []string{}, Name: "View", Scope: "personal", State: boardview.SavedViewState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
			test.change(&v)
			hubfixture.RequireStatus(t, f.Request(t, user, http.MethodPost, hubfixture.OrganizationBase(f.Primary)+"/work-views", v), http.StatusUnprocessableEntity)
		})
	}
}

func TestSavedWorkViewPagination(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	alice := f.User(t, "alice", "member", "alice@example.test", "read", "")
	bob := f.User(t, "bob", "member", "bob@example.test", "read", "")
	base := hubfixture.OrganizationBase(f.Primary) + "/work-views"
	view := boardview.SavedView{ProjectIDs: []string{}, Name: "View", Scope: "organization", State: boardview.SavedViewState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
	view.State.Label = make([]string, 24)
	for i := range view.State.Label {
		view.State.Label[i] = strings.Repeat("x", 512)
	}
	remaining := map[string]bool{}
	for range 26 {
		response := f.Request(t, alice, http.MethodPost, base, view)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var created boardview.SavedView
		hubfixture.Decode(t, response, &created)
		remaining[created.ID] = true
	}
	cursor := ""
	for {
		response := f.Request(t, bob, http.MethodGet, base+cursor, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page boardview.SavedViewPage
		hubfixture.Decode(t, response, &page)
		if cursor == "" && page.NextCursor == "" {
			t.Fatal("first page did not provide a continuation")
		}
		for _, got := range page.Views {
			if !remaining[got.ID] {
				t.Fatalf("unexpected or duplicate view %s", got.ID)
			}
			if got.CanEdit || !slices.Equal(got.State.Label, view.State.Label) {
				t.Fatalf("page changed ownership or selections for view %s", got.ID)
			}
			delete(remaining, got.ID)
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Views) == 0 {
			t.Fatal("empty page returned a continuation")
		}
		cursor = "?cursor=" + url.QueryEscape(page.NextCursor)
	}
	if len(remaining) != 0 {
		t.Fatalf("pagination omitted views: %v", remaining)
	}
	for _, query := range []string{"?cursor=a&cursor=b", "?limit=1000", "?cursor=" + strings.Repeat("x", 101)} {
		hubfixture.RequireStatus(t, f.Request(t, bob, http.MethodGet, base+query, nil), http.StatusUnprocessableEntity)
	}
	view.State.Q = strings.Repeat("\x01", 8192)
	view.State.Label = nil
	hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodPost, base, view), http.StatusUnprocessableEntity)
}

func TestSavedWorkViewQuotas(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	users := make([]hubfixture.HostedUser, 5)
	for i := range users {
		name := fmt.Sprintf("member-%d", i)
		users[i] = f.User(t, name, "member", name+"@example.test", "read", "")
	}
	base := hubfixture.OrganizationBase(f.Primary) + "/work-views"
	view := boardview.SavedView{ProjectIDs: []string{}, Name: "View", Scope: "organization", State: boardview.SavedViewState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
	first := make([]boardview.SavedView, 4)
	for i := range 4 {
		for j := range 50 {
			response := f.Request(t, users[i], http.MethodPost, base, view)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			if j == 0 {
				hubfixture.Decode(t, response, &first[i])
			}
		}
		hubfixture.RequireStatus(t, f.Request(t, users[i], http.MethodPost, base, view), http.StatusUnprocessableEntity)
		hubfixture.RequireStatus(t, f.Request(t, users[i], http.MethodGet, base+"/"+first[i].ID, nil), http.StatusOK)
	}
	hubfixture.RequireStatus(t, f.Request(t, users[4], http.MethodPost, base, view), http.StatusUnprocessableEntity)
	first[1].Name = "Renamed at capacity"
	hubfixture.RequireStatus(t, f.Request(t, users[1], http.MethodPut, base+"/"+first[1].ID, first[1]), http.StatusOK)
	hubfixture.RequireStatus(t, f.Request(t, users[0], http.MethodDelete, base+"/"+first[0].ID, nil), http.StatusNoContent)
	start := make(chan struct{})
	statuses := make(chan int, 2)
	for _, user := range []hubfixture.HostedUser{users[0], users[4]} {
		go func() {
			<-start
			statuses <- f.Request(t, user, http.MethodPost, base, view).Code
		}()
	}
	close(start)
	success := 0
	for range 2 {
		switch status := <-statuses; status {
		case http.StatusOK:
			success++
		case http.StatusConflict, http.StatusUnprocessableEntity:
		default:
			t.Fatalf("concurrent create status=%d", status)
		}
	}
	if success != 1 {
		t.Fatalf("last slot admitted %d creates", success)
	}
	hubfixture.RequireStatus(t, f.Request(t, users[1], http.MethodDelete, base+"/"+first[1].ID, nil), http.StatusNoContent)
	hubfixture.RequireStatus(t, f.Request(t, users[4], http.MethodPost, base, view), http.StatusOK)
	hubfixture.RequireStatus(t, f.Request(t, users[2], http.MethodGet, base+"/"+first[2].ID, nil), http.StatusOK)
}

func TestSavedWorkViewWorkingSets(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	alice := f.User(t, "alice", "member", "alice@example.test", "read", "")
	bob := f.User(t, "bob", "member", "bob@example.test", "read", "")
	project := string(f.Project)
	siblingProject := f.SeedProject(t, "prj_second", "Second project")
	second := string(siblingProject.ID)
	f.Grant(t, alice, siblingProject.ID, false, false)
	base := hubfixture.OrganizationBase(f.Primary) + "/work-views"
	siblingResponse := f.Request(t, alice, http.MethodPost, base, boardview.SavedView{Name: "Sibling", Scope: "personal", ProjectIDs: []string{second}, ProjectID: &second, State: boardview.SavedViewState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}})
	hubfixture.RequireStatus(t, siblingResponse, http.StatusOK)
	var sibling boardview.SavedView
	hubfixture.Decode(t, siblingResponse, &sibling)
	for _, test := range []struct {
		name  string
		ids   []string
		focus *string
	}{
		{"all", []string{}, nil},
		{"single", []string{project}, &project},
		{"multi focused", []string{second, project}, &second},
		{"multi all", []string{project, second}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := boardview.SavedView{Name: test.name, Scope: "organization", ProjectIDs: test.ids, ProjectID: test.focus, Favorite: true, State: boardview.SavedViewState{View: "list", Tab: "all", Sort: "title", CompletedWindow: "7d", Label: []string{"bug"}, Lanes: []string{"Todo"}, Collapsed: []string{"Done"}}}
			response := f.Request(t, alice, http.MethodPost, base, input)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var created boardview.SavedView
			hubfixture.Decode(t, response, &created)
			if !slices.Equal(created.ProjectIDs, test.ids) || !reflect.DeepEqual(created.ProjectID, test.focus) {
				t.Fatalf("created scope=%+v", created)
			}
			response = f.Request(t, alice, http.MethodGet, base+"/"+created.ID, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var read boardview.SavedView
			hubfixture.Decode(t, response, &read)
			if !reflect.DeepEqual(read, created) {
				t.Fatalf("read=%+v want=%+v", read, created)
			}
			if test.name == "multi focused" {
				hubfixture.RequireStatus(t, f.Request(t, bob, http.MethodGet, base+"/"+created.ID, nil), http.StatusNotFound)
				hubfixture.RequireStatus(t, f.Request(t, bob, http.MethodPost, base, input), http.StatusNotFound)
				response = f.Request(t, bob, http.MethodGet, base, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var page boardview.SavedViewPage
				hubfixture.Decode(t, response, &page)
				if len(page.Views) != 0 {
					t.Fatalf("inaccessible views listed: %+v", page.Views)
				}
			}
			created.ProjectIDs = []string{second}
			created.ProjectID = &second
			created.State.Sort = "updated"
			response = f.Request(t, alice, http.MethodPut, base+"/"+created.ID, created)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			hubfixture.Decode(t, response, &read)
			if !slices.Equal(read.ProjectIDs, []string{second}) || read.ProjectID == nil || *read.ProjectID != second || read.State.Sort != "updated" || !slices.Equal(read.State.Label, []string{"bug"}) || !read.Favorite {
				t.Fatalf("updated=%+v", read)
			}
			hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodDelete, base+"/"+created.ID, nil), http.StatusNoContent)
			hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodGet, base+"/"+created.ID, nil), http.StatusNotFound)
			hubfixture.RequireStatus(t, f.Request(t, alice, http.MethodGet, base+"/"+sibling.ID, nil), http.StatusOK)
		})
	}
}
