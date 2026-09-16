package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		if !strings.Contains(request.Query, "              body\n") || !strings.Contains(request.Query, "... on ProjectV2 {\n      updatedAt") {
			t.Error("board query must retain scalar bodies and project revision")
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
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if strings.Contains(req.Query, "RefreshProjectRevision") {
					fmt.Fprint(w, `{"data":{"node":{"updatedAt":"2026-09-16T20:00:00Z","items":{"totalCount":2}}}}`)
					return
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
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"updatedAt": "2026-09-16T20:00:00Z", "items": page}}}); err != nil {
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
	for _, failure := range []string{"unsupported scheduler", "incomplete evidence", "graphql backoff"} {
		t.Run(failure, func(t *testing.T) {
			for _, count := range []int{1, 152} {
				t.Run(strconv.Itoa(count), func(t *testing.T) {
					var bodies, rest int
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
								switch failure {
								case "unsupported scheduler":
									fmt.Fprint(w, `{"errors":[{"message":"Cannot query field blockedBy on type Issue"}]}`)
								case "graphql backoff":
									http.Error(w, "rate limit", http.StatusTooManyRequests)
								default:
									fmt.Fprint(w, `{"data":{"issue0":{"id":"I1","body":"partial"}}}`)
								}
								return
							}
							page := projectItemsConnection{TotalCount: count}
							for n := 1; n <= count; n++ {
								node := &githubIssueNode{TypeName: "Issue", ID: fmt.Sprintf("I%d", n), Number: n, Body: body, Repository: repository{NameWithOwner: "owner/repo"}}
								node.Comments.TotalCount = 1
								page.Nodes = append(page.Nodes, projectItemNode{ID: fmt.Sprintf("P%d", n), StatusValue: &singleSelectValue{Name: "Todo"}, Content: node})
							}
							write(map[string]any{"data": map[string]any{"node": map[string]any{"items": page}}})
							return
						}
						rest++
						switch {
						case strings.HasSuffix(r.URL.Path, "/issues/1"):
							bodies++
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
					c.client.restPolicy.FanoutMaxRequests = 40
					result := c.FetchRefreshIssues(connector.WithRESTFanoutBudget(t.Context(), "refresh"), []string{"Todo"}, nil, connector.IssueFilterHint{})
					if bodies != 0 || rest > 40 {
						t.Fatalf("body fetches=%d REST=%d", bodies, rest)
					}
					if count == 152 {
						if !errors.Is(result.CandidateError, ErrRESTFanoutDeferred) || len(result.Candidates) != 0 || len(result.LaneSignalCandidates) != 0 {
							t.Fatalf("expected bounded, unpublished fallback: %+v", result)
						}
						return
					}
					if result.CandidateError != nil || len(result.Candidates) != 1 {
						t.Fatalf("result: %+v", result)
					}
					issue := result.Candidates[0]
					if issue.Description != body || issue.ModelOverride != "fixture-model" || len(issue.Comments) != 1 || issue.DependencySource != connector.BlockedRefSourceNative {
						t.Fatalf("incomplete fallback: %+v", issue)
					}
				})
			}
		})
	}
}

