package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestCandidateColdRequestCounts(t *testing.T) {
	prAlias := regexp.MustCompile(`(pr[0-9]+): repository\(owner:"fixture",name:"project"\) \{ pullRequest\(number:([0-9]+)\)`)
	for _, count := range []int{0, 1, 19, 20, 21, 40, 41} {
		t.Run(fmt.Sprintf("PR status/%d", count), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if !strings.Contains(request.Query, "CandidatePullRequestStatus") {
					t.Errorf("unexpected query: %s", request.Query)
				}
				if size := strings.Count(request.Query, "pullRequest(number:"); size > 20 {
					t.Errorf("PR batch size=%d exceeds 20", size)
				}
				w.Header().Set("Content-Type", "application/json")
				data := map[string]any{}
				for _, match := range prAlias.FindAllStringSubmatch(request.Query, -1) {
					n, err := strconv.Atoi(match[2])
					if err != nil {
						t.Error(err)
						return
					}
					snapshot := candidatePRFixtureSnapshot("fixture/project", n-100)
					candidateFixtureCommit(snapshot)["statusCheckRollup"] = nil
					data[match[1]] = map[string]any{"pullRequest": snapshot}
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{Repository: "fixture/project"})
			keys := make(map[pullRequestKey][]string, count)
			evidence := make(map[string]githubIssueNode, count)
			for n := range count {
				evidence[fmt.Sprintf("I%d", n)] = githubIssueNode{CandidatePR: &candidatePullRequestEvidence{}}
				keys[pullRequestKey{Repo: pullRequestRepo{Owner: "fixture", Name: "project"}, Number: n + 1}] = []string{fmt.Sprintf("I%d", n)}
			}
			c.observeCandidatePullRequestStatus(t.Context(), keys, evidence)
			for n := range count {
				observed := evidence[fmt.Sprintf("I%d", n)].CandidatePR
				if !observed.complete || observed.pullRequest == nil || observed.pullRequest.Number != n+1 {
					t.Errorf("PR %d not hydrated: %+v", n+1, observed)
				}
			}
			if want := (count + 19) / 20; requests != want {
				t.Fatalf("PR-status requests=%d want=%d", requests, want)
			}
		})
	}

	for _, mode := range []string{"board", "labels", "board fallback", "labels fallback"} {
		t.Run(mode, func(t *testing.T) {
			var graphql, rest int
			var points int64
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
							assertCandidateQueryShape(t, request.Query, request.Variables)
							if !strings.Contains(request.Query, "rateLimit {") || !strings.Contains(request.Query, "cost") {
								t.Error("missing request cost")
							}
							if strings.Contains(request.Query, "closedByPullRequestsReferences(") || strings.Contains(request.Query, "labels(first: 100)") {
								t.Error("expensive candidate preview")
							}
							enhanced := strings.Contains(request.Query, "blockedBy(")
							if enhanced && strings.Contains(mode, "fallback") {
								fmt.Fprint(w, `{"errors":[{"message":"Field 'blockedBy' doesn't exist on type 'Issue'"}]}`)
								return
							}
							if strings.Contains(request.Query, "DetentGitHubCandidateHydration") {
								data := map[string]any{"rateLimit": map[string]any{"cost": 1, "remaining": 4999}}
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
							// Measured on 2026-09-16: first:10 enriched board page
							// costs 2; the scalar fallback page costs 1.
							cost := 1
							if enhanced {
								cost = 2
							}
							fmt.Fprint(w, strings.Replace(body, `"data":{`, fmt.Sprintf(`"data":{"rateLimit":{"cost":%d,"remaining":4999},`, cost), 1))
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
					points += c.client.FlushGraphQLRateLimitUsage().TotalCost
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

			wantPoints := int64(10)
			if mode == "board" {
				wantPoints = 20
			}
			if mode == "labels fallback" {
				wantPoints = 0
			}
			if points != wantPoints {
				t.Errorf("GraphQL points=%d want=%d", points, wantPoints)
			}
			t.Logf("ten cold projects: GraphQL=%d REST=%d billable REST=%d", graphql, rest, rest)
			if graphql != wantGraphQL || rest != wantREST {
				t.Fatalf("want GraphQL=%d REST=%d", wantGraphQL, wantREST)
			}
		})
	}
}

