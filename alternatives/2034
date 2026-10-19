package hubserver

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
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

func TestSavedWorkViewBounds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		large bool
	}{{"count page", false}, {"byte page", true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			alice := f.user(t, "alice", "member", "alice@example.test", "read", "")
			bob := f.user(t, "bob", "member", "bob@example.test", "read", "")
			base := "/api/v2/organizations/org_security/work-views"
			view := savedWorkView{Name: "View", Scope: "organization", State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
			if test.large {
				view.State.Label = make([]string, 24)
				for i := range view.State.Label {
					view.State.Label[i] = strings.Repeat("x", 512)
				}
			}
			want := map[string]bool{}
			for range 26 {
				response := f.request(t, alice, http.MethodPost, base, view)
				requireNativeStatus(t, response, http.StatusOK)
				var created savedWorkView
				decodeHubResponse(t, response, &created)
				want[created.ID] = true
			}
			seen := map[string]bool{}
			cursor := ""
			for pageNumber := range 27 {
				response := f.request(t, bob, http.MethodGet, base+cursor, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if response.Body.Len() > 256<<10 {
					t.Fatalf("page is %d bytes", response.Body.Len())
				}
				var page struct {
					Views      []savedWorkView `json:"views"`
					NextCursor string          `json:"next_cursor"`
				}
				decodeHubResponse(t, response, &page)
				if len(page.Views) == 0 || len(page.Views) > 25 {
					t.Fatalf("page has %d views", len(page.Views))
				}
				if pageNumber == 0 && (page.NextCursor == "" || (test.large && len(page.Views) >= 25)) {
					t.Fatalf("page was not bounded: views=%d cursor=%q", len(page.Views), page.NextCursor)
				}
				for _, got := range page.Views {
					if !want[got.ID] || seen[got.ID] {
						t.Fatalf("unexpected or duplicate view %s", got.ID)
					}
					labels := view.State.Label
					if labels == nil {
						labels = []string{}
					}
					if got.CanEdit || !reflect.DeepEqual(got.State.Label, labels) {
						t.Fatal("page changed ownership or selections")
					}
					seen[got.ID] = true
				}
				if page.NextCursor == "" {
					break
				}
				cursor = "?cursor=" + url.QueryEscape(page.NextCursor)
			}
			if len(seen) != len(want) {
				t.Fatalf("read %d of %d views", len(seen), len(want))
			}
			for _, query := range []string{"?cursor=a&cursor=b", "?limit=1000", "?cursor=" + strings.Repeat("x", 101)} {
				requireNativeStatus(t, f.request(t, bob, http.MethodGet, base+query, nil), http.StatusUnprocessableEntity)
			}
		})
	}
	t.Run("payload", func(t *testing.T) {
		t.Parallel()
		f := newHostedSecurityFixture(t)
		alice := f.user(t, "alice", "member", "alice@example.test", "read", "")
		for _, test := range []struct {
			name  string
			state savedWorkState
		}{
			{"selections", savedWorkState{Label: func() []string {
				values := make([]string, 256)
				for i := range values {
					values[i] = strings.Repeat("x", 512)
				}
				return values
			}()}},
			{"escaped search", savedWorkState{Q: strings.Repeat("\x01", 8192)}},
		} {
			t.Run(test.name, func(t *testing.T) {
				state := test.state
				state.View = "board"
				state.Tab = "active"
				state.Sort = "default"
				state.CompletedWindow = "24h"
				response := f.request(t, alice, http.MethodPost, "/api/v2/organizations/org_security/work-views", savedWorkView{Name: "View", Scope: "organization", State: state})
				if response.Code != http.StatusUnprocessableEntity {
					t.Fatalf("oversized view status=%d, want 422", response.Code)
				}
			})
		}
	})
}

func TestSavedWorkViewQuotas(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	users := make([]hostedSecurityUser, 5)
	for i := range users {
		name := fmt.Sprintf("member-%d", i)
		users[i] = f.user(t, name, "member", name+"@example.test", "read", "")
	}
	base := "/api/v2/organizations/org_security/work-views"
	view := savedWorkView{Name: "View", Scope: "organization", State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
	first := make([]savedWorkView, 4)
	for i := range 4 {
		for j := range 50 {
			response := f.request(t, users[i], http.MethodPost, base, view)
			requireNativeStatus(t, response, http.StatusOK)
			if j == 0 {
				decodeHubResponse(t, response, &first[i])
			}
		}
		requireNativeStatus(t, f.request(t, users[i], http.MethodPost, base, view), http.StatusUnprocessableEntity)
		requireNativeStatus(t, f.request(t, users[i], http.MethodGet, base+"/"+first[i].ID, nil), http.StatusOK)
	}
	requireNativeStatus(t, f.request(t, users[4], http.MethodPost, base, view), http.StatusUnprocessableEntity)
	first[1].Name = "Renamed at capacity"
	requireNativeStatus(t, f.request(t, users[1], http.MethodPut, base+"/"+first[1].ID, first[1]), http.StatusOK)
	requireNativeStatus(t, f.request(t, users[0], http.MethodDelete, base+"/"+first[0].ID, nil), http.StatusNoContent)
	start := make(chan struct{})
	statuses := make(chan int, 2)
	for _, user := range []hostedSecurityUser{users[0], users[4]} {
		go func() {
			<-start
			statuses <- f.request(t, user, http.MethodPost, base, view).Code
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
	requireNativeStatus(t, f.request(t, users[1], http.MethodDelete, base+"/"+first[1].ID, nil), http.StatusNoContent)
	requireNativeStatus(t, f.request(t, users[4], http.MethodPost, base, view), http.StatusOK)
	requireNativeStatus(t, f.request(t, users[2], http.MethodGet, base+"/"+first[2].ID, nil), http.StatusOK)
}

func TestSavedWorkViewStorageScopes(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: hostedTestDatabasePath(t), GitHubDisabled: true})
	for _, user := range []string{"alice", "bob"} {
		if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, "token_"+user, user, apikey.HashToken("token_"+user), "", "operator", "now", "now"); err != nil {
			t.Fatal(err)
		}
		if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_members(user_id,email,membership_id,role,principal_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, user, user+"@example.test", "membership_"+user, "member", "token_"+user, "now", "now"); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_first", "org_second"} {
		if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)`, org, org, "now"); err != nil {
			t.Fatal(err)
		}
		for index, user := range []string{"alice", "bob"} {
			ownerSlot, orgSlot, err := service.savedWorkViewSlots(t.Context(), org, user)
			if err != nil || ownerSlot != 0 || orgSlot != index {
				t.Fatalf("%s/%s slots: owner=%d organization=%d err=%v", org, user, ownerSlot, orgSlot, err)
			}
			id := "view_" + org + "_" + user
			if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_work_views(id,organization_id,owner_id,name,scope,layout,tab,search,sort,completed_window,archived,created_by,all_lanes,owner_slot,organization_slot) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, org, user, id, "personal", "board", "active", "", "default", "24h", false, "", true, ownerSlot, orgSlot); err != nil {
				t.Fatal(err)
			}
			view, err := service.readSavedWorkView(t.Context(), org, user, id)
			if err != nil || !view.CanEdit || view.Name != id {
				t.Fatalf("view %s=%+v err=%v", id, view, err)
			}
		}
	}
}
