package boardprojection_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedWorkBoardAuthority(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	owner := f.User(t, "owner", "owner", "owner@example.test", "write", "")
	viewer := f.User(t, "viewer", "viewer", "viewer@example.test", "read", "")
	second := f.SeedProject(t, "prj_second", "Second project")
	f.Grant(t, owner, second.ID, true, false)
	f.Grant(t, viewer, second.ID, false, false)
	issues := make(map[tracker.ProjectID]tracker.NativeIssue)
	for _, id := range []tracker.ProjectID{f.Project, second.ID} {
		response := f.Request(t, owner, http.MethodPost, hubfixture.ProjectBase(f.Primary, id)+"/work-items", tracker.CreateIssue{
			Mutation: tracker.Mutation{IdempotencyKey: "board-authority-" + string(id)}, Title: "Board item " + string(id),
			Body: hubfixture.IssueContractSections, State: "Todo",
		})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		hubfixture.Decode(t, response, &issue)
		issues[id] = issue
	}
	f.RefreshBoard(t, f.Primary)

	t.Run("working set", func(t *testing.T) {
		base := hubfixture.OrganizationBase(f.Primary) + "/work-board"
		read := func(path string, ids []string, state boardview.SavedViewState) *httptest.ResponseRecorder {
			t.Helper()
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			query := url.Values{"project": ids, "state": {string(encoded)}}
			return f.Request(t, viewer, http.MethodGet, path+"?"+query.Encode(), nil)
		}
		requireVisible := func(response *httptest.ResponseRecorder, want ...tracker.ProjectID) {
			t.Helper()
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var view struct {
				Projects []boardview.Project       `json:"projects"`
				Cards    []tracker.NativeBoardCard `json:"cards"`
			}
			hubfixture.Decode(t, response, &view)
			var ids []tracker.ProjectID
			for _, project := range view.Projects {
				ids = append(ids, project.Project.ID)
			}
			slices.Sort(ids)
			slices.Sort(want)
			if !slices.Equal(ids, want) {
				t.Fatalf("projects = %v, want %v", ids, want)
			}
			wantCards := make(map[tracker.ProjectID]tracker.NativeWorkItemID, len(want))
			for _, id := range want {
				wantCards[id] = issues[id].WorkItemID
			}
			cards := make(map[tracker.ProjectID]tracker.NativeWorkItemID, len(view.Cards))
			for _, card := range view.Cards {
				issue := card.Issue
				if _, repeated := cards[issue.ProjectID]; repeated || issue.OrganizationID != f.Primary || issue.Terminal {
					t.Fatalf("unexpected board item: %+v", issue.NativeReference)
				}
				cards[issue.ProjectID] = issue.WorkItemID
			}
			if !maps.Equal(cards, wantCards) {
				t.Fatalf("cards = %v, want %v", cards, wantCards)
			}
		}
		ids := []string{string(second.ID), string(f.Project)}
		state := boardview.SavedViewState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}
		requireVisible(read(base, ids, state), f.Project, second.ID)
		requireVisible(read(base, nil, state), f.Project, second.ID)
		search := state
		search.Q = "no matching title"
		hubfixture.RequireStatus(t, read(base, ids, search), http.StatusUnprocessableEntity)
		list := state
		list.View, list.Tab = "list", "all"
		hubfixture.RequireStatus(t, read(base, ids, list), http.StatusUnprocessableEntity)
		hubfixture.RequireStatus(t, read(base, []string{ids[0], ids[0]}, state), http.StatusUnprocessableEntity)
		hubfixture.RequireStatus(t, read(base, slices.Repeat(ids, 251), state), http.StatusUnprocessableEntity)

		if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteAccessProjectGrant(t.Context(), viewer.Identity.Subject, second.ID)
		}); err != nil {
			t.Fatal(err)
		}
		requireVisible(read(base, ids, state), f.Project)
		hubfixture.RequireStatus(t, read(base, []string{string(second.ID)}, state), http.StatusNotFound)
		hubfixture.RequireStatus(t, read(hubfixture.OrganizationBase("org_other")+"/work-board", ids, state), http.StatusNotFound)
	})
	t.Run("keys and MCP", func(t *testing.T) {
		verifyHostedBoardKeysAndMCP(t, f, owner, second.ID, issues)
	})
}
