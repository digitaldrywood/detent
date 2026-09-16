package github

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

// Five isolated projects simulate an hour of normal refresh, not admission.
// The fixture clock and serialized counters make request counts deterministic.
func TestProjectRefreshHourlyWorkload(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("large board four refreshes hourly/resumed=%t", resumed), func(t *testing.T) { testLargeProjectRefreshHourlyWorkload(t, resumed) })
	}
	for _, workload := range []struct {
		name                string
		projects, refreshes int
	}{{"five projects hourly", 5, 120}, {"ten projects cold", 10, 1}} {
		t.Run(workload.name, func(t *testing.T) {
			for _, mode := range []string{"no validators", "stable etag", "changing etag", "fallback etag", "schema fallback etag"} {
				for _, observed := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/observed=%t", mode, observed), func(t *testing.T) {
						var details, graphql, fallbacks, notModified int
						var countsMu sync.Mutex
						var reportedTotal, reportedBillable, reported304 int
						for project := range workload.projects {
							now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
							repo := fmt.Sprintf("fixture/project%d", project)
							var logs bytes.Buffer
							server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								countsMu.Lock()
								defer countsMu.Unlock()
								w.Header().Set("Content-Type", "application/json")
								write := func(v any) {
									t.Helper()
									if strings.Contains(mode, "etag") && r.Method == http.MethodGet {
										data, err := json.Marshal(v)
										if err != nil {
											t.Error(err)
											return
										}
										etag := fmt.Sprintf("\"%x\"", sha256.Sum256(data))
										if mode == "changing etag" {
											etag = fmt.Sprintf("\"%d\"", details)
										}
										w.Header().Set("ETag", etag)
										if r.Header.Get("If-None-Match") == etag {
											notModified++
											w.WriteHeader(http.StatusNotModified)
											return
										}
									}
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
									if mode == "schema fallback etag" && strings.Contains(req.Query, "blockedBy(") {
										write(map[string]any{"errors": []map[string]string{{"message": "Field 'blockedBy' doesn't exist on type 'Issue'"}}})
										return
									}
									switch {
									case strings.Contains(req.Query, "CandidateHydration"):
										data := map[string]any{}
										for n := 1; n <= 3; n++ {
											data[fmt.Sprintf("issue%d", n-1)] = candidatePRFixtureIssue(repo, n)
										}
										write(map[string]any{"data": data})
									case strings.Contains(req.Query, "CandidatePullRequestReferences"):
										if strings.Contains(mode, "fallback") {
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
									default:
										items := []map[string]any{}
										for n := 1; n <= 3; n++ {
											node := candidatePRFixtureIssue(repo, n)
											if mode == "schema fallback etag" {
												delete(node, "blockedBy")
											}
											items = append(items, map[string]any{"id": fmt.Sprintf("P%d", n), "content": node, "statusValue": map[string]string{"name": "Human Review"}})
										}
										write(map[string]any{"data": map[string]any{"node": map[string]any{"items": map[string]any{"nodes": items}}}})
									}
									return
								}
								switch {
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
								case strings.Contains(r.URL.Path, "/dependencies/blocked_by"):
									details++
									write([]any{})
								case strings.HasSuffix(r.URL.Path, "/issues/1"), strings.HasSuffix(r.URL.Path, "/issues/2"), strings.HasSuffix(r.URL.Path, "/issues/3"):
									details++
									var n int
									fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/"+repo+"/issues/"), "%d", &n)
									write(map[string]any{"node_id": fmt.Sprintf("I%d", n), "number": n, "state": "open", "title": "Candidate", "body": "body", "comments": 0})
								case strings.HasSuffix(r.URL.Path, "/statuses"), strings.HasSuffix(r.URL.Path, "/comments"):
									details++
									write([]any{})
								default:
									t.Errorf("unexpected request %s", r.URL)
									w.WriteHeader(http.StatusNotFound)
								}
							}))
							t.Cleanup(server.Close)
							cfg := Config{ProjectSlug: "PVT_1", Repository: repo, ActiveStates: []string{"Todo"}, ObservedStates: []string{"Human Review"}, Now: func() time.Time { return now }}
							c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, cfg)
							c.logger = slog.New(slog.NewTextHandler(&logs, nil))
							for refresh := range workload.refreshes {
								now = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(refresh) * 30 * time.Second)
								candidateStates, observedStates := []string{"Human Review"}, []string(nil)
								if observed {
									candidateStates, observedStates = nil, candidateStates
								}
								result := c.FetchRefreshIssues(t.Context(), candidateStates, observedStates, connector.IssueFilterHint{})
								issues := result.Candidates
								if observed {
									issues = result.Statuses
								}
								if result.CandidateError != nil || result.StatusError != nil || len(issues) != 3 {
									t.Fatalf("refresh errors: %v, %v; issues=%d", result.CandidateError, result.StatusError, len(issues))
								}
								for _, issue := range issues {
									if issue.PullRequest == nil || issue.PullRequest.CIStatus != "pass" {
										t.Fatalf("missing PR status: %+v", issue.PullRequest)
									}
								}
							}
							usage := c.client.FlushRESTRateLimitUsage()
							reportedTotal += int(usage.TotalRequests)
							reportedBillable += int(usage.BillableRequests)
							reported304 += int(usage.NotModifiedRequests)
							fallbacks += strings.Count(logs.String(), "github candidate PR detail fallback")
							server.Close()
						}
						rest := details
						billable := rest - notModified
						t.Logf("projects=%d refreshes=%d cadence=30s candidates/project=3 REST=%d billable_REST=%d GraphQL=%d 304=%d fallback=%d", workload.projects, workload.refreshes, rest, billable, graphql, notModified, fallbacks)
						if reportedTotal != rest || reportedBillable != billable || reported304 != notModified {
							t.Fatalf("accounting=(%d,%d,%d) server=(%d,%d,%d)", reportedTotal, reportedBillable, reported304, rest, billable, notModified)
						}
						if !strings.Contains(mode, "fallback") {
							wantREST := workload.projects * 6
							wantGraphQL := workload.projects * workload.refreshes * 4
							if rest != wantREST || graphql != wantGraphQL || notModified != 0 {
								t.Fatalf("want REST=%d GraphQL=%d 304=0", wantREST, wantGraphQL)
							}
							if billable >= 2500 {
								t.Fatalf("billable REST=%d must stay below 2500", billable)
							}
						} else if workload.refreshes > 1 && (notModified == 0 || billable >= rest) {
							t.Fatal("fallback failed to preserve free 304s")
						}

					})
				}
			}
		})
	}
}

