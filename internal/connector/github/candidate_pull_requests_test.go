package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

// The legacy arm exercises the pre-batch detail path against identical source
// data. No fixture sends ETags, so each counted REST read is billable.
func TestCandidatePRRepeatedRefreshCounts(t *testing.T) {
	for _, mode := range []string{"board", "labels", "board fallback"} {
		for _, legacy := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/legacy=%t", mode, legacy), func(t *testing.T) {
				var lists, details, graphql, fallbacks int
				for project := range 10 {
					repo := fmt.Sprintf("fixture/project%d", project)
					var logs bytes.Buffer
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						write := func(v any) {
							t.Helper()
							if err := json.NewEncoder(w).Encode(v); err != nil {
								t.Error(err)
							}
						}
						if r.Method == http.MethodPost {
							graphql++
							var req struct {
								Query     string
								Variables map[string]any
							}
							if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
								t.Error(err)
								return
							}
							switch {
							case strings.Contains(req.Query, "CandidatePullRequestReferences"):
								if legacy || strings.Contains(mode, "fallback") {
									write(map[string]any{"errors": []map[string]string{{"message": "legacy fixture"}}})
									return
								}
								nodes := []map[string]any{}
								for n := 1; n <= 3; n++ {
									nodes = append(nodes, map[string]any{"id": fmt.Sprintf("I%d", n), "closedByPullRequestsReferences": map[string]any{"totalCount": 1, "nodes": []any{candidatePRFixtureReference(repo, n)}}})
								}
								write(map[string]any{"data": map[string]any{"nodes": nodes, "repo0": map[string]any{"pullRequests": map[string]any{"nodes": []any{}}}}})
							case strings.Contains(req.Query, "CandidatePullRequestStatus"):
								data := map[string]any{}
								for n := 1; n <= 3; n++ {
									data[fmt.Sprintf("pr%d", n-1)] = map[string]any{"pullRequest": candidatePRFixtureSnapshot(repo, n)}
								}
								write(map[string]any{"data": data})
							case strings.Contains(req.Query, "LabelIssuePullRequestReferences"):
								nodes := []map[string]any{}
								for _, id := range req.Variables["issueIds"].([]any) {
									var n int
									fmt.Sscanf(id.(string), "I%d", &n)
									nodes = append(nodes, map[string]any{"__typename": "Issue", "id": id, "number": n, "repository": map[string]any{"nameWithOwner": repo}, "closedByPullRequestsReferences": map[string]any{"nodes": []any{candidatePRFixtureReference(repo, n)}}})
								}
								write(map[string]any{"data": map[string]any{"nodes": nodes}})
							case strings.Contains(req.Query, "CandidateHydration"):
								data := map[string]any{}
								for n := 1; n <= 3; n++ {
									data[fmt.Sprintf("issue%d", n-1)] = candidatePRFixtureIssue(repo, n)
								}
								write(map[string]any{"data": data})
							default:
								items := []map[string]any{}
								for n := 1; n <= 3; n++ {
									items = append(items, map[string]any{"id": fmt.Sprintf("P%d", n), "content": candidatePRFixtureIssue(repo, n), "statusValue": map[string]string{"name": "Human Review"}})
								}
								write(map[string]any{"data": map[string]any{"node": map[string]any{"items": map[string]any{"nodes": items}}}})
							}
							return
						}
						switch {
						case r.URL.Path == "/repos/"+repo+"/issues":
							lists++
							rows := []map[string]any{}
							for n := 1; n <= 3; n++ {
								rows = append(rows, map[string]any{"node_id": fmt.Sprintf("I%d", n), "number": n, "state": "open", "title": "Candidate", "body": "body", "labels": []map[string]string{{"name": "detent:human-review"}}})
							}
							write(rows)
						case strings.Contains(r.URL.Path, "/pulls/"):
							details++
							if strings.HasSuffix(r.URL.Path, "/reviews") {
								write([]any{})
								return
							}
							var number int
							fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/"+repo+"/pulls/"), "%d", &number)
							write(map[string]any{"number": number, "state": "open", "head": map[string]string{"sha": fmt.Sprintf("head%d", number-100), "ref": "branch"}, "base": map[string]string{"sha": "base", "ref": "main"}})
						case strings.HasSuffix(r.URL.Path, "/check-runs"):
							details++
							write(map[string]any{"check_runs": []map[string]any{{"id": 1, "name": "unit", "status": "completed", "conclusion": "success", "created_at": "2026-09-15T00:00:00Z", "started_at": "2026-09-15T00:00:01Z", "completed_at": "2026-09-15T00:00:02Z"}}})
						case strings.HasSuffix(r.URL.Path, "/statuses"), strings.HasSuffix(r.URL.Path, "/comments"):
							details++
							write([]any{})
						default:
							t.Errorf("unexpected request %s", r.URL)
							w.WriteHeader(http.StatusNotFound)
						}
					}))
					cfg := Config{ProjectSlug: "PVT_1", Repository: repo, ActiveStates: []string{"Todo"}, ObservedStates: []string{"Human Review"}}
					if mode == "labels" {
						cfg.GitHubStatusSource = GitHubStatusSourceLabel
					}
					c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, cfg)
					c.logger = slog.New(slog.NewTextHandler(&logs, nil))
					for range 3 {
						var result connector.CandidateResult
						var err error
						if legacy {
							result, err = legacyCandidatePRRefresh(t, c, mode)
						} else {
							result, err = c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Human Review"}, Limit: 10, PageSize: 10})
						}
						if err != nil || len(result.Issues) != 3 || result.Truncated {
							t.Fatalf("result=%+v err=%v", result, err)
						}
						for _, issue := range result.Issues {
							if issue.PullRequest == nil || issue.PullRequest.CIStatus != "pass" {
								t.Fatalf("missing PR status: %+v", issue.PullRequest)
							}
						}
					}
					fallbacks += strings.Count(logs.String(), "github candidate PR detail fallback")
					server.Close()
				}
				wantLists := 0
				if mode == "labels" {
					wantLists = 30
				}
				wantDetails, wantFallbacks := 60, 30
				if legacy {
					wantDetails, wantFallbacks = 210, 0
				}
				if !legacy && strings.Contains(mode, "fallback") {
					wantDetails = 450
				}
				t.Logf("10 projects x 3 refreshes x 3 candidates: REST lists=%d PR detail=%d GraphQL=%d fallback=%d", lists, details, graphql, fallbacks)
				if lists != wantLists || details != wantDetails || fallbacks != wantFallbacks {
					t.Fatalf("want lists=%d details=%d fallback=%d", wantLists, wantDetails, wantFallbacks)
				}
			})
		}
	}
}

