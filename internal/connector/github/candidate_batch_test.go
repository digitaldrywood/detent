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

func TestCandidateColdRequestCounts(t *testing.T) {
	for _, mode := range []string{"board", "labels", "board fallback", "labels fallback"} {
		t.Run(mode, func(t *testing.T) {
			var graphql, rest int
			for project := range 10 {
				t.Run(strconv.Itoa(project), func(t *testing.T) {
					repo := fmt.Sprintf("fixture/project%d", project)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if r.Method == http.MethodPost {
							graphql++
							var request struct {
								Query     string         `json:"query"`
								Variables map[string]any `json:"variables"`
							}
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
								t.Error(err)
							}
							enhanced := strings.Contains(request.Query, "blockedBy(")
							if enhanced && strings.Contains(mode, "fallback") {
								fmt.Fprint(w, `{"errors":[{"message":"schema unavailable"}]}`)
								return
							}
							if strings.Contains(request.Query, "DetentGitHubCandidateHydration") {
								data := make(map[string]any)
								for i := range 3 {
									data[fmt.Sprintf("issue%d", i)] = map[string]any{"id": fmt.Sprintf("I%d", i+1), "body": "scheduler body", "comments": map[string]any{"totalCount": 1, "nodes": []map[string]any{{"id": "1", "body": "Historical note"}}}, "blockedBy": map[string]any{"nodes": []any{}}}
								}
								if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
									t.Error(err)
								}
								return
							}
							items := make([]string, 0, 3)
							for n := 1; n <= 3; n++ {
								items = append(items, fmt.Sprintf(`{"id":"P%d","content":{"__typename":"Issue","id":"I%d","number":%d,"title":"Candidate","body":"scheduler body","state":"OPEN","url":"https://github.com/%s/issues/%d","repository":{"nameWithOwner":%q},"comments":{"totalCount":1}},"statusValue":{"name":"Backlog"}}`, n, n, n, repo, n, repo))
							}
							body := projectItemsPageResponseWithTotal(3, false, "", items)
							if enhanced {
								body = strings.ReplaceAll(body, `"comments":{"totalCount":1}`, `"comments":{"totalCount":1,"nodes":[{"id":"1","body":"Historical note"}]},"blockedBy":{"nodes":[]}`)
							}
							fmt.Fprint(w, body)
							return
						}
						rest++
						switch {
						case strings.Contains(r.URL.Path, "/dependencies/blocked_by"):
							fmt.Fprint(w, `[]`)
						case strings.HasSuffix(r.URL.Path, "/comments"):
							fmt.Fprint(w, `[{"id":1,"body":"Historical note"}]`)
						default:
							items := make([]map[string]any, 0, 3)
							for n := 1; n <= 3; n++ {
								items = append(items, map[string]any{"node_id": fmt.Sprintf("I%d", n), "number": n, "title": "Candidate", "body": "scheduler body", "state": "open", "html_url": fmt.Sprintf("https://github.com/%s/issues/%d", repo, n), "comments": 1, "labels": []map[string]string{{"name": "detent:backlog"}}})
							}
							if err := json.NewEncoder(w).Encode(items); err != nil {
								t.Error(err)
							}
						}
					}))
					defer server.Close()
					cfg := Config{ProjectSlug: "PVT_1", Repository: repo, ActiveStates: []string{"Todo"}, ObservedStates: []string{"Backlog"}}
					if strings.HasPrefix(mode, "labels") {
						cfg.GitHubStatusSource = GitHubStatusSourceLabel
					}
					if !strings.Contains(mode, "fallback") {
						cfg.RESTFanoutMaxRequests = 1
					}
					c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, cfg)
					got, err := c.ReadCandidates(connector.WithRESTFanoutBudget(t.Context(), "candidates"), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Backlog"}, Limit: 10, PageSize: 10})
					if err != nil || len(got.Issues) != 3 || got.Truncated {
						t.Fatalf("result=%+v error=%v", got, err)
					}
					for _, issue := range got.Issues {
						if issue.DependencySource != connector.BlockedRefSourceNative || len(issue.BlockedBy) != 0 || issue.Description != "scheduler body" || len(issue.Comments) != 1 {
							t.Errorf("scheduler evidence=%+v", issue)
						}
					}
				})
			}
			wantREST, wantGraphQL := 0, 10
			switch mode {
			case "labels":
				wantREST = 10
			case "board fallback":
				wantREST = 60
				wantGraphQL = 20
			case "labels fallback":
				wantREST = 70
			}

			t.Logf("ten cold projects: GraphQL=%d REST=%d billable REST=%d", graphql, rest, rest)
			if graphql != wantGraphQL || rest != wantREST {
				t.Fatalf("want GraphQL=%d REST=%d", wantGraphQL, wantREST)
			}
		})
	}
}

