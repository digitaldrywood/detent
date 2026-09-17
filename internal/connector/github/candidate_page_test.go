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

func TestCandidatePageObservation(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		failScheduler bool
		failStatus    bool
	}{
		{name: "complete page"},
		{name: "partial scheduler page", failScheduler: true},
		{name: "failed middle status batch", failStatus: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const repo = "fixture/page"
			var associations, statuses, legacyReads int
			aliases := regexp.MustCompile(`(pr[0-9]+): repository\(owner:"fixture",name:"page"\) \{ pullRequest\(number:([0-9]+)\)`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodPost {
					if strings.Contains(r.URL.Path, "/pulls/") && !strings.HasSuffix(r.URL.Path, "/reviews") {
						legacyReads++
						var number int
						fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/repos/"+repo+"/pulls/"), "%d", &number)
						fmt.Fprintf(w, `{"number":%d,"state":"open","head":{"sha":"legacy","ref":"branch"},"base":{"sha":"base","ref":"main"}}`, number)
					} else if strings.HasSuffix(r.URL.Path, "/check-runs") {
						fmt.Fprint(w, `{"check_runs":[]}`)
					} else {
						fmt.Fprint(w, "[]")
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
					if scenario.failScheduler && req.Variables["id0"] == "I26" {
						fmt.Fprint(w, `{"errors":[{"message":"fixture scheduler failure"}]}`)
						return
					}
					for key, id := range req.Variables {
						if strings.HasPrefix(key, "id") {
							data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": id, "comments": map[string]any{}, "blockedBy": map[string]any{}}
						}
					}
				case strings.Contains(req.Query, "CandidatePullRequestReferences"):
					associations++
					nodes := []any{}
					for _, id := range req.Variables["ids"].([]any) {
						var n int
						fmt.Sscanf(id.(string), "I%d", &n)
						nodes = append(nodes, map[string]any{"id": id, "closedByPullRequestsReferences": map[string]any{"totalCount": 1, "nodes": []any{candidatePRFixtureReference(repo, n)}}})
					}
					data["nodes"] = nodes
					data["repo0"] = map[string]any{"pullRequests": map[string]any{}}
				case strings.Contains(req.Query, "CandidatePullRequestStatus"):
					statuses++
					if scenario.failStatus && statuses == 2 {
						fmt.Fprint(w, `{"errors":[{"message":"fixture status failure"}]}`)
						return
					}
					for _, match := range aliases.FindAllStringSubmatch(req.Query, -1) {
						number, err := strconv.Atoi(match[2])
						if err != nil {
							t.Error(err)
							return
						}
						snapshot := candidatePRFixtureSnapshot(repo, number-100)
						candidateFixtureCommit(snapshot)["statusCheckRollup"] = nil
						data[match[1]] = map[string]any{"pullRequest": snapshot}
					}
				default:
					t.Errorf("unexpected query: %s", req.Query)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: repo})
			nodes := make([]githubIssueNode, 41)
			for i := range nodes {
				nodes[i] = githubIssueNode{ID: fmt.Sprintf("I%d", i+1), Number: i + 1, CandidateState: "Human Review", Repository: repository{NameWithOwner: repo}}
			}
			evidence, err := c.candidateEvidenceBatched(t.Context(), nodes, true)
			if (err != nil) != scenario.failScheduler {
				t.Fatalf("scheduler error=%v", err)
			}
			wantEvidence, wantStatuses := 41, 3
			if scenario.failScheduler {
				wantEvidence, wantStatuses = 25, 2
			}
			if len(evidence) != wantEvidence || associations != 1 || statuses != wantStatuses {
				t.Fatalf("evidence=%d associations=%d statuses=%d; want %d, 1, %d", len(evidence), associations, statuses, wantEvidence, wantStatuses)
			}
			for n := 1; n <= wantEvidence; n++ {
				id := fmt.Sprintf("I%d", n)
				node := evidence[id]
				failed := scenario.failStatus && n >= 21 && n <= 40
				if node.CandidatePR == nil || node.CandidatePR.complete == failed {
					t.Fatalf("%s observation=%+v", id, node.CandidatePR)
				}
				issue := connector.Issue{ID: id, Identifier: fmt.Sprintf("%s#%d", repo, n), State: "Human Review"}
				hydrated, err := c.hydratePullRequestWithEvidence(t.Context(), issue, node, true)
				if err != nil || hydrated.PullRequest == nil {
					t.Fatalf("%s hydration=%+v error=%v", id, hydrated.PullRequest, err)
				}
			}
			wantLegacy := 0
			if scenario.failStatus {
				wantLegacy = 20
			}
			if legacyReads != wantLegacy {
				t.Fatalf("legacy PR reads=%d want=%d", legacyReads, wantLegacy)
			}
		})
	}
}