func candidatePRFixtureReference(repo string, n int) map[string]any {
	return map[string]any{"number": 100 + n, "state": "OPEN", "headRefOid": fmt.Sprintf("head%d", n), "repository": map[string]string{"nameWithOwner": repo}}
}
func candidatePRFixtureIssue(repo string, n int) map[string]any {
	return map[string]any{"__typename": "Issue", "id": fmt.Sprintf("I%d", n), "number": n, "state": "OPEN", "title": "Candidate", "body": "body", "repository": map[string]string{"nameWithOwner": repo}, "comments": map[string]any{"totalCount": 0, "nodes": []any{}}, "blockedBy": map[string]any{"nodes": []any{}}, "closedByPullRequestsReferences": map[string]any{"nodes": []any{candidatePRFixtureReference(repo, n)}}}
}
func candidatePRFixtureSnapshot(repo string, n int) map[string]any {
	return map[string]any{"id": fmt.Sprintf("PR%d", n), "number": 100 + n, "state": "OPEN", "headRefOid": fmt.Sprintf("head%d", n), "baseRefOid": "base", "headRefName": "branch", "baseRefName": "main", "labels": map[string]any{"totalCount": 0, "nodes": []any{}}, "reviews": map[string]any{"totalCount": 0, "nodes": []any{}}, "comments": map[string]any{"totalCount": 0, "nodes": []any{}}, "commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": fmt.Sprintf("head%d", n), "statusCheckRollup": map[string]any{"contexts": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"__typename": "CheckRun", "databaseId": 1, "name": "unit", "status": "COMPLETED", "conclusion": "SUCCESS"}}}}}}}}}
}

