package boardprojection_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func verifyHostedBoardKeysAndMCP(t *testing.T, f *hubfixture.Hosted, owner hubfixture.HostedUser, second tracker.ProjectID, issues map[tracker.ProjectID]tracker.NativeIssue) {
	t.Helper()
	reporter := f.User(t, "reporter", "reporter", "reporter@example.test", "read", "")
	f.Grant(t, reporter, second, false, false)
	var member hubtest.SeededMember
	if err := f.Repository.ReadTenant(t.Context(), f.Primary, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member.Member, err = r.AccessMember(t.Context(), owner.Identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expiry := f.Now().Add(time.Hour)
	readAll := hubtest.Credential(t, f.Repository, f.Primary, hubtest.CredentialOptions{Member: &member, KeyScope: "read", AllProject: true, ExpiresAt: &expiry}).Token
	selected := hubtest.Credential(t, f.Repository, f.Primary, hubtest.CredentialOptions{Member: &member, KeyScope: "write", Projects: []tracker.ProjectID{f.Project}, ExpiresAt: &expiry}).Token
	worker := hubtest.Credential(t, f.Repository, f.Primary, hubtest.CredentialOptions{Scope: "worker", KeyScope: "write", ExpiresAt: &expiry}).Token
	base := hubfixture.OrganizationBase(f.Primary) + "/work-board"
	compact := func(t *testing.T, response *httptest.ResponseRecorder) boardview.CompactBoard {
		t.Helper()
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var board boardview.CompactBoard
		hubfixture.Decode(t, response, &board)
		return board
	}

	allQuery := "format=compact&lanes=all"
	t.Run("read key sees every project", func(t *testing.T) {
		board := compact(t, f.Hub.Request(t, http.MethodGet, base+"?"+allQuery, readAll, nil))
		wantProjects := []tracker.ProjectID{f.Project, second}
		slices.Sort(wantProjects)
		if got := compactBoardProjectIDs(board); !slices.Equal(got, wantProjects) {
			t.Fatalf("projects = %v, want %v", got, wantProjects)
		}
		seen := map[tracker.ProjectID]bool{}
		for _, card := range board.Cards {
			issue, known := issues[card.ProjectID]
			if !known || seen[card.ProjectID] || card.ID != issue.WorkItemID || card.Identifier != fmt.Sprintf("%s#%d", issue.ProjectID, issue.Number) || card.Title != issue.Title || card.State != "Todo" {
				t.Fatalf("unexpected or repeated card: %+v", card)
			}
			seen[card.ProjectID] = true
		}
		for project := range issues {
			if !seen[project] {
				t.Errorf("board omitted the item in %s", project)
			}
		}
	})
	t.Run("selected key asking for two projects", func(t *testing.T) {
		query := url.Values{"format": {"compact"}, "project": {string(f.Project), string(second)}}
		board := compact(t, f.Hub.Request(t, http.MethodGet, base+"?"+query.Encode(), selected, nil))
		if !slices.Equal(compactBoardProjectIDs(board), []tracker.ProjectID{f.Project}) || !slices.Equal(board.Unavailable, []tracker.ProjectID{second}) {
			t.Fatalf("projects = %+v, unavailable = %v", board.Projects, board.Unavailable)
		}
	})
	t.Run("worker key refused", func(t *testing.T) {
		if response := f.Hub.Request(t, http.MethodGet, base, worker, nil); response.Code == http.StatusOK {
			t.Fatalf("worker key read the board: %s", response.Body)
		}
	})
	t.Run("reporter sees only own items", func(t *testing.T) {
		board := compact(t, f.Request(t, reporter, http.MethodGet, base+"?"+allQuery, nil))
		wantProjects := []tracker.ProjectID{f.Project, second}
		slices.Sort(wantProjects)
		if !slices.Equal(compactBoardProjectIDs(board), wantProjects) || len(board.Cards) != 0 {
			t.Fatalf("reporter projects = %+v, cards = %+v", board.Projects, board.Cards)
		}
	})
	t.Run("MCP matches HTTP", func(t *testing.T) {
		call := hubfixture.MCP(t, f.Handler(), hubfixture.MCPPath(f.Primary), readAll)
		list := call("tools/list", "", nil)
		var tools struct {
			Tools []struct{ Name string } `json:"tools"`
		}
		if err := json.Unmarshal(list.Result, &tools); err != nil {
			t.Fatalf("tools/list returned %s %s: %v", list.Result, list.Error, err)
		}
		hasBoard := false
		for _, tool := range tools.Tools {
			hasBoard = hasBoard || tool.Name == operatortool.WorkBoard
		}
		if !hasBoard {
			t.Fatal("work_board tool is unavailable for a read key")
		}
		for _, test := range []struct {
			name      string
			arguments map[string]any
			query     string
		}{
			{"all lanes", map[string]any{"lanes": "all"}, allQuery},
			{"selected project and lane", map[string]any{"lane": []string{"Todo"}, "project_ids": []tracker.ProjectID{second}}, url.Values{"format": {"compact"}, "lane": {"Todo"}, "project": {string(second)}}.Encode()},
			{"excluded lane", map[string]any{"lanes": "active", "exclude_lane": []string{"Todo"}}, "format=compact&lanes=active&exclude_lane=Todo"},
		} {
			t.Run(test.name, func(t *testing.T) {
				raw := hubfixture.MCPData(t, call("tools/call", operatortool.WorkBoard, test.arguments), false)
				var mcp boardview.CompactBoard
				if err := json.Unmarshal(raw, &mcp); err != nil {
					t.Fatal(err)
				}
				httpBoard := compact(t, f.Hub.Request(t, http.MethodGet, base+"?"+test.query, readAll, nil))
				if mcp.OrganizationID != httpBoard.OrganizationID || !slices.Equal(compactBoardProjectIDs(mcp), compactBoardProjectIDs(httpBoard)) ||
					!reflect.DeepEqual(mcp.Lanes, httpBoard.Lanes) || !reflect.DeepEqual(mcp.Cards, httpBoard.Cards) {
					t.Fatalf("MCP board and HTTP board differ: MCP %s, HTTP %+v", raw, httpBoard)
				}
			})
		}
		hubfixture.MCPData(t, call("tools/call", operatortool.WorkBoard, map[string]any{"lane": []string{"Nonexistent"}}), true)
	})
}

func compactBoardProjectIDs(board boardview.CompactBoard) []tracker.ProjectID {
	ids := make([]tracker.ProjectID, 0, len(board.Projects))
	for _, project := range board.Projects {
		ids = append(ids, project.ID)
	}
	slices.Sort(ids)
	return ids
}
