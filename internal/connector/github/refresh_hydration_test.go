package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
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
				addHydratedProjectFields(data, req.Variables)
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
				addHydratedProjectFields(data, req.Variables)
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
				for _, field := range []string{"contexts(first:100)", "annotations(first:100)", "labels(first:100)", "reviews(first:100)", "comments(first:100)"} {
					if strings.Count(req.Query, field) != aliases {
						t.Errorf("unbounded %s", field)
					}
				}
				if aliases*(100+100+100+1+100+100*100) > 500000 {
					t.Error("PR connection bound exceeds 500000")
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

// Empty refresh batches are valid for both candidate and observed-state reads.
func TestRefreshPullRequestsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name   string
		issues []connector.Issue
	}{
		{name: "nil"},
		{name: "empty", issues: []connector.Issue{}},
	} {
		for _, candidates := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/candidates=%t", tc.name, candidates), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("empty refresh made an unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}))
				t.Cleanup(server.Close)
				c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/empty"})
				if err := c.hydrateRefreshPullRequests(t.Context(), tc.issues, nil, candidates); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLabelRefreshSharesFreshSchedulerEvidence(t *testing.T) {
	const repo = "fixture/labels"
	const stamp = "2026-09-30T20:00:00Z"
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			reads := map[string]int{}
			phase := 0
			statuses := []string{"blocked", "in_progress", "complete"}
			commentUpdated := func() string { return fmt.Sprintf("2026-09-30T20:0%d:00Z", phase) }
			commentBody := func() string {
				action := "null"
				if phase == 0 {
					action = "Approve this work."
				}
				return fmt.Sprintf("## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: %s\nblockers: []\nhuman_action: %s\n```", statuses[phase], action)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					reads[r.URL.Path]++
					switch {
					case r.URL.Path == "/repos/fixture/labels/issues":
						label := r.URL.Query().Get("labels")
						start, end := 1, 30
						switch label {
						case "detent:blocked":
							start, end = 31, 31
						case "detent:backlog":
							start, end = 32, 51
						}
						rows := []any{}
						for n := start; n <= end; n++ {
							rows = append(rows, map[string]any{"node_id": fmt.Sprintf("I%d", n), "number": n, "state": "open", "body": "body", "updated_at": stamp, "comments": 1, "labels": []any{map[string]any{"name": label}}})
						}
						json.NewEncoder(w).Encode(rows)
					case strings.HasSuffix(r.URL.Path, "/comments"):
						body := fmt.Sprintf("answer%d", phase)
						if strings.HasSuffix(r.URL.Path, "/issues/31/comments") {
							body = commentBody()
						}
						json.NewEncoder(w).Encode([]any{map[string]any{"id": 1, "node_id": "C1", "body": body, "created_at": stamp, "updated_at": commentUpdated(), "author_association": "OWNER", "user": map[string]any{"login": "operator"}}})
					case strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by"), strings.HasSuffix(r.URL.Path, "/pulls"):
						fmt.Fprint(w, `[]`)
					default:
						t.Errorf("unexpected REST read: %s", r.URL)
						http.NotFound(w, r)
					}
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
				data := map[string]any{}
				switch {
				case strings.Contains(req.Query, "CandidateHydration"):
					reads["batch"]++
					if fallback {
						fmt.Fprint(w, `{"errors":[{"message":"fixture unavailable scheduler fields"}]}`)
						return
					}
					for key, value := range req.Variables {
						if !strings.HasPrefix(key, "id") {
							continue
						}
						id := value.(string)
						reads["evidence:"+id]++
						dependencies := []any{}
						if phase == 1 {
							dependencies = append(dependencies, map[string]any{"id": "D1", "number": 99, "state": "CLOSED", "repository": map[string]any{"nameWithOwner": repo}, "labels": map[string]any{"nodes": []any{}}})
						}
						body := fmt.Sprintf("answer%d", phase)
						if id == "I31" {
							body = commentBody()
						}
						data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": id, "body": "body", "updatedAt": stamp, "comments": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "body": body, "createdAt": stamp, "updatedAt": commentUpdated(), "author": map[string]any{"login": "operator"}, "authorAssociation": "OWNER"}}}, "blockedBy": map[string]any{"nodes": dependencies}}
					}
				case strings.Contains(req.Query, "CandidatePullRequestReferences"), strings.Contains(req.Query, "LabelIssuePullRequestReferences"):
					ids, ok := req.Variables["ids"].([]any)
					if !ok {
						ids, _ = req.Variables["issueIds"].([]any)
					}
					nodes := []any{}
					for _, id := range ids {
						var number int
						fmt.Sscanf(id.(string), "I%d", &number)
						label := "detent:todo"
						if id == "I31" {
							label = "detent:blocked"
						}
						references := []any{}
						if !fallback && id == "I31" {
							pr := candidatePRFixtureReference(repo, 31)
							pr["headRefOid"] = fmt.Sprintf("head%d", phase)
							references = append(references, pr)
						}
						nodes = append(nodes, map[string]any{"__typename": "Issue", "id": id, "number": number, "repository": map[string]any{"nameWithOwner": repo}, "timelineItems": map[string]any{"nodes": []any{map[string]any{"__typename": "LabeledEvent", "createdAt": stamp, "label": map[string]any{"name": label}, "actor": map[string]any{"__typename": "User", "login": "operator"}}}}, "closedByPullRequestsReferences": map[string]any{"totalCount": len(references), "nodes": references}})
					}
					data["nodes"] = nodes
					data["repo0"] = map[string]any{"pullRequests": map[string]any{}}
				case strings.Contains(req.Query, "CandidatePullRequestStatus"):
					reads["pr-status"]++
					for _, alias := range regexp.MustCompile(`(pr[0-9]+): repository`).FindAllStringSubmatch(req.Query, -1) {
						pr := candidatePRFixtureSnapshot(repo, 31)
						pr["headRefOid"] = fmt.Sprintf("head%d", phase)
						candidateFixtureCommit(pr)["oid"] = fmt.Sprintf("head%d", phase)
						candidateFixtureCommit(pr)["statusCheckRollup"] = nil
						candidateFixtureCommit(pr)["committedDate"] = stamp
						data[alias[1]] = map[string]any{"pullRequest": pr}
					}
				default:
					t.Errorf("unexpected query: %s", req.Query)
				}
				json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: repo, ActiveStates: []string{"Todo", "Blocked"}, ObservedStates: []string{"Todo", "Blocked", "Backlog"}, RESTFanoutMaxRequests: 500})
			if !c.CombinedRefreshEnabled() {
				t.Fatal("label refresh is not combined")
			}
			for phase = range 3 {
				result := c.FetchRefreshIssues(t.Context(), []string{"Todo", "Blocked"}, []string{"Todo", "Blocked", "Backlog"}, connector.IssueFilterHint{SchedulerStates: []string{"Blocked"}})
				if result.CandidateError != nil || result.StatusError != nil || len(result.Candidates) != 31 || len(result.Statuses) != 51 {
					t.Fatalf("refresh: %+v", result)
				}
				for _, issue := range result.Candidates {
					if len(issue.Comments) != 1 || (!fallback && issue.ID != "I31" && issue.Comments[0].Body != fmt.Sprintf("answer%d", phase)) || issue.DependencySource != connector.BlockedRefSourceNative {
						t.Fatalf("scheduler evidence: %+v", issue)
					}
				}
				issue := result.Candidates[30]
				signal := issue.WorkpadSignal
				if signal == nil || signal.Invalid != nil || signal.Status != statuses[phase] || signal.RecordedAt == nil || signal.RecordedAt.Format("2006-01-02T15:04:05Z") != commentUpdated() {
					t.Fatalf("same-comment edit retained previous Workpad authority: %+v", signal)
				}
				if issue.Comments[0].ID != "C1" || !issue.Comments[0].AuthorAuthorized || issue.Comments[0].Body != commentBody() || (signal.HumanAction != "") != (phase == 0) {
					t.Fatalf("same-comment human authority stale: %+v", issue)
				}
				if !fallback && len(result.Candidates[0].BlockedBy) != phase%2 {
					t.Fatalf("native dependencies stale: %+v", result.Candidates[0].BlockedBy)
				}
				blocked := result.Candidates[30]
				if !fallback && (blocked.PRNumber == nil || *blocked.PRNumber != 131 || blocked.PRHeadSHA != fmt.Sprintf("head%d", phase)) {
					t.Fatalf("PR identity/head stale: %+v", blocked)
				}
				if blocked.StageUpdatedAt == nil || blocked.StageUpdatedActor.Login != "operator" {
					t.Fatalf("lane evidence: %+v", blocked)
				}
				for _, issue := range result.Statuses[31:] {
					if len(issue.Comments) != 0 || issue.DependencySource != "" {
						t.Fatalf("metadata-only lane enriched: %+v", issue)
					}
				}
			}
			if reads["/repos/fixture/labels/issues"] != 9 {
				t.Fatalf("label lists repeated: %v", reads)
			}
			if !fallback {
				if reads["pr-status"] != 3 {
					t.Fatalf("overlapping PR status hydrated more than once: %v", reads)
				}
				if reads["batch"] != 6 {
					t.Fatalf("batch reads=%d want6", reads["batch"])
				}
				for n := 1; n <= 31; n++ {
					if reads[fmt.Sprintf("evidence:I%d", n)] != 3 {
						t.Fatalf("evidence repeated or stale: %v", reads)
					}
				}
				for endpoint := range reads {
					if strings.HasSuffix(endpoint, "/comments") || strings.HasSuffix(endpoint, "/dependencies/blocked_by") {
						t.Fatalf("complete batch used REST evidence: %v", reads)
					}
				}
			}
		})
	}
}
