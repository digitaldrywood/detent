package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRefreshBoardFilterCost(t *testing.T) {
	for _, tt := range []struct{ total, owned int }{{9, 9}, {1991, 9}, {1991, 109}} {
		total, owned := tt.total, tt.owned
		t.Run(fmt.Sprintf("board_%d_owned_%d", total, owned), func(t *testing.T) {
			points, pages := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				count := total
				if strings.Contains(req.Query, "query: $filter") && req.Variables["filter"] == `label:"owned"` {
					count = owned
				}
				start := 0
				if after, ok := req.Variables["after"].(string); ok {
					start, _ = strconv.Atoi(after)
				}
				end := min(start+100, count)
				page := projectItemsConnection{TotalCount: count, PageInfo: pageInfo{HasNextPage: end < count, EndCursor: strconv.Itoa(end)}}
				for i := start; i < end; i++ {
					node := &githubIssueNode{TypeName: "Issue", ID: fmt.Sprintf("I%d", i), Number: i + 1, State: "CLOSED", Repository: repository{NameWithOwner: "owner/repo"}}
					if i < owned {
						node.Labels.Nodes = []label{{Name: "owned"}}
						node.Labels.TotalCount = 1
					}
					page.Nodes = append(page.Nodes, projectItemNode{ID: fmt.Sprintf("P%d", i), StatusValue: &singleSelectValue{Name: "Done"}, Content: node})
				}
				// GitHub's connection-cost estimate: root plus labels and assignees
				// per requested card, divided by 100 and rounded, minimum one point.
				cost := max(1, (1+2*int(req.Variables["first"].(float64))+50)/100)
				points += cost
				pages++
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"items": page}, "rateLimit": map[string]int{"cost": cost, "remaining": 5000 - points}}}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
			result := c.FetchRefreshIssues(t.Context(), nil, []string{"Done"}, connector.IssueFilterHint{LabelInclude: []string{"owned"}})
			if result.CandidateError != nil || result.StatusError != nil || len(result.Statuses) != owned {
				t.Fatalf("refresh: %+v", result)
			}
			wantPages := (owned + 99) / 100
			if points != 2*wantPages || pages != wantPages {
				t.Fatalf("board=%d owned=%d points=%d pages=%d; want %d points and %d pages", total, owned, points, pages, 2*wantPages, wantPages)
			}
		})
	}
}

