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

func TestRefreshHydrationResumes(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(fmt.Sprintf("edited=%t", edited), func(t *testing.T) {
			const stamp = "2026-09-16T20:00:00Z"
			failed, resumed := false, false
			counts := make(map[string]int)
			boardReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					fmt.Fprint(w, `[]`)
					return
				}
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				data := make(map[string]any)
				switch {
				case strings.Contains(req.Query, "RefreshProjectRevision"):
					data["node"] = map[string]any{"updatedAt": stamp}
				case strings.Contains(req.Query, "CandidateHydration"), strings.Contains(req.Query, "RefreshEvidenceRevision"):
					hydration := strings.Contains(req.Query, "CandidateHydration")
					if hydration && req.Variables["id0"] == "I26" && !failed {
						failed = true
						http.Error(w, "interrupted hydration", http.StatusBadGateway)
						return
					}
					for key, value := range req.Variables {
						if !strings.HasPrefix(key, "id") {
							continue
						}
						id := value.(string)
						if hydration {
							counts[id]++
						}
						comments := []any{}
						if id == "I1" {
							updated, body := stamp, "old comment"
							if resumed && edited {
								updated, body = "2026-09-16T20:01:00Z", "edited comment"
							}
							comments = append(comments, map[string]any{"id": "C1", "updatedAt": updated, "body": body})
						}
						data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": id, "updatedAt": stamp, "body": "body", "comments": map[string]any{"totalCount": len(comments), "nodes": comments}, "blockedBy": map[string]any{"nodes": []any{}}}
					}
				default:
					boardReads++
					items := []any{}
					for n := 1; n <= 30; n++ {
						items = append(items, map[string]any{"id": fmt.Sprintf("P%d", n), "statusValue": map[string]any{"name": "Todo"}, "content": map[string]any{"__typename": "Issue", "id": fmt.Sprintf("I%d", n), "number": n, "title": "issue", "state": "OPEN", "repository": map[string]any{"nameWithOwner": "fixture/resume"}}})
					}
					data["node"] = map[string]any{"updatedAt": stamp, "items": map[string]any{"totalCount": 30, "nodes": items}}
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/resume", ActiveStates: []string{"Todo"}})
			first := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, nil, connector.IssueFilterHint{})
			if first.CandidateError == nil || len(first.Candidates) > 0 {
				t.Fatalf("published interrupted refresh: %+v", first)
			}
			if counts["I1"] != 1 || counts["I25"] != 1 {
				t.Fatalf("first batch not retained: %v", counts)
			}
			resumed = true
			result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, nil, connector.IssueFilterHint{})
			if result.CandidateError != nil || len(result.Candidates) != 30 {
				t.Fatalf("resume: %+v", result)
			}
			for n := 1; n <= 30; n++ {
				id := fmt.Sprintf("I%d", n)
				want := 1
				if n == 1 && edited {
					want = 2
				}
				if counts[id] != want {
					t.Errorf("%s hydrated %d times want %d", id, counts[id], want)
				}
			}
			wantBody := "old comment"
			if edited {
				wantBody = "edited comment"
			}
			if got := result.Candidates[0].Comments; len(got) != 1 || got[0].Body != wantBody {
				t.Fatalf("comments: %+v", got)
			}
			if boardReads > 2 {
				t.Fatalf("replayed board %d times", boardReads)
			}
		})
	}
}

