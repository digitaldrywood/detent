package runnerruntime_test

import (
	"net/http/httptest"
	"slices"

	"github.com/digitaldrywood/detent/internal/tracker"

	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceRecordRoutes(t *testing.T) {
	t.Parallel()
	clock := hubtest.NewClock(time.Now())
	hub := hubfixture.Open(t, hubfixture.Config{Clock: clock.Now, Workspace: &workspacehub.Config{Enabled: true}})

	t.Run("idempotency", func(t *testing.T) {
		project := hubfixture.NewNative(t, hub, "", "Workspace replay")
		issue := project.Create(t, "Replay subject")
		body := map[string]any{"idempotency_key": "open-once", "work_item_id": string(issue.WorkItemID), "ref": "main"}
		first := project.Request(t, http.MethodPost, project.Base+"/workspaces", body)
		hubfixture.RequireStatus(t, first, http.StatusCreated)
		replay := project.Request(t, http.MethodPost, project.Base+"/workspaces", body)
		hubfixture.RequireStatus(t, replay, http.StatusCreated)
		if replay.Body.String() != first.Body.String() {
			t.Fatalf("workspace replay changed response: %s", replay.Body.String())
		}
		if list := listWorkspaceRoutes(t, project, ""); len(list) != 1 {
			t.Fatalf("workspace replay opened %d sessions", len(list))
		}
		changed := maps.Clone(body)
		changed["ref"] = "release"
		hubfixture.RequireCode(t, project.Request(t, http.MethodPost, project.Base+"/workspaces", changed), http.StatusConflict, "idempotency_conflict")
	})

	t.Run("reads filter and scope", func(t *testing.T) {
		project := hubfixture.NewNative(t, hub, "", "Workspace filtering")
		firstIssue := project.Create(t, "First subject")
		secondIssue := project.Create(t, "Second subject")
		first := openWorkspaceRoute(t, project, string(firstIssue.WorkItemID))
		clock.Advance(time.Second)
		second := openWorkspaceRoute(t, project, string(secondIssue.WorkItemID))

		if list := listWorkspaceRoutes(t, project, ""); len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
			t.Fatalf("workspace list is out of order: %+v", list)
		}
		secondSubject := "?work_item=" + string(secondIssue.WorkItemID)
		for _, query := range []string{secondSubject, secondSubject + "&state=requested"} {
			if list := listWorkspaceRoutes(t, project, query); len(list) != 1 || list[0].ID != second.ID {
				t.Fatalf("workspace list for %s: %+v", query, list)
			}
		}
		if list := listWorkspaceRoutes(t, project, "?state=requested"); len(list) != 2 {
			t.Fatalf("requested workspaces: %+v", list)
		}
		for _, query := range []string{"?state=closed", secondSubject + "&state=closed"} {
			if list := listWorkspaceRoutes(t, project, query); len(list) != 0 {
				t.Fatalf("workspace list for %s: %+v", query, list)
			}
		}
		path := project.Base + "/workspaces"
		hubfixture.RequireStatus(t, project.Request(t, http.MethodGet, path+"/"+first.ID, nil), http.StatusOK)
		hubfixture.RequireCode(t, project.Request(t, http.MethodGet, path+"?state=paused", nil), http.StatusUnprocessableEntity, "invalid_request")

		foreign := hubfixture.NewNative(t, hub, project.Organization(), "Workspace foreign project")
		foreignIssue := foreign.Create(t, "Foreign subject")
		foreignSession := openWorkspaceRoute(t, foreign, string(foreignIssue.WorkItemID))
		for _, id := range []string{"ws_abcdef", foreignSession.ID} {
			hubfixture.RequireStatus(t, project.Request(t, http.MethodGet, path+"/"+id, nil), http.StatusNotFound)
		}
	})
}

func openWorkspaceRoute(t *testing.T, project hubfixture.Native, item string) workspacesession.Session {
	t.Helper()
	response := project.Request(t, http.MethodPost, project.Base+"/workspaces", map[string]string{"work_item_id": item, "idempotency_key": hubapi.NewID("open")})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	hubfixture.Decode(t, response, &session)
	return session
}

func listWorkspaceRoutes(t *testing.T, project hubfixture.Native, query string) []workspacesession.Session {
	t.Helper()
	response := project.Request(t, http.MethodGet, project.Base+"/workspaces"+query, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var listing workspacehub.ListResponse
	hubfixture.Decode(t, response, &listing)
	return listing.Workspaces
}

func TestWorkspaceOpenLimitsNameTheScopeReached(t *testing.T) {
	t.Parallel()
	hub := hubfixture.Open(t, hubfixture.Config{Workspace: &workspacehub.Config{Enabled: true, PersonMaxOpen: 2, PlanMaxOpen: 3}})
	project := hubfixture.NewNative(t, hub, "", "Workspace limits")
	issue := project.Create(t, "Workspace limit subject")
	other := hubtest.Credential(t, hub.Repository, project.Organization(), hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.Project.ID}, Now: hub.Now()})
	post := func(t *testing.T, token string) *httptest.ResponseRecorder {
		t.Helper()
		return hub.Request(t, http.MethodPost, project.Base+"/workspaces", token, map[string]string{"work_item_id": string(issue.WorkItemID), "idempotency_key": hubapi.NewID("open")})
	}
	for range 2 {
		hubfixture.RequireStatus(t, post(t, project.Token), http.StatusCreated)
	}

	t.Run("a person past their own cap is refused with scope person", func(t *testing.T) {
		requireWorkspaceLimit(t, post(t, project.Token), "person", 2)
	})

	t.Run("an organization past its plan cap is refused with scope organization", func(t *testing.T) {
		hubfixture.RequireStatus(t, post(t, other.Token), http.StatusCreated)
		requireWorkspaceLimit(t, post(t, other.Token), "organization", 3)
	})
}

func requireWorkspaceLimit(t *testing.T, response *httptest.ResponseRecorder, scope string, limit int) {
	t.Helper()
	hubfixture.RequireCode(t, response, http.StatusUnprocessableEntity, "workspace_limit")
	var failure hubapi.NativeError
	hubfixture.Decode(t, response, &failure)
	if failure.Details["scope"] != scope {
		t.Fatalf("details = %#v, want scope %s", failure.Details, scope)
	}
	if value, ok := failure.Details["limit"].(float64); !ok || int(value) != limit {
		t.Fatalf("details = %#v, want limit %d", failure.Details, limit)
	}
}

func TestNativeCapabilitiesAdvertiseWorkspaceSessionsOnlyWhenServed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		workspace *workspacehub.Config
		want      bool
	}{
		{name: "workspaces enabled", workspace: &workspacehub.Config{Enabled: true}, want: true},
		{name: "workspaces disabled", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{Workspace: test.workspace})
			response := f.Request(t, http.MethodGet, "/api/v2/capabilities", nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var document struct {
				Features []string `json:"features"`
			}
			hubfixture.Decode(t, response, &document)
			if got := slices.Contains(document.Features, tracker.NativeWorkspaceCapability); got != test.want {
				t.Fatalf("features = %v, advertise workspace sessions = %t, want %t", document.Features, got, test.want)
			}
		})
	}
}