func TestCandidatePRCacheRevision(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			now := time.Now()
			cache := newPullRequestStatusCache(time.Second, func() time.Time { return now })
			repo := pullRequestRepo{Owner: "a", Name: "b"}
			revision := [32]byte{1}
			cache.SetObserved(repo, 1, "head", revision, pullRequestStatus{ci: pullRequestCI{State: "SUCCESS"}})
			now = now.Add(time.Hour)
			if changed {
				revision[0]++
			}
			_, ok := cache.GetObserved(repo, 1, "head", revision)
			if ok == changed {
				t.Fatalf("reuse=%t changed=%t", ok, changed)
			}
			cache.Set(repo, 1, "head", pullRequestStatus{})
			if _, ok := cache.GetObserved(repo, 1, "head", revision); ok {
				t.Fatal("REST-only status retained observation revision")
			}
		})
	}
}

// Reproduce #2761's path: one source page, scheduler evidence, then per-issue
// PR hydration. Keeping this arm outside the new observer gives real before
// counts rather than measuring the new implementation's failure path.
func legacyCandidatePRRefresh(t *testing.T, c *Connector, mode string) (connector.CandidateResult, error) {
	t.Helper()
	var nodes []githubIssueNode
	var issues []connector.Issue
	if mode == "labels" {
		var rows []restIssue
		if err := c.client.REST(t.Context(), http.MethodGet, restRepositoryIssuesByLabelPagePath(c.repository, "detent:human-review", 1, 10, true), nil, &rows); err != nil {
			return connector.CandidateResult{}, err
		}
		for _, row := range rows {
			node := githubIssueNodeFromREST(issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: row.Number}, row)
			nodes = append(nodes, node)
			issues = append(issues, c.buildLabelIssue(node, "Human Review"))
		}
	} else {
		var response struct {
			Node struct{ Items projectItemsConnection }
		}
		if err := c.client.GraphQLWithType(t.Context(), graphQLQueryCandidateIssues, candidateProjectItemsQuery, map[string]any{"projectId": "PVT_1", "first": 10}, &response); err != nil {
			return connector.CandidateResult{}, err
		}
		for _, item := range response.Node.Items.Nodes {
			nodes = append(nodes, *item.Content)
			issue, _, ok, _, err := c.normalizeProjectItem(item)
			if err != nil {
				return connector.CandidateResult{}, err
			}
			if ok {
				issues = append(issues, issue)
			}
		}
	}
	evidence := c.candidateEvidence(t.Context(), nodes, mode == "labels")
	for i, issue := range issues {
		hydrated, err := c.hydrateCandidateWithEvidence(t.Context(), issue, []string{"Human Review"}, evidence)
		if err != nil {
			return connector.CandidateResult{}, err
		}
		issues[i] = hydrated
	}
	return connector.CandidateResult{Issues: issues}, nil
}

func TestCandidatePRIndependentRefreshEvidence(t *testing.T) {
	for _, entry := range []string{"admission", "refresh candidates", "refresh observed", "refresh overlap"} {
		t.Run(entry, func(t *testing.T) { testIndependentRefreshEvidence(t, entry) })
	}
}