func TestCandidateBatchedPaginationAndAuthority(t *testing.T) {
	for _, mode := range []string{"board", "labels"} {
		t.Run(mode, func(t *testing.T) {
			var graphql, rest int
			updated := "2026-09-15T01:02:03Z"
			body := "Depends on: other/repo#7\n<!-- model: fixture-model -->"
			comment := issueComment{ID: "C1", Body: "Depends on: owner/repo#98"}
			human := githubIssueNode{ID: "H", Number: 7, Body: strings.Replace(prerequisiteBody(t), "schema: 1", "schema: 1\ncompletion_evidence: Verified test tenant", 1), State: "CLOSED", Repository: repository{NameWithOwner: "other/repo"}}
			first := func(id string, number int) githubIssueNode {
				return githubIssueNode{TypeName: "Issue", ID: id, Number: number, Title: "Candidate", State: "OPEN", Body: body, UpdatedAt: &updated, Repository: repository{NameWithOwner: "owner/repo"}, Comments: nodeConnection[issueComment]{TotalCount: 2, Nodes: []issueComment{comment}, PageInfo: pageInfo{HasNextPage: true, EndCursor: "C1"}}, BlockedBy: &issueNodesConnection{Nodes: []githubIssueNode{human}, PageInfo: pageInfo{HasNextPage: true, EndCursor: "D1"}}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					if err := json.NewEncoder(w).Encode(v); err != nil {
						t.Error(err)
					}
				}
				if r.Method != http.MethodPost {
					rest++
					rows := []restIssue{{NodeID: "I1", Number: 1, State: "open", Body: &body, Labels: []label{{Name: "detent:backlog"}}}, {NodeID: "I2", Number: 2, State: "open", Body: &body, Labels: []label{{Name: "detent:backlog"}}}}
					write(rows)
					return
				}
				graphql++
				var request struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if strings.Contains(request.Query, "ProjectItems") {
					write(map[string]any{"data": map[string]any{"node": map[string]any{"items": projectItemsConnection{Nodes: []projectItemNode{{ID: "P1", Content: ptrCandidateNode(first("I1", 1)), StatusValue: &singleSelectValue{Name: "Backlog"}}, {ID: "P2", Content: ptrCandidateNode(first("I2", 2)), StatusValue: &singleSelectValue{Name: "Backlog"}}}}}}})
					return
				}
				data := map[string]any{}
				for i := range 2 {
					id := fmt.Sprintf("I%d", i+1)
					if request.Variables[fmt.Sprintf("id%d", i)] != id {
						t.Errorf("batch IDs: %v", request.Variables)
					}
					if request.Variables[fmt.Sprintf("comments%d", i)] == nil {
						data[fmt.Sprintf("issue%d", i)] = first(id, i+1)
						continue
					}
					if request.Variables[fmt.Sprintf("comments%d", i)] != "C1" || request.Variables[fmt.Sprintf("dependencies%d", i)] != "D1" {
						t.Errorf("cursors: %v", request.Variables)
					}
					node := first(id, i+1)
					node.Comments = nodeConnection[issueComment]{TotalCount: 2, Nodes: []issueComment{{ID: "C2", Body: "Latest note"}}}
					node.BlockedBy = &issueNodesConnection{Nodes: []githubIssueNode{{ID: "B", Number: 8, State: "OPEN", Repository: repository{NameWithOwner: "third/repo"}}}}
					data[fmt.Sprintf("issue%d", i)] = node
				}
				write(map[string]any{"data": data})
			}))
			defer server.Close()
			cfg := Config{ProjectSlug: "PVT_1", Repository: "owner/repo", ActiveStates: []string{"Todo"}, ObservedStates: []string{"Backlog"}}
			if mode == "labels" {
				cfg.GitHubStatusSource = GitHubStatusSourceLabel
			}
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, cfg)
			result, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Backlog"}, Limit: 10, PageSize: 10})
			if err != nil || len(result.Issues) != 2 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for _, issue := range result.Issues {
				if issue.Description != body || issue.UpdatedAt == nil || issue.UpdatedAt.Format("2006-01-02T15:04:05Z") != updated || issue.ModelOverride != "fixture-model" {
					t.Errorf("scheduler fields=%+v", issue)
				}
				if len(issue.Comments) != 2 || issue.Comments[1].ID != "C2" || len(issue.DependencyNotes) != 1 || len(issue.BlockedBy) != 2 || issue.DependencySource != connector.BlockedRefSourceNative {
					t.Fatalf("evidence=%+v", issue)
				}
				if blocker := issue.BlockedBy[0]; blocker.Identifier != "other/repo#7" || !blocker.HumanOwned || !blocker.HumanCompletionReady || blocker.State != "Done" {
					t.Errorf("human blocker=%+v", blocker)
				}
				if blocker := issue.BlockedBy[1]; blocker.Identifier != "third/repo#8" || blocker.State != "Open" {
					t.Errorf("cross repo blocker=%+v", blocker)
				}
			}
			wantREST := 0
			if mode == "labels" {
				wantREST = 1
			}
			if graphql != 2 || rest != wantREST {
				t.Errorf("GraphQL=%d REST=%d", graphql, rest)
			}
		})
	}
}