func TestRefreshProjectFilter(t *testing.T) {
	for _, tt := range []struct {
		name string
		hint connector.IssueFilterHint
		want string
	}{
		{name: "empty"},
		{name: "authors stay local", hint: connector.IssueFilterHint{Authors: []string{"alice"}}},
		{name: "assignee alternatives", hint: connector.IssueFilterHint{Assignees: []string{"alice", "bob"}}, want: `assignee:"alice","bob"`},
		{name: "combined", hint: connector.IssueFilterHint{Assignees: []string{" alice "}, LabelInclude: []string{"owned", "needs review"}, LabelExclude: []string{"skip", "other"}}, want: `assignee:"alice" label:"owned" label:"needs review" -label:"skip" -label:"other"`},
		{name: "unsafe alternatives stay local", hint: connector.IssueFilterHint{Assignees: []string{"alice", "@me"}, LabelInclude: []string{"owned"}}, want: `label:"owned"`},
		{name: "syntax stays local", hint: connector.IssueFilterHint{LabelInclude: []string{`a"b`, `a,b`, `a*b`, `a\b`, "a\nb", ""}, LabelExclude: []string{"*"}}},
		{name: "blank values", hint: connector.IssueFilterHint{Assignees: []string{" ", "alice"}, LabelInclude: []string{" "}}, want: `assignee:"alice"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := refreshProjectFilter(tt.hint); got != tt.want {
				t.Fatalf("filter=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestFilteredRefreshExternalBlocker(t *testing.T) {
	for _, tt := range []struct {
		name, state, lane string
		human             bool
		want              string
	}{
		{"open custom lane", "OPEN", "Review", false, "Review"},
		{"human prerequisite", "OPEN", "Blocked", true, "Blocked"},
		{"closed blocker", "CLOSED", "Review", false, "Done"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			blockerReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				data := map[string]any{}
				switch {
				case strings.Contains(req.Query, "CandidatePullRequestReferences"):
					var nodes []any
					for _, id := range req.Variables["ids"].([]any) {
						nodes = append(nodes, map[string]any{"id": id, "closedByPullRequestsReferences": map[string]any{"totalCount": 0, "nodes": []any{}}})
					}
					data["nodes"] = nodes
					data["repo0"] = map[string]any{"pullRequests": map[string]any{"nodes": []any{}}}
				case strings.Contains(req.Query, "CandidateHydration"):
					labels := []any{}
					if tt.human {
						labels = append(labels, map[string]any{"name": "human-owned"})
					}
					for key, value := range req.Variables {
						if strings.HasPrefix(key, "id") {
							data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": value, "body": "Task", "comments": map[string]any{"totalCount": 0}, "blockedBy": map[string]any{"nodes": []any{map[string]any{"id": "I99", "number": 99, "state": tt.state, "repository": map[string]any{"nameWithOwner": "owner/repo"}, "labels": map[string]any{"nodes": labels}}}}}
						}
					}
					addSelectorFixtureProjectFields(data, req.Variables)
				case strings.Contains(req.Query, "projectItems("):
					blockerReads++
					if req.Variables["issueId"] != "I99" {
						t.Errorf("unexpected blocker: %v", req.Variables)
					}
					data["node"] = map[string]any{"projectItems": map[string]any{"nodes": []any{map[string]any{"id": "P99", "project": map[string]any{"id": "PVT_1"}, "statusValue": map[string]any{"name": tt.lane}}}}}
				case strings.Contains(req.Query, "items(first:"):
					if req.Variables["filter"] != `label:"owned"` {
						t.Errorf("filter: %v", req.Variables)
					}
					page := projectItemsConnection{TotalCount: 2}
					for n := 1; n <= 2; n++ {
						node := &githubIssueNode{TypeName: "Issue", ID: fmt.Sprintf("I%d", n), Number: n, State: "OPEN", Repository: repository{NameWithOwner: "owner/repo"}}
						node.Labels.Nodes = []label{{Name: "owned"}}
						node.Labels.TotalCount = 1
						page.Nodes = append(page.Nodes, projectItemNode{ID: fmt.Sprintf("P%d", n), StatusValue: &singleSelectValue{Name: "Blocked"}, Content: node})
					}
					data["node"] = map[string]any{"items": page}
				default:
					t.Errorf("unexpected query %s", req.Query)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
			result := c.FetchRefreshIssues(t.Context(), nil, []string{"Blocked"}, connector.IssueFilterHint{LabelInclude: []string{"owned"}, SchedulerStates: []string{"Blocked"}})
			if result.CandidateError != nil || result.StatusError != nil || len(result.Statuses) != 2 {
				t.Fatalf("refresh: %+v", result)
			}
			for _, issue := range result.Statuses {
				if len(issue.BlockedBy) != 1 || issue.BlockedBy[0].State != tt.want || issue.BlockedBy[0].HumanOwned != tt.human {
					t.Fatalf("blockers: %+v", issue.BlockedBy)
				}
			}
			wantReads := 1
			if tt.state == "CLOSED" {
				wantReads = 0
			}
			if blockerReads != wantReads {
				t.Fatalf("blocker reads=%d want=%d", blockerReads, wantReads)
			}
		})
	}
}

func TestRefreshFilterChangeRestartsInterruptedScan(t *testing.T) {
	phase := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if phase == 0 && req.Variables["after"] != nil {
			http.Error(w, "interrupted page", http.StatusBadGateway)
			return
		}
		if phase == 1 && req.Variables["after"] != nil {
			t.Error("changed selector reused old cursor")
		}
		labelName := "old"
		total := 2
		if phase == 1 {
			labelName = "new"
			total = 1
		}
		if req.Variables["filter"] != `label:"`+labelName+`"` {
			t.Errorf("filter=%v", req.Variables)
		}
		node := &githubIssueNode{TypeName: "Issue", ID: labelName, Number: phase + 1, State: "CLOSED", Repository: repository{NameWithOwner: "owner/repo"}}
		node.Labels.Nodes = []label{{Name: labelName}}
		node.Labels.TotalCount = 1
		page := projectItemsConnection{TotalCount: total, PageInfo: pageInfo{HasNextPage: phase == 0, EndCursor: "second"}, Nodes: []projectItemNode{{ID: "P" + labelName, StatusValue: &singleSelectValue{Name: "Done"}, Content: node}}}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"updatedAt": "stable", "items": page}}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
	result := c.FetchRefreshIssues(t.Context(), nil, []string{"Done"}, connector.IssueFilterHint{LabelInclude: []string{"old"}})
	if result.CandidateError == nil || len(result.Statuses) != 0 {
		t.Fatal("partial scan published")
	}
	phase = 1
	result = c.FetchRefreshIssues(t.Context(), nil, []string{"Done"}, connector.IssueFilterHint{LabelInclude: []string{"new"}})
	if result.CandidateError != nil || len(result.Statuses) != 1 || result.Statuses[0].ID != "new" {
		t.Fatalf("changed filter: %+v", result)
	}
}

func TestFilteredRefreshBlockerLookupFailure(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		wantError bool
	}{{"temporary", 503, false}, {"permanent", 401, true}} {
		t.Run(tt.name, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads++; http.Error(w, "lookup unavailable", tt.status) }))
			t.Cleanup(server.Close)
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
			issues := []connector.Issue{{ID: "I1", BlockedBy: []connector.BlockedRef{{ID: "I99", Identifier: "owner/repo#99", State: "Todo"}}}, {ID: "I2", BlockedBy: []connector.BlockedRef{{ID: "I99", Identifier: "owner/repo#99", State: "Todo"}}}, {ID: "unrelated"}}
			err := c.resolveFilteredRefreshBlockers(t.Context(), issues, nil)
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v wantError=%t", err, tt.wantError)
			}
			if !tt.wantError && (issues[0].BlockedBy[0].State != "" || issues[1].BlockedBy[0].State != "" || issues[2].ID != "unrelated") {
				t.Fatalf("unresolved dependencies: %+v", issues)
			}
			if reads != 1 {
				t.Fatalf("reads=%d want=1", reads)
			}
		})
	}
}