func testIndependentRefreshEvidence(t *testing.T, entry string) {
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any, *[]any)
		check  func(*testing.T, connector.Issue)
	}{
		{name: "issue labels", mutate: func(i, p map[string]any, r *[]any) {
			i["labels"] = map[string]any{"nodes": []any{map[string]string{"name": "urgent"}}}
		}, check: func(t *testing.T, i connector.Issue) {
			if len(i.Labels) != 1 || i.Labels[0] != "urgent" {
				t.Fatal(i.Labels)
			}
		}},
		{name: "association added", mutate: func(i, p map[string]any, r *[]any) { *r = []any{candidatePRFixtureReference("fixture/project", 1)} }, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest == nil || i.PullRequest.Number != 101 {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "branch discovered", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest == nil || i.PRSource != "detent_branch" {
				t.Fatal(i)
			}
		}},
		{name: "Blocked without association", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest != nil {
				t.Fatal(i.PullRequest)
			}
		}},

		{name: "elapsed queue time", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.UnstartedCheckCount != 1 {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "Merging lane", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.State != "Merging" || i.PullRequest == nil {
				t.Fatal(i)
			}
		}},
		{name: "Blocked lane", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.State != "Blocked" || i.PullRequest == nil {
				t.Fatal(i)
			}
		}},
		{name: "write issue body", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.Description != "written body" {
				t.Fatal(i.Description)
			}
		}},
		{name: "write issue comment", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if len(i.Comments) != 1 || i.Comments[0].Body != "written comment" {
				t.Fatal(i.Comments)
			}
		}},
		{name: "write PR comment", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.LatestCodexReviewState != "PENDING" {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "native relation added", mutate: func(i, p map[string]any, r *[]any) { i["blockedBy"] = candidateFixtureBlocker("OPEN") }, check: func(t *testing.T, i connector.Issue) {
			if len(i.BlockedBy) != 1 || i.BlockedBy[0].State != "Open" {
				t.Fatal(i.BlockedBy)
			}
		}},
		{name: "native relation removed", mutate: func(i, p map[string]any, r *[]any) { i["blockedBy"] = map[string]any{"nodes": []any{}} }, check: func(t *testing.T, i connector.Issue) {
			if len(i.BlockedBy) != 0 {
				t.Fatal(i.BlockedBy)
			}
		}},
		{name: "native blocker closed", mutate: func(i, p map[string]any, r *[]any) { i["blockedBy"] = candidateFixtureBlocker("CLOSED") }, check: func(t *testing.T, i connector.Issue) {
			if len(i.BlockedBy) != 1 || i.BlockedBy[0].State != "Done" {
				t.Fatal(i.BlockedBy)
			}
		}},
		{name: "native blocker reopened", mutate: func(i, p map[string]any, r *[]any) { i["blockedBy"] = candidateFixtureBlocker("OPEN") }, check: func(t *testing.T, i connector.Issue) {
			if len(i.BlockedBy) != 1 || i.BlockedBy[0].State != "Open" {
				t.Fatal(i.BlockedBy)
			}
		}},
		{name: "PR summary edited", mutate: func(i, p map[string]any, r *[]any) { p["comments"] = candidateFixtureSummary(false) }, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.LatestCodexReviewState != "PENDING" {
				t.Fatal(i.PullRequest)
			}
		}},

		{name: "unchanged", mutate: func(map[string]any, map[string]any, *[]any) {}, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.CIStatus != "pass" {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "issue body", mutate: func(i, p map[string]any, r *[]any) { i["body"] = "edited body" }, check: func(t *testing.T, i connector.Issue) {
			if i.Description != "edited body" {
				t.Fatal(i.Description)
			}
		}},
		{name: "added issue comment", mutate: func(i, p map[string]any, r *[]any) {
			i["comments"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "body": "new comment"}}}
		}, check: func(t *testing.T, i connector.Issue) {
			if len(i.Comments) != 1 {
				t.Fatal(i.Comments)
			}
		}},
		{name: "edited issue comment", mutate: func(i, p map[string]any, r *[]any) {
			i["comments"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "body": "edited comment"}}}
		}, check: func(t *testing.T, i connector.Issue) {
			if len(i.Comments) != 1 || i.Comments[0].Body != "edited comment" {
				t.Fatal(i.Comments)
			}
		}},
		{name: "PR closed", mutate: func(i, p map[string]any, r *[]any) { p["state"] = "CLOSED" }, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.State != "CLOSED" {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "PR reopened", mutate: func(i, p map[string]any, r *[]any) { p["state"] = "OPEN" }, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.State != "OPEN" {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "PR head", mutate: func(i, p map[string]any, r *[]any) {
			p["headRefOid"] = "new-head"
			candidateFixtureCommit(p)["oid"] = "new-head"
		}, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.HeadSHA != "new-head" {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "PR label", mutate: func(i, p map[string]any, r *[]any) {
			p["labels"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]string{"name": "hold"}}}
		}, check: func(t *testing.T, i connector.Issue) {
			if len(i.PullRequest.Labels) != 1 || i.PullRequest.Labels[0] != "hold" {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "check changes on same head", mutate: func(i, p map[string]any, r *[]any) { candidateFixtureCheck(p)["conclusion"] = "FAILURE" }, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.CIStatus != "fail" {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "checks removed on same head", mutate: func(i, p map[string]any, r *[]any) { candidateFixtureCommit(p)["statusCheckRollup"] = nil }, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.CheckRunCount != 0 || len(i.PullRequest.Checks) != 0 {
				t.Fatal(i.PullRequest)
			}
		}},
		{name: "association removed", mutate: func(i, p map[string]any, r *[]any) { *r = nil }, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest != nil || i.PRNumber != nil {
				t.Fatal(i.PullRequest, i.PRNumber)
			}
		}},
		{name: "review added", mutate: func(i, p map[string]any, r *[]any) {
			p["reviews"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"state": "CHANGES_REQUESTED", "body": "Please fix", "author": map[string]string{"login": "chatgpt-codex-connector[bot]"}, "commit": map[string]string{"oid": "head1"}, "submittedAt": "2026-09-15T00:00:00Z"}}}
		}, check: func(t *testing.T, i connector.Issue) {
			if i.PullRequest.CodexReviewAPIState != "CHANGES_REQUESTED" {
				t.Fatal(i.PullRequest)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const repo = "fixture/project"
			issue, snapshot := candidatePRFixtureIssue(repo, 1), candidatePRFixtureSnapshot(repo, 1)
			lane := "Human Review"
			now := time.Date(2026, 9, 15, 0, 0, 10, 0, time.UTC)
			if test.name == "elapsed queue time" {
				check := candidateFixtureCheck(snapshot)
				check["status"], check["conclusion"] = "QUEUED", ""
			}
			if strings.HasSuffix(test.name, " lane") {
				lane = strings.TrimSuffix(test.name, " lane")
			}
			if test.name == "native relation removed" || test.name == "native blocker closed" {
				issue["blockedBy"] = candidateFixtureBlocker("OPEN")
			}
			if test.name == "native blocker reopened" {
				issue["blockedBy"] = candidateFixtureBlocker("CLOSED")
			}
			if test.name == "PR summary edited" {
				snapshot["comments"] = candidateFixtureSummary(true)
			}
			issue["comments"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "body": "original comment"}}}
			refs := []any{candidatePRFixtureReference(repo, 1)}
			discovered := []any{}
			if test.name == "association added" || test.name == "branch discovered" || test.name == "Blocked without association" {
				refs = nil
			}
			if test.name == "branch discovered" || test.name == "Blocked without association" {
				ref := candidatePRFixtureReference(repo, 1)
				ref["headRefName"] = detentIssueBranchPrefix(repo+"#1") + "-work"
				discovered = append(discovered, ref)
			}
			if test.name == "Blocked without association" {
				lane = "Blocked"
			}
			if test.name == "PR reopened" {
				snapshot["state"] = "CLOSED"
			}
			restReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					if err := json.NewEncoder(w).Encode(v); err != nil {
						t.Error(err)
					}
				}
				if strings.HasPrefix(r.URL.Path, "/repos/") && r.Method != http.MethodGet {
					var body struct{ Body string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					switch {
					case strings.HasSuffix(r.URL.Path, "/issues/1/comments"):
						issue["comments"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "body": body.Body}}}
					case strings.HasSuffix(r.URL.Path, "/issues/101/comments"):
						snapshot["comments"] = candidateFixtureSummary(false)
					case r.Method == http.MethodPatch:
						issue["body"] = body.Body
					default:
						t.Errorf("unexpected write %s", r.URL)
					}
					write(map[string]any{"node_id": "I1", "number": 1, "body": issue["body"]})
					return
				}
				if r.Method != http.MethodPost {
					restReads++
					switch {
					case strings.HasSuffix(r.URL.Path, "/issues/1"):
						write(map[string]any{"node_id": "I1", "number": 1, "body": issue["body"]})
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						check := candidateFixtureCheck(snapshot)
						run := map[string]any{"id": 1, "name": "unit", "status": strings.ToLower(check["status"].(string)), "conclusion": strings.ToLower(check["conclusion"].(string)), "created_at": "2026-09-15T00:00:00Z", "started_at": "2026-09-15T00:00:01Z", "completed_at": "2026-09-15T00:00:02Z"}
						if test.name == "elapsed queue time" {
							run["started_at"], run["completed_at"] = nil, nil
						}
						write(map[string]any{"check_runs": []any{run}})
					case strings.HasSuffix(r.URL.Path, "/statuses"), strings.HasSuffix(r.URL.Path, "/annotations"):
						write([]any{})
					default:
						t.Errorf("unexpected REST %s", r.URL)
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}
				var req struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if !strings.Contains(req.Query, "rateLimit {") || !strings.Contains(req.Query, "cost") {
					t.Error("missing per-request GraphQL cost")
				}
				switch {
				case strings.Contains(req.Query, "CandidateHydration"):
					write(map[string]any{"data": map[string]any{"issue0": issue}})
				case strings.Contains(req.Query, "CandidatePullRequestReferences"):
					write(map[string]any{"data": map[string]any{"nodes": []any{map[string]any{"id": "I1", "closedByPullRequestsReferences": map[string]any{"totalCount": len(refs), "nodes": refs}}}, "repo0": map[string]any{"pullRequests": map[string]any{"nodes": discovered}}}})
				case strings.Contains(req.Query, "CandidatePullRequestStatus"):
					write(map[string]any{"data": map[string]any{"pr0": map[string]any{"pullRequest": snapshot}}})
				default:
					write(map[string]any{"data": map[string]any{"node": map[string]any{"items": map[string]any{"nodes": []any{map[string]any{"id": "P1", "content": issue, "statusValue": map[string]string{"name": lane}}}}}}})
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: repo, ActiveStates: []string{"Todo"}, ObservedStates: []string{lane}})
			c.now = func() time.Time { return now }
			c.unstartedThreshold = time.Minute
			refresh := func() connector.Issue {
				t.Helper()
				var result connector.CandidateResult
				var err error
				if entry == "admission" {
					result, err = c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{lane}, Limit: 10, PageSize: 10})
				} else {
					candidates, observed := []string{lane}, []string(nil)
					if entry == "refresh observed" {
						candidates, observed = nil, candidates
					}
					if entry == "refresh overlap" {
						observed = candidates
					}
					refreshed := c.FetchRefreshIssues(t.Context(), candidates, observed, connector.IssueFilterHint{})
					if refreshed.StatusError != nil {
						t.Fatal(refreshed.StatusError)
					}
					result.Issues, err = refreshed.Candidates, refreshed.CandidateError
					if entry == "refresh observed" {
						result.Issues = refreshed.Statuses
					}
					if entry == "refresh overlap" && (len(refreshed.Statuses) != 1 || !reflect.DeepEqual(refreshed.Candidates, refreshed.Statuses)) {
						t.Fatalf("overlap=%+v", refreshed)
					}
				}
				if err != nil || len(result.Issues) != 1 {
					t.Fatalf("%+v %v", result, err)
				}
				return result.Issues[0]
			}
			initial := refresh()
			if test.name == "PR summary edited" && initial.PullRequest.LatestCodexReviewState != "COMMENTED" {
				t.Fatal(initial.PullRequest)
			}
			for range 120 {
				now = now.Add(30 * time.Second)
				refresh()
			}
			before := restReads
			test.mutate(issue, snapshot, &refs)
			if test.name == "elapsed queue time" {
				now = now.Add(2 * time.Minute)
			}
			switch test.name {
			case "write issue body":
				if err := c.UpdateIssueBody(t.Context(), "I1", "written body"); err != nil {
					t.Fatal(err)
				}
			case "write issue comment":
				if err := c.CreateComment(t.Context(), "I1", "written comment"); err != nil {
					t.Fatal(err)
				}
			case "write PR comment":
				if err := c.CreatePullRequestComment(t.Context(), repo, 101, codexReviewSummaryMarker); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(test.name, "write ") {
				before = restReads
			}
			got := refresh()
			test.check(t, got)
			if test.name == "unchanged" || test.name == "elapsed queue time" || strings.Contains(test.name, "issue") {
				if restReads != before {
					t.Fatalf("unchanged PR reread detail: %d -> %d", before, restReads)
				}
			}
		})
	}
}

