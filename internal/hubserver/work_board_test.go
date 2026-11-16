package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	t.Run("board", func(t *testing.T) {
		response := read(viewer, base, ids, state)
		requireNativeStatus(t, response, http.StatusOK)
		var view workBoardView
		decodeHubResponse(t, response, &view)
		if len(view.Projects) != 2 || len(view.Cards) != 2 || view.GrantVersion == "" {
			t.Fatalf("view=%+v", view)
		}
		seen := map[tracker.ProjectID]int{}
		for _, project := range view.Projects {
			if !slices.Contains(ids, string(project.Project.ID)) || project.Work == nil || project.Sequence == 0 || len(project.Project.States) == 0 {
				t.Fatalf("project=%+v", project)
			}
		}
		for _, card := range view.Cards {
			seen[card.Issue.ProjectID]++
			if card.Issue.OrganizationID != "org_security" || card.Issue.Terminal {
				t.Fatalf("card=%+v", card.Issue.NativeReference)
			}
		}
		if seen[second.ID] != 1 || seen[f.project] != 1 {
			t.Fatalf("cards by project=%v", seen)
		}
	})
	for _, test := range []struct {
		name   string
		change func(*savedWorkState)
	}{
		{"search", func(s *savedWorkState) { s.Q = "no matching title" }},
		{"list", func(s *savedWorkState) { s.View, s.Tab = "list", "all" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := state
			test.change(&selected)
			requireNativeStatus(t, read(viewer, base, ids, selected), http.StatusUnprocessableEntity)
		})
	}
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
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), second.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), viewer.identity.Subject, second.ID)
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, read(viewer, base, ids, state), http.StatusNotFound)
	requireNativeStatus(t, read(viewer, "/api/v2/organizations/org_other/work-board", ids, state), http.StatusNotFound)
}
