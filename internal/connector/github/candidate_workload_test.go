package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

// The baseline runs the legacy per-issue PR detail path against the same
// fixture. It is a modeled comparison, not a measurement of an old live build.
func TestCandidateHourlyWorkload(t *testing.T) {
	for _, workload := range []struct {
		name                string
		projects, refreshes int
	}{{"five projects hourly", 5, 120}, {"ten projects cold", 10, 1}} {
		t.Run(workload.name, func(t *testing.T) {
			for _, mode := range []string{"board", "labels"} {
				for _, legacy := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/legacy=%t", mode, legacy), func(t *testing.T) {
						var lists, details, graphql, fallbacks int
						var countsMu sync.Mutex
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
										if legacy {
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
											if strings.Contains(req.Query, fmt.Sprintf("pullRequest(number:%d)", 100+n)) {
												data[fmt.Sprintf("pr%d", len(data))] = map[string]any{"pullRequest": candidatePRFixtureSnapshot(repo, n)}
											}
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
							if mode == "labels" {
								cfg.GitHubStatusSource = GitHubStatusSourceLabel
							}
							c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, cfg)
							c.logger = slog.New(slog.NewTextHandler(&logs, nil))
							for refresh := range workload.refreshes {
								now = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(refresh) * 30 * time.Second)
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
						rest := lists + details
						billable := rest
						t.Logf("projects=%d refreshes=%d cadence=30s candidates/project=3 REST=%d billable_REST=%d GraphQL=%d fallback=%d", workload.projects, workload.refreshes, rest, billable, graphql, fallbacks)
						if !legacy && billable >= 2500 {
							t.Fatalf("billable REST=%d must stay below half of 5000", billable)
						}
						if !legacy && fallbacks != workload.projects*3 {
							t.Fatalf("fallbacks=%d, want only %d cold detail timestamp fallbacks", fallbacks, workload.projects*3)
						}

					})
				}
			}
		})
	}
}