func candidateFixtureCommit(snapshot map[string]any) map[string]any {
	return snapshot["commits"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["commit"].(map[string]any)
}
func candidateFixtureCheck(snapshot map[string]any) map[string]any {
	return candidateFixtureCommit(snapshot)["statusCheckRollup"].(map[string]any)["contexts"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
}

func TestCandidatePRIncompleteObservation(t *testing.T) {
	for _, collection := range []string{"labels", "reviews", "comments", "checks", "annotations", "head"} {
		t.Run(collection, func(t *testing.T) {
			data := candidatePRFixtureSnapshot("fixture/project", 1)
			switch collection {
			case "head":
				candidateFixtureCommit(data)["oid"] = "different-head"
			case "checks":
				candidateFixtureCommit(data)["statusCheckRollup"].(map[string]any)["contexts"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true}
			case "annotations":
				candidateFixtureCheck(data)["annotations"] = map[string]any{"totalCount": 2, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": true}}
			default:
				data[collection].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true}
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot candidatePRSnapshot
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.complete() {
				t.Fatal("incomplete observation accepted as reusable")
			}
		})
	}
}

func candidateFixtureBlocker(state string) map[string]any {
	return map[string]any{"nodes": []any{map[string]any{"id": "B1", "number": 2, "state": state, "repository": map[string]string{"nameWithOwner": "fixture/project"}, "labels": map[string]any{"nodes": []any{map[string]string{"name": "detent:todo"}}}}}}
}
func candidateFixtureSummary(completed bool) map[string]any {
	body := codexReviewSummaryMarker + " in progress"
	if completed {
		body = testCodexReviewSummaryBody("✅ **Completed** <relative-time datetime=\"2026-09-15T00:01:00Z\">2026-09-15T00:01:00Z</relative-time>", "abcdef0")
	}
	return map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C1", "databaseId": 1, "body": body, "author": map[string]string{"login": "chatgpt-codex-connector[bot]", "__typename": "Bot"}, "createdAt": "2026-09-15T00:00:00Z", "updatedAt": "2026-09-15T00:02:00Z"}}}
}

func TestCandidatePRPartialCursor(t *testing.T) {
	const repo = "fixture/project"
	details := map[int]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			t.Errorf("unexpected REST %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
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
		case strings.Contains(req.Query, "CandidatePullRequestReferences"):
			ids := req.Variables["ids"].([]any)
			if len(ids) != 1 {
				t.Errorf("observed %d candidates beyond cursor limit", len(ids))
			}
			nodes := []any{}
			for _, id := range ids {
				var n int
				if _, err := fmt.Sscanf(id.(string), "I%d", &n); err != nil {
					t.Error(err)
					return
				}
				nodes = append(nodes, map[string]any{"id": id, "closedByPullRequestsReferences": map[string]any{"totalCount": 1, "nodes": []any{candidatePRFixtureReference(repo, n)}}})
			}
			data["nodes"] = nodes
			data["repo0"] = map[string]any{"pullRequests": map[string]any{"nodes": []any{}}}
		case strings.Contains(req.Query, "CandidatePullRequestStatus"):
			for n := 1; n <= 3; n++ {
				if strings.Contains(req.Query, fmt.Sprintf("pullRequest(number:%d)", 100+n)) {
					details[n]++
					snapshot := candidatePRFixtureSnapshot(repo, n)
					candidateFixtureCommit(snapshot)["statusCheckRollup"] = nil
					data["pr0"] = map[string]any{"pullRequest": snapshot}
				}
			}
		default:
			items := []any{}
			for n := 1; n <= 3; n++ {
				items = append(items, map[string]any{"id": fmt.Sprintf("P%d", n), "content": candidatePRFixtureIssue(repo, n), "statusValue": map[string]string{"name": "Merging"}})
			}
			data["node"] = map[string]any{"items": map[string]any{"nodes": items}}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: repo, ActiveStates: []string{"Todo"}, ObservedStates: []string{"Merging"}})
	cursor := ""
	for n := 1; n <= 3; n++ {
		result, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Merging"}, Limit: 1, PageSize: 10, Cursor: cursor})
		if err != nil || len(result.Issues) != 1 || result.Issues[0].PullRequest == nil || result.Issues[0].PullRequest.Number != 100+n || result.Truncated != (n < 3) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		cursor = result.NextCursor
	}
	for n := 1; n <= 3; n++ {
		if details[n] != 1 {
			t.Fatalf("candidate %d detail observations=%d", n, details[n])
		}
	}
}

func TestCandidatePRFallbackUsesFreshAssociation(t *testing.T) {
	for _, kind := range []string{"association pagination", "status unavailable"} {
		t.Run(kind, func(t *testing.T) {
			const repo = "fixture/project"
			associationReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					if err := json.NewEncoder(w).Encode(v); err != nil {
						t.Error(err)
					}
				}
				if r.Method == http.MethodPost {
					associationReads++
					var req struct{ Query string }
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						return
					}
					if strings.Contains(req.Query, "ReferencesPage") {
						write(map[string]any{"data": map[string]any{"node": map[string]any{"__typename": "Issue", "id": "I1", "closedByPullRequestsReferences": map[string]any{"nodes": []any{candidatePRFixtureReference(repo, 2)}}}}})
					} else {
						ref := candidatePRFixtureReference(repo, 1)
						ref["state"] = "CLOSED"
						write(map[string]any{"data": map[string]any{"nodes": []any{map[string]any{"__typename": "Issue", "id": "I1", "number": 1, "repository": map[string]string{"nameWithOwner": repo}, "closedByPullRequestsReferences": map[string]any{"nodes": []any{ref}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "next"}}}}}})
					}
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/102"):
					write(map[string]any{"number": 102, "state": "open", "head": map[string]string{"sha": "head2", "ref": "branch"}})
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					write(map[string]any{"check_runs": []any{}})
				case strings.HasSuffix(r.URL.Path, "/statuses"), strings.HasSuffix(r.URL.Path, "/reviews"), strings.HasSuffix(r.URL.Path, "/comments"):
					write([]any{})
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: repo, ActiveStates: []string{"Todo"}, ObservedStates: []string{"Human Review"}})
			old := 101
			issue := connector.Issue{ID: "I1", Identifier: repo + "#1", State: "Human Review", PRNumber: &old, PRRepository: repo}
			observation := &candidatePullRequestEvidence{}
			if kind == "status unavailable" {
				observation.number = 102
				observation.repo = pullRequestRepo{Owner: "fixture", Name: "project"}
				observation.source = "github_closing_reference"
			}
			node := githubIssueNode{ID: "I1", BlockedBy: &issueNodesConnection{}, CandidatePR: observation}
			result, err := c.hydrateCandidateWithEvidence(t.Context(), issue, []string{"Human Review"}, map[string]githubIssueNode{"I1": node})
			if err != nil || result.PullRequest == nil || result.PullRequest.Number != 102 {
				t.Fatalf("PR=%+v err=%v", result.PullRequest, err)
			}
			want := 0
			if kind == "association pagination" {
				want = 2
			}
			if associationReads != want {
				t.Fatalf("association pages=%d want=%d", associationReads, want)
			}
		})
	}
}