func ptrCandidateNode(node githubIssueNode) *githubIssueNode { return &node }

func TestCandidateEvidenceFallback(t *testing.T) {
	for _, test := range []struct{ name, response string }{
		{"rate limited", `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`},
		{"missing native relation", `{"data":{"issue0":{"id":"I1","comments":{"totalCount":0}}}}`},
		{"null issue", `{"data":{"issue0":null}}`},
		{"missing cursor", `{"data":{"issue0":{"id":"I1","blockedBy":{"pageInfo":{"hasNextPage":true}}}}}`},
		{"repeated cursor", `{"data":{"issue0":{"id":"I1","blockedBy":{"pageInfo":{"hasNextPage":true,"endCursor":"D1"}}}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var graphql, rest int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					graphql++
					fmt.Fprint(w, test.response)
					return
				}
				rest++
				if strings.HasSuffix(r.URL.Path, "/issues/90") {
					fmt.Fprint(w, `{"node_id":"B90","number":90,"state":"closed"}`)
					return
				}
				if strings.Contains(r.URL.Path, "/dependencies/") {
					fmt.Fprint(w, `[]`)
					return
				}
				if r.URL.Query().Get("page") == "2" {
					fmt.Fprint(w, `[{"id":2,"body":"Depends on: #92"}]`)
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?per_page=100&page=2>; rel="next"`, r.Host, r.URL.Path))
				fmt.Fprint(w, `[{"id":1,"body":"Depends on: #91"}]`)
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "owner/repo"})
			nodes := []githubIssueNode{{ID: "I1", BlockedBy: &issueNodesConnection{PageInfo: pageInfo{HasNextPage: true, EndCursor: "D1"}}}}
			evidence := c.candidateEvidence(t.Context(), nodes, false)
			if len(evidence) != 0 {
				t.Fatalf("incomplete evidence accepted: %v", evidence)
			}
			issue := connector.Issue{ID: "I1", Identifier: "owner/repo#1", State: "Backlog", Description: "Depends on: #90", CommentCount: 2}
			got, err := c.hydrateCandidateWithEvidence(t.Context(), issue, []string{"Backlog"}, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.BlockedBy) != 1 || got.BlockedBy[0].Identifier != "owner/repo#90" || got.DependencySource != connector.BlockedRefSourceNative || len(got.Comments) != 2 || len(got.DependencyNotes) != 2 {
				t.Fatalf("fallback evidence=%+v", got)
			}
			if graphql != 1 || rest != 4 {
				t.Fatalf("GraphQL=%d REST=%d", graphql, rest)
			}
		})
	}
}