func TestRefreshConfiguredSchedulerStates(t *testing.T) {
	for _, tt := range []struct {
		name                       string
		active, observed, terminal []string
		state                      string
		enrich                     bool
	}{
		{"custom active", []string{"Queued"}, nil, []string{"Archived"}, "Queued", true},
		{"custom observed", []string{"Queued"}, []string{"Review"}, []string{"Archived"}, "Review", true},
		{"custom terminal", []string{"Queued"}, []string{"Archived"}, []string{"Archived"}, "Archived", false},
		{"unconfigured observed", []string{"Queued"}, []string{"Review"}, []string{"Archived"}, "Retired", false},
		{"default name repurposed", []string{"Done"}, nil, []string{"Archived"}, "Done", true},
		{"backlog repurposed", []string{"Queued"}, []string{"Backlog"}, []string{"Archived"}, "Backlog", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hydrated := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodPost {
					fmt.Fprint(w, `[]`)
					return
				}
				var req struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if strings.Contains(req.Query, "CandidateHydration") {
					hydrated = true
					fmt.Fprint(w, `{"data":{"issue0":{"id":"I1","body":"scheduler body","blockedBy":{"nodes":[]},"comments":{"totalCount":0}}}}`)
					return
				}
				page := projectItemsConnection{TotalCount: 1, Nodes: []projectItemNode{{ID: "P1", StatusValue: &singleSelectValue{Name: tt.state}, Content: &githubIssueNode{TypeName: "Issue", ID: "I1", Number: 1, Repository: repository{NameWithOwner: "owner/repo"}}}}}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"items": page}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo", ActiveStates: tt.active, ObservedStates: tt.observed, TerminalStates: tt.terminal})
			result := c.FetchRefreshIssues(t.Context(), nil, []string{tt.state}, connector.IssueFilterHint{})
			if result.CandidateError != nil || result.StatusError != nil || hydrated != tt.enrich {
				t.Fatalf("hydrated=%t want=%t result=%+v", hydrated, tt.enrich, result)
			}
		})
	}
}

func TestRefreshBoardChangedBetweenAttempts(t *testing.T) {
	for _, change := range []string{"revision", "count", "missing revision", "during resume"} {
		t.Run(change, func(t *testing.T) {
			phase, first, second := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				revision, total := "2026-09-16T20:00:00Z", 2
				if phase > 0 {
					if change != "count" {
						revision = "2026-09-16T20:01:00Z"
					}
					if change == "count" {
						total = 3
					}
				}
				if change == "missing revision" {
					revision = ""
				}
				page := projectItemsConnection{TotalCount: total}
				if strings.Contains(req.Query, "RefreshProjectRevision") {
					if change == "during resume" {
						revision = "2026-09-16T20:00:00Z"
					}
				} else {
					numbers := []int{1}
					if req.Variables["after"] == nil {
						first++
						if phase > 0 {
							numbers = []int{2}
						}
						page.PageInfo = pageInfo{HasNextPage: true, EndCursor: "second"}
					} else {
						second++
						if phase == 0 {
							http.Error(w, "fixture page failure", http.StatusBadGateway)
							return
						}
						numbers = []int{1}
						if change == "count" {
							numbers = []int{1, 3}
						}
					}
					for _, n := range numbers {
						page.Nodes = append(page.Nodes, projectItemNode{ID: fmt.Sprintf("P%d", n), StatusValue: &singleSelectValue{Name: "Done"}, Content: &githubIssueNode{TypeName: "Issue", ID: fmt.Sprintf("I%d", n), Number: n, Repository: repository{NameWithOwner: "owner/repo"}}})
					}
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"updatedAt": revision, "items": page}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
			fetch := func() connector.RefreshIssueResult {
				return c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Done"}, connector.IssueFilterHint{})
			}
			result := fetch()
			if result.CandidateError == nil || len(result.LaneSignalCandidates) != 0 {
				t.Fatalf("partial scan: %+v", result)
			}
			phase = 1
			result = fetch()
			if change == "during resume" {
				if !errors.Is(result.CandidateError, ErrProjectItemsTruncated) || len(result.LaneSignalCandidates) != 0 {
					t.Fatalf("changed resumed scan published: %+v", result)
				}
				if c.refreshScan.scan.BoardCounts != nil {
					t.Fatal("invalid progress retained")
				}
				phase = 2
				result = fetch()
			}
			want := 2
			if change == "count" {
				want = 3
			}
			ids := make(map[string]bool)
			for _, issue := range result.LaneSignalCandidates {
				ids[issue.ID] = true
			}
			if result.CandidateError != nil || result.StatusError != nil || len(ids) != want || !ids["I1"] || !ids["I2"] || first != 2 {
				t.Fatalf("first=%d second=%d ids=%v errors=(%v,%v)", first, second, ids, result.CandidateError, result.StatusError)
			}
		})
	}
}
