package runnerruntime_test

import (
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
		if list := listWorkspaceRoutes(t, project, secondSubject+"&state=closed"); len(list) != 0 {
			t.Fatalf("closed workspaces for subject: %+v", list)
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