func TestCandidateEvidenceIndependentConnections(t *testing.T) {
	for _, paginate := range []string{"comments", "dependencies"} {
		t.Run(paginate, func(t *testing.T) {
			original := githubIssueNode{ID: "I1", Comments: nodeConnection[issueComment]{TotalCount: 1, Nodes: []issueComment{{ID: "C1"}}}, BlockedBy: &issueNodesConnection{Nodes: []githubIssueNode{{ID: "B1"}}}}
			response := githubIssueNode{ID: "I1", BlockedBy: &issueNodesConnection{}}
			if paginate == "comments" {
				original.Comments.TotalCount = 2
				original.Comments.PageInfo = pageInfo{HasNextPage: true, EndCursor: "C1"}
				response.Comments = nodeConnection[issueComment]{TotalCount: 2, Nodes: []issueComment{{ID: "C2"}}}
			} else {
				original.BlockedBy.PageInfo = pageInfo{HasNextPage: true, EndCursor: "B1"}
				response.BlockedBy.Nodes = []githubIssueNode{{ID: "B2"}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue0": response}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{Repository: "owner/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			evidence := c.candidateEvidence(t.Context(), []githubIssueNode{original}, false)
			got, ok := evidence["I1"]
			if !ok {
				t.Fatal("missing complete evidence")
			}
			comments, dependencies := 1, 1
			if paginate == "comments" {
				comments = 2
			} else {
				dependencies = 2
			}
			if len(got.Comments.Nodes) != comments || len(got.BlockedBy.Nodes) != dependencies {
				t.Fatalf("comments=%v dependencies=%v", got.Comments, got.BlockedBy)
			}
		})
	}
}

func TestCandidateBoardBatchedCursor(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected REST %s", r.URL)
					http.Error(w, "unexpected REST", 500)
					return
				}
				calls++
				var request struct{ Variables map[string]any }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				numbers := []int{1, 2}
				page := projectItemsConnection{PageInfo: pageInfo{HasNextPage: true, EndCursor: "board-page-2"}}
				if request.Variables["after"] != nil {
					if request.Variables["after"] != "board-page-2" {
						t.Errorf("after=%v", request.Variables["after"])
					}
					numbers = []int{3}
					page.PageInfo = pageInfo{}
				}
				for _, number := range numbers {
					page.Nodes = append(page.Nodes, projectItemNode{ID: fmt.Sprintf("P%d", number), StatusValue: &singleSelectValue{Name: "Backlog"}, Content: &githubIssueNode{ID: fmt.Sprintf("I%d", number), TypeName: "Issue", Number: number, Repository: repository{NameWithOwner: "owner/repo"}, BlockedBy: &issueNodesConnection{}}})
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"items": page}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo"})
			request := connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Backlog"}, Limit: limit, PageSize: 2}
			var ids []string
			for range 3 {
				got, err := c.ReadCandidates(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, githubIssueIDs(got.Issues)...)
				if !got.Truncated {
					break
				}
				if got.NextCursor == "" || got.NextCursor == request.Cursor {
					t.Fatalf("cursor failed to advance: %+v", got)
				}
				request.Cursor = got.NextCursor
			}
			if strings.Join(ids, ",") != "I1,I2,I3" {
				t.Fatalf("IDs=%v", ids)
			}
			wantCalls := 2
			if limit == 1 {
				wantCalls = 3
			}
			if calls != wantCalls {
				t.Errorf("board calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}