// Costs measured against the Detent ProjectV2 board on 2026-09-16 using
// data.rateLimit.cost: thin page(first:100), including scalar bodies and the
// project updatedAt revision, costs 2; scheduler aliases=22 for 100
// issues, 11 for 52, and 1 for 3. These are response fixtures, not header deltas.
func testLargeProjectRefreshHourlyWorkload(t *testing.T, resumed bool) {
	const total, candidates, refreshes = 1500, 152, 4
	var graphql, rest, points, preflights, failedPages int
	failPage := resumed
	humanBody := strings.Replace(prerequisiteBody(t), "schema: 1", "schema: 1\ncompletion_evidence: Verified test tenant", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			rest++
			if !strings.HasSuffix(r.URL.Path, "/pulls") {
				t.Errorf("unexpected REST %s", r.URL)
			}
			fmt.Fprint(w, `[]`)
			return
		}
		graphql++
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(req.Query, "rateLimit {") || !strings.Contains(req.Query, "cost") {
			t.Error("missing per-request cost")
		}
		data := map[string]any{}
		cost := 2
		if strings.Contains(req.Query, "RefreshProjectRevision") {
			preflights++
			cost = 1
			if strings.Contains(req.Query, "items(") {
				t.Error("revision query must read updatedAt without an items connection")
			}
			data["node"] = map[string]any{"updatedAt": "2026-09-16T20:00:00Z"}
		} else if strings.Contains(req.Query, "CandidateHydration") {
			count := 0
			for key, value := range req.Variables {
				if !strings.HasPrefix(key, "id") {
					continue
				}
				var n int
				fmt.Sscanf(value.(string), "I%d", &n)
				if n < 1 || n > candidates {
					t.Errorf("hydrated observed item %d", n)
				}
				data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": value, "body": "scheduler body", "comments": map[string]any{"totalCount": 0, "nodes": []any{}}, "blockedBy": map[string]any{"nodes": []any{}}}
				if n == 1 {
					data["issue"+strings.TrimPrefix(key, "id")].(map[string]any)["blockedBy"] = map[string]any{"nodes": []any{map[string]any{"id": "I1500", "number": 1500, "body": humanBody, "state": "CLOSED", "repository": map[string]string{"nameWithOwner": "fixture/large"}}}}
				}
				count++
			}
			switch count {
			case 100:
				cost = 22
			case 52:
				cost = 11
			default:
				t.Errorf("unexpected batch size %d", count)
			}
			if strings.Contains(req.Query, "labels(first: 100)") || strings.Contains(req.Query, "closedByPullRequestsReferences") {
				t.Error("unbounded dependency labels or redundant PR preview")
			}
		} else {
			for _, field := range []string{"blockedBy(", "comments(first:", "closedByPullRequestsReferences(", "commits("} {
				if strings.Contains(req.Query, field) {
					t.Errorf("board query carries %s", field)
				}
			}
			start := 0
			if after, ok := req.Variables["after"].(string); ok {
				fmt.Sscanf(after, "page-%d", &start)
			}
			if start == 100 && failPage {
				failPage = false
				failedPages++
				http.Error(w, "fixture interrupted refresh", http.StatusBadGateway)
				return
			}
			page := projectItemsConnection{TotalCount: total, PageInfo: pageInfo{HasNextPage: start+100 < total, EndCursor: fmt.Sprintf("page-%d", start+100)}}
			for n := start + 1; n <= start+100; n++ {
				state := "Done"
				if n <= candidates {
					state = "Todo"
				} else if n <= candidates+70 {
					state = "Backlog"
				}
				page.Nodes = append(page.Nodes, projectItemNode{ID: fmt.Sprintf("P%d", n), StatusValue: &singleSelectValue{Name: state}, Content: &githubIssueNode{TypeName: "Issue", ID: fmt.Sprintf("I%d", n), Number: n, State: "OPEN", Title: "Fixture", Repository: repository{NameWithOwner: "fixture/large"}}})
			}
			data["node"] = map[string]any{"updatedAt": "2026-09-16T20:00:00Z", "items": page}
		}
		points += cost
		data["rateLimit"] = map[string]any{"cost": cost, "remaining": 5000 - points, "limit": 5000}
		// Concurrent credential users can move headers far beyond this request's cost.
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(5000-graphql*40))
		w.Header().Set("X-RateLimit-Used", strconv.Itoa(graphql*40))
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/large", ActiveStates: []string{"Todo"}, ObservedStates: []string{"Backlog", "Human Review", "Blocked"}})
	for range refreshes {
		failPage = resumed
		if resumed {
			interrupted := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Done", "Backlog"}, connector.IssueFilterHint{})
			if interrupted.CandidateError == nil || len(interrupted.LaneSignalCandidates) != 0 {
				t.Fatalf("partial refresh published: %+v", interrupted)
			}
		}
		result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Done", "Backlog"}, connector.IssueFilterHint{})
		if result.CandidateError != nil || result.StatusError != nil || len(result.Candidates) != candidates || len(result.Statuses) != total-candidates || len(result.LaneSignalCandidates) != total {
			t.Fatalf("refresh errors=(%v,%v) candidates=%d observed=%d", result.CandidateError, result.StatusError, len(result.Candidates), len(result.Statuses))
		}
		for _, issue := range result.Statuses {
			if issue.State == "Backlog" && (issue.Description != "" || issue.PullRequest != nil || len(issue.Comments) != 0) {
				t.Fatalf("Backlog enriched: %+v", issue)
			}
		}
		for _, issue := range result.Candidates {
			if issue.ID == "I1" && (len(issue.BlockedBy) != 1 || !issue.BlockedBy[0].HumanOwned || !issue.BlockedBy[0].HumanCompletionReady || issue.BlockedBy[0].State != "Done") {
				t.Fatalf("thin board erased human evidence: %+v", issue.BlockedBy)
			}
			if issue.Description != "scheduler body" || issue.DependencySource != connector.BlockedRefSourceNative {
				t.Fatalf("incomplete candidate %s", issue.Identifier)
			}
		}
	}
	usage := c.client.FlushGraphQLRateLimitUsage()
	// Failed page requests have no response cost and are absent from usage.
	wantQueries, wantPoints, wantPreflights := 68, 252, 0
	if resumed {
		wantQueries, wantPoints, wantPreflights = 76, 256, 4
	}
	if preflights != wantPreflights {
		t.Fatalf("preflights=%d want=%d", preflights, wantPreflights)
	}
	if graphql != wantQueries || rest > 4 || points != wantPoints || points >= 1000 || usage.TotalCost != int64(points) || usage.TotalQueries != int64(graphql-failedPages) {
		t.Fatalf("GraphQL=%d REST=%d points=%d accounting=%+v", graphql, rest, points, usage)
	}
	t.Logf("1500 items, 152 candidates, four refreshes/hour: GraphQL=%d REST=%d points=%d", graphql, rest, points)
}
