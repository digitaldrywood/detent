package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRefreshBoardQueryIsThin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		for _, field := range []string{"blockedBy(", "comments(first:", "closedByPullRequestsReferences(", "commits("} {
			if strings.Contains(request.Query, field) {
				t.Errorf("whole-board enumeration carries %s", field)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"node":{"items":{"totalCount":0,"nodes":[]}},"rateLimit":{"cost":1,"remaining":4999}}}`))
	}))
	defer server.Close()
	c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
	result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Done"}, connector.IssueFilterHint{})
	if result.CandidateError != nil || result.StatusError != nil {
		t.Fatalf("refresh: %+v", result)
	}
}

func TestRefreshBoardResumesFailedPage(t *testing.T) {
	for _, failure := range []string{"page error", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			var first, second int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Variables map[string]any }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				page := projectItemsConnection{TotalCount: 2}
				n := 1
				if req.Variables["after"] == nil {
					first++
					page.PageInfo = pageInfo{HasNextPage: true, EndCursor: "second"}
				} else {
					second++
					if req.Variables["after"] != "second" {
						t.Errorf("cursor: %v", req.Variables)
					}
					if second == 1 {
						http.Error(w, "fixture failure", http.StatusBadGateway)
						return
					}
					n = 2
				}
				page.Nodes = []projectItemNode{{ID: fmt.Sprintf("P%d", n), StatusValue: &singleSelectValue{Name: "Done"}, Content: &githubIssueNode{TypeName: "Issue", ID: fmt.Sprintf("I%d", n), Number: n, State: "CLOSED", Repository: repository{NameWithOwner: "owner/repo"}}}}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"items": page}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
			result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Done"}, connector.IssueFilterHint{})
			if result.CandidateError == nil || len(result.Statuses) != 0 || len(result.LaneSignalCandidates) != 0 {
				t.Fatalf("partial snapshot published: %+v", result)
			}
			if c.refreshScan.position.After != "second" {
				t.Fatalf("position: %+v", c.refreshScan.position)
			}
			if failure == "cancelled" {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				cancelled := c.FetchRefreshIssues(ctx, []string{"Todo"}, []string{"Done"}, connector.IssueFilterHint{})
				if cancelled.CandidateError == nil {
					t.Fatal("cancelled refresh succeeded")
				}
			}
			result = c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Done"}, connector.IssueFilterHint{})
			if result.CandidateError != nil || result.StatusError != nil || len(result.Statuses) != 2 || first != 1 || second != 2 {
				t.Fatalf("first=%d second=%d result=%+v", first, second, result)
			}
			if c.refreshScan.scan.BoardCounts != nil {
				t.Fatal("completed scan retained")
			}
		})
	}
}

func TestRefreshThinBodyFallback(t *testing.T) {
	for _, failure := range []string{"unsupported scheduler", "incomplete evidence"} {
		t.Run(failure, func(t *testing.T) {
			const body = "Task body\n<!-- model: fixture-model -->"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					if err := json.NewEncoder(w).Encode(v); err != nil {
						t.Error(err)
					}
				}
				if r.Method == http.MethodPost {
					var req struct{ Query string }
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if strings.Contains(req.Query, "CandidateHydration") {
						if failure == "unsupported scheduler" {
							fmt.Fprint(w, `{"errors":[{"message":"Cannot query field blockedBy on type Issue"}]}`)
						} else {
							fmt.Fprint(w, `{"data":{"issue0":{"id":"I1","body":"partial"}}}`)
						}
						return
					}
					write(map[string]any{"data": map[string]any{"node": map[string]any{"items": projectItemsConnection{Nodes: []projectItemNode{{ID: "P1", StatusValue: &singleSelectValue{Name: "Todo"}, Content: &githubIssueNode{TypeName: "Issue", ID: "I1", Number: 1, Repository: repository{NameWithOwner: "owner/repo"}}}}}}}})
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/issues/1"):
					write(map[string]any{"node_id": "I1", "number": 1, "body": body, "comments": 1, "state": "open"})
				case strings.HasSuffix(r.URL.Path, "/comments"):
					fmt.Fprint(w, `[{"id":1,"body":"Historical note"}]`)
				case strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by"), strings.HasSuffix(r.URL.Path, "/pulls"):
					fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
			result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, nil, connector.IssueFilterHint{})
			if result.CandidateError != nil || len(result.Candidates) != 1 {
				t.Fatalf("result: %+v", result)
			}
			issue := result.Candidates[0]
			if issue.Description != body || issue.ModelOverride != "fixture-model" || len(issue.Comments) != 1 || issue.DependencySource != connector.BlockedRefSourceNative {
				t.Fatalf("incomplete fallback: %+v", issue)
			}
		})
	}
}
