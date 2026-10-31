package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkBoardWorkingSet(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"name": "Second project", "states": nativeFixtureStates(), "idempotency_key": "second-project", "grant_access": true})
	requireNativeStatus(t, response, http.StatusCreated)
	var second tracker.NativeProject
	decodeHubResponse(t, response, &second)
	other := f
	other.project = second.ID
	other.base = "/api/v2/organizations/org_security/projects/" + string(second.ID)
	other.grant(t, owner, true, false)
	other.grant(t, viewer, false, false)
	for _, fixture := range []hostedSecurityFixture{f, other} {
		fixture.seedIssue(t, 1)
	}
	drainTestNativeBoard(t, f.service)
	base := "/api/v2/organizations/org_security/work-board"
	read := func(user hostedSecurityUser, path string, ids []string, state savedWorkState) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		params := url.Values{"project": ids, "state": {string(encoded)}}
		return f.request(t, user, http.MethodGet, path+"?"+params.Encode(), nil)
	}
	ids := []string{string(second.ID), string(f.project)}
	state := savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}
	for _, test := range []struct {
		name   string
		change func(*savedWorkState)
		count  int
	}{
		{"board", func(*savedWorkState) {}, 1},
		{"selected lane", func(s *savedWorkState) { s.Lanes = []string{"Todo"} }, 1},
		{"hidden lanes", func(s *savedWorkState) { s.Lanes = []string{} }, 0},
		{"state filter", func(s *savedWorkState) { s.State = []string{"Backlog"} }, 0},
		{"search", func(s *savedWorkState) { s.Q = "no matching title" }, 0},
		{"list", func(s *savedWorkState) { s.View, s.Tab = "list", "all" }, 1},
		{"completed from", func(s *savedWorkState) { s.View, s.Tab, s.CompletedFrom = "list", "all", "2026-10-26T00:00:00Z" }, 0},
		{"completed to", func(s *savedWorkState) { s.View, s.Tab, s.CompletedTo = "list", "all", "2026-10-27T00:00:00Z" }, 0},
		{"closed list", func(s *savedWorkState) { s.View, s.Tab = "list", "closed" }, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := state
			test.change(&selected)
			response := read(viewer, base, ids, selected)
			requireNativeStatus(t, response, http.StatusOK)
			var projects []workBoardProject
			decodeHubResponse(t, response, &projects)
			if len(projects) != 2 {
				t.Fatalf("projects=%d, want 2", len(projects))
			}
			for i, project := range projects {
				if string(project.Project.ID) != ids[i] || len(project.Issues) != test.count || len(project.Cards) != test.count || project.Work == nil || project.Sequence == 0 || project.GrantVersion == "" {
					t.Fatalf("project %d=%+v", i, project)
				}
				for _, issue := range project.Issues {
					if issue.ProjectID != project.Project.ID {
						t.Fatalf("project contains another project's issue: %+v", issue.NativeReference)
					}
				}
			}
		})
	}
	t.Run("list cursor continues through project API", func(t *testing.T) {
		for number := 2; number <= 101; number++ {
			f.seedIssue(t, number)
		}
		drainTestNativeBoard(t, f.service)
		selected := state
		selected.View, selected.Tab = "list", "all"
		response := read(viewer, base, []string{string(f.project)}, selected)
		requireNativeStatus(t, response, http.StatusOK)
		var projects []workBoardProject
		decodeHubResponse(t, response, &projects)
		if len(projects) != 1 || len(projects[0].Issues) != 100 || projects[0].NextCursor == "" {
			t.Fatalf("first page=%+v", projects)
		}
		response = f.request(t, viewer, http.MethodGet, f.base+"/work-items?include=board&completed_window=24h&limit=100&cursor="+url.QueryEscape(projects[0].NextCursor), nil)
		requireNativeStatus(t, response, http.StatusOK)
		var next tracker.NativeIssuePage
		decodeHubResponse(t, response, &next)
		if len(next.Items) != 1 || next.NextCursor != "" {
			t.Fatalf("continuation=%+v", next)
		}
	})
	for _, test := range []struct {
		name string
		ids  []string
	}{
		{"missing", nil},
		{"empty", []string{}},
		{"duplicate", []string{ids[0], ids[0]}},
		{"oversized", slices.Repeat(ids, 51)},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, read(viewer, base, test.ids, state), http.StatusUnprocessableEntity)
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE project_id=? AND user_id=?", second.ID, viewer.identity.Subject); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, read(viewer, base, ids, state), http.StatusNotFound)
	requireNativeStatus(t, read(viewer, "/api/v2/organizations/org_other/work-board", ids, state), http.StatusNotFound)
}