func TestCandidateHydrationShape(t *testing.T) {
	for _, fetch := range []bool{true, false} {
		t.Run(fmt.Sprintf("fetch=%t", fetch), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				requests++
				aliases := strings.Count(req.Query, ": node(id:")
				if aliases > 25 || aliases == 0 {
					t.Errorf("aliases=%d", aliases)
				}
				if nodes := strings.Count(req.Query, "comments(first:100")*100 + strings.Count(req.Query, "blockedBy(first:20")*(20+20*20); nodes > 13000 {
					t.Errorf("connection nodes=%d", nodes)
				}
				data := map[string]any{}
				for key, id := range req.Variables {
					if !strings.HasPrefix(key, "id") {
						continue
					}
					index := strings.TrimPrefix(key, "id")
					more := req.Variables["comments"+index] == nil
					commentID := "C2"
					if more {
						commentID = "C1"
					}
					data["issue"+index] = map[string]any{"id": id, "comments": map[string]any{"totalCount": 2, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": "next"}, "nodes": []any{map[string]any{"id": commentID}}}, "blockedBy": map[string]any{"nodes": []any{}}}
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/shape"})
			nodes := make([]githubIssueNode, 61)
			for i := range nodes {
				nodes[i].ID = fmt.Sprintf("I%d", i)
				if !fetch {
					if err := json.Unmarshal([]byte(`{"comments":{"totalCount":2,"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[{"id":"C1"}]},"blockedBy":{"nodes":[]}}`), &nodes[i]); err != nil {
						t.Fatal(err)
					}
				}
			}
			evidence, err := c.candidateEvidenceBatched(t.Context(), nodes, fetch)
			if err != nil || len(evidence) != 61 {
				t.Fatalf("evidence=%d err=%v", len(evidence), err)
			}
			want := 3
			if fetch {
				want = 6
			}
			if requests != want {
				t.Fatalf("requests=%d want=%d", requests, want)
			}
		})
	}
}

func TestCandidatePRStatusShape(t *testing.T) {
	for _, count := range []int{1, 20, 41} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				requests++
				aliases := strings.Count(req.Query, ": repository(")
				if aliases == 0 || aliases > 20 {
					t.Errorf("PR aliases=%d", aliases)
				}
				for _, field := range []string{"contexts(first:20)", "annotations(first:20)", "labels(first:50)", "reviews(first:50)", "comments(first:50)"} {
					if strings.Count(req.Query, field) != aliases {
						t.Errorf("unbounded %s", field)
					}
				}
				if aliases*(50+50+50+1+20+20*20) > 13000 {
					t.Error("PR connection bound exceeds 13000")
				}
				fmt.Fprint(w, `{"data":{}}`)
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/shape"})
			keys := make(map[pullRequestKey][]string)
			for n := 1; n <= count; n++ {
				keys[pullRequestKey{Repo: pullRequestRepo{Owner: "fixture", Name: "shape"}, Number: n}] = []string{fmt.Sprintf("I%d", n)}
			}
			c.observeCandidatePullRequestStatus(t.Context(), keys, make(map[string]githubIssueNode))
			if requests != (count+19)/20 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}

func TestRefreshCommentRevisionPagination(t *testing.T) {
	for _, scenario := range []string{"unchanged", "edited old comment", "missing timestamp", "repeated cursor", "request failure"} {
		t.Run(scenario, func(t *testing.T) {
			stamp := "2026-09-16T20:00:00Z"
			original := githubIssueNode{ID: "I1", UpdatedAt: &stamp}
			original.Comments.TotalCount = 101
			for i := range 101 {
				original.Comments.Nodes = append(original.Comments.Nodes, issueComment{ID: fmt.Sprintf("C%d", i), UpdatedAt: &stamp})
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if strings.Contains(req.Query, "body") || strings.Contains(req.Query, "blockedBy") {
					t.Error("revision check fetched expensive evidence")
				}
				if calls == 1 && req.Variables["after0"] != nil {
					t.Errorf("first cursor=%v", req.Variables["after0"])
				}
				if calls == 2 && req.Variables["after0"] != "next" {
					t.Errorf("continuation cursor=%v", req.Variables["after0"])
				}
				if calls == 2 && scenario == "request failure" {
					http.Error(w, "interrupted", http.StatusBadGateway)
					return
				}
				node := original
				node.Comments.Nodes = append([]issueComment(nil), original.Comments.Nodes...)
				if calls == 1 {
					node.Comments.Nodes = node.Comments.Nodes[:100]
					node.Comments.PageInfo = pageInfo{HasNextPage: true, EndCursor: "next"}
					if scenario == "edited old comment" {
						edited := "2026-09-16T20:01:00Z"
						node.Comments.Nodes[0].UpdatedAt = &edited
					}
					if scenario == "missing timestamp" {
						node.Comments.Nodes[0].UpdatedAt = nil
					}
				} else {
					node.Comments.Nodes = node.Comments.Nodes[100:]
					if scenario == "repeated cursor" {
						node.Comments.PageInfo = pageInfo{HasNextPage: true, EndCursor: "next"}
					}
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issue0": node}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/revision"})
			progress := projectItemsScanProgress{evidence: map[string]githubIssueNode{"I1": original}, hydrated: map[string]bool{"I1": true}}
			err := c.validateRefreshEvidence(t.Context(), &progress)
			wantErr := scenario == "repeated cursor" || scenario == "request failure"
			if (err != nil) != wantErr {
				t.Fatalf("error=%v", err)
			}
			wantRetained := scenario == "unchanged" || wantErr
			if progress.hydrated["I1"] != wantRetained || (len(progress.evidence) == 1) != wantRetained {
				t.Fatalf("retained=%t evidence=%d", progress.hydrated["I1"], len(progress.evidence))
			}
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