func TestCandidateBatchedPaginationAndAuthority(t *testing.T) {
	for _, mode := range []string{"board", "labels", "refresh"} {
		t.Run(mode, func(t *testing.T) {
			lane := "Backlog"
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
				assertCandidateQueryShape(t, request.Query, request.Variables)
				if strings.Contains(request.Query, "ProjectItems") {
					write(map[string]any{"data": map[string]any{"node": map[string]any{"items": projectItemsConnection{Nodes: []projectItemNode{{ID: "P1", Content: ptrCandidateNode(first("I1", 1)), StatusValue: &singleSelectValue{Name: lane}}, {ID: "P2", Content: ptrCandidateNode(first("I2", 2)), StatusValue: &singleSelectValue{Name: lane}}}}}}})
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
				addHydratedProjectFields(data, request.Variables)
				write(map[string]any{"data": data})
			}))
			defer server.Close()
			cfg := Config{ProjectSlug: "PVT_1", Repository: "owner/repo", ActiveStates: []string{"Todo"}, ObservedStates: []string{lane}}
			if mode == "labels" {
				cfg.GitHubStatusSource = GitHubStatusSourceLabel
			}
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, cfg)
			var result connector.CandidateResult
			var err error
			if mode == "refresh" {
				refreshed := c.FetchRefreshIssues(t.Context(), []string{lane}, []string{lane}, connector.IssueFilterHint{})
				result.Issues, err = refreshed.Statuses, refreshed.StatusError
				if refreshed.CandidateError != nil {
					t.Fatal(refreshed.CandidateError)
				}
			} else {
				result, err = c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{lane}, Limit: 10, PageSize: 10})
			}
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
			if mode == "labels" || mode == "refresh" {
				wantREST = 1
			}
			wantGraphQL := 2
			if mode == "refresh" {
				wantGraphQL = 3
			}
			if graphql != wantGraphQL || rest != wantREST {
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
		{"truncated blocker labels", `{"data":{"issue0":{"id":"I1","comments":{"totalCount":0},"blockedBy":{"nodes":[{"id":"B1","labels":{"pageInfo":{"hasNextPage":true,"endCursor":"L20"}}}]}}}}`},
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
			// ReadCandidates resolves body refs after all candidate evidence is collected.
			resolved := []connector.Issue{got}
			if err := c.resolveBlockedByProjectState(t.Context(), resolved); err != nil {
				t.Fatal(err)
			}
			got = resolved[0]
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
				var request struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				assertCandidateQueryShape(t, request.Query, request.Variables)
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
				var request struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				assertCandidateQueryShape(t, request.Query, request.Variables)
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

func TestCandidateBodyDependencyRefresh(t *testing.T) {
	for _, mode := range []string{"shared prose", "native", "reopened", "closed", "expired", "snapshot", "rate limited"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
			var refresh, rest, fields int
			var logs bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					rest++
					if r.URL.Path != "/repos/owner/repo/issues/99" {
						t.Errorf("unexpected REST %s", r.URL.Path)
					}
					state := "closed"
					if (mode == "reopened" && refresh == 2) || (mode == "closed" && refresh == 1) {
						state = "open"
					}
					fmt.Fprintf(w, `{"node_id":"B99","number":99,"state":%q,"body":"","labels":[],"html_url":"https://github.com/owner/repo/issues/99"}`, state)
					return
				}
				var request struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				assertCandidateQueryShape(t, request.Query, request.Variables)
				if strings.Contains(request.Query, "projectItems(") {
					fields++
					if mode == "rate limited" {
						w.WriteHeader(http.StatusTooManyRequests)
						fmt.Fprint(w, `{"message":"rate limited"}`)
						return
					}
					state := "Done"
					if (mode == "reopened" && refresh == 2) || mode == "closed" {
						state = "Todo"
					}
					fmt.Fprintf(w, `{"data":{"node":{"projectItems":{"nodes":[{"id":"PB99","project":{"id":"PVT_1"},"statusValue":{"name":%q}}]}}}}`, state)
					return
				}
				refresh++
				items := []string{}
				for n := 1; n <= 3; n++ {
					body, native := "Depends on: #99", `[]`
					if n == 3 {
						body = ""
					}
					if mode == "native" && n < 3 {
						native = `[{"id":"B99","number":99,"state":"CLOSED","repository":{"nameWithOwner":"owner/repo"}}]`
					}
					items = append(items, fmt.Sprintf(`{"id":"P%d","content":{"__typename":"Issue","id":"I%d","number":%d,"title":"Candidate","body":%q,"state":"OPEN","repository":{"nameWithOwner":"owner/repo"},"comments":{"nodes":[]},"blockedBy":{"nodes":%s}},"statusValue":{"name":"Todo"}}`, n, n, n, body, native))
				}
				if mode == "snapshot" {
					items = append(items, `{"id":"PB99","content":{"__typename":"Issue","id":"B99","number":99,"state":"OPEN","repository":{"nameWithOwner":"owner/repo"},"comments":{"nodes":[]},"blockedBy":{"nodes":[]}},"statusValue":{"name":"Todo"}}`)
				}
				fmt.Fprint(w, projectItemsPageResponseWithTotal(len(items), false, "", items))
			}))
			t.Cleanup(server.Close)
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo", ActiveStates: []string{"Todo"}, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			c.projectCache = newProjectCache(time.Minute, func() time.Time { return now })
			passes := 2
			if mode == "rate limited" {
				passes = 1
			}
			for pass := 1; pass <= passes; pass++ {
				if mode == "expired" && pass == 2 {
					now = now.Add(2 * time.Minute)
				}
				got, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Todo"}, Limit: 10})
				wantLen := 3
				if mode == "snapshot" {
					wantLen = 4
				}
				if err != nil || len(got.Issues) != wantLen {
					t.Fatalf("pass %d: result=%+v err=%v", pass, got, err)
				}
				want := "Done"
				if mode == "snapshot" || (mode == "reopened" && pass == 2) || (mode == "closed" && pass == 1) {
					want = "Todo"
				}
				if mode == "rate limited" {
					want = ""
				}
				for _, issue := range got.Issues {
					if issue.ID == "I1" || issue.ID == "I2" {
						if len(issue.BlockedBy) != 1 || issue.BlockedBy[0].State != want {
							t.Fatalf("pass %d: blockers=%+v want %q", pass, issue.BlockedBy, want)
						}
					}
				}
			}
			wantREST, wantFields := 2, 1
			switch mode {
			case "native", "snapshot":
				wantREST, wantFields = 0, 0
			case "reopened", "expired":
				wantFields = 2
			case "rate limited":
				wantREST = 1
			}
			if rest != wantREST || fields != wantFields {
				t.Fatalf("REST=%d fields=%d; want %d/%d", rest, fields, wantREST, wantFields)
			}
			if mode == "rate limited" && strings.Count(logs.String(), "github blocked-by states unresolved") != 1 {
				t.Fatalf("want one aggregate warning: %s", logs.String())
			}
		})
	}
}

func assertCandidateQueryShape(t *testing.T, query string, variables map[string]any) {
	t.Helper()
	aliases := strings.Count(query, ": node(id:")
	if aliases > 25 {
		t.Errorf("scheduler aliases=%d exceeds 25", aliases)
	}
	compact := strings.ReplaceAll(query, " ", "")
	nodes := strings.Count(compact, "comments(first:100")*100 + strings.Count(compact, "blockedBy(first:20")*420
	if first, ok := variables["first"].(float64); ok && strings.Contains(query, "blockedBy(") {
		if first > 25 {
			t.Errorf("enriched board first=%v exceeds 25", first)
		}
		nodes *= int(first)
	}
	if nodes > 13000 {
		t.Errorf("scheduler connection nodes=%d exceeds 13000", nodes)
	}
}
