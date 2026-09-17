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

func TestRefreshAfterStatusFailure(t *testing.T) {
	for _, change := range []string{"blocker closed", "lane changed", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			const stamp = "2026-09-16T20:00:00Z"
			attempt, boardReads, hydrations := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					if attempt < 3 {
						http.Error(w, "status unavailable", http.StatusBadGateway)
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
				blockerStamp, blockerState := stamp, "OPEN"
				if attempt >= 2 && change == "blocker closed" {
					blockerStamp, blockerState = "2026-09-16T20:01:00Z", "CLOSED"
				}
				switch {
				case strings.Contains(req.Query, "RefreshBlockerRevision"):
					data["nodes"] = []any{map[string]any{"id": "B", "updatedAt": blockerStamp}}
				case strings.Contains(req.Query, "RefreshProjectRevision"):
					data["node"] = map[string]any{"updatedAt": stamp}
				case strings.Contains(req.Query, "CandidateHydration"), strings.Contains(req.Query, "RefreshEvidenceRevision"):
					if strings.Contains(req.Query, "CandidateHydration") {
						hydrations++
					}
					data["issue0"] = map[string]any{"id": "I1", "updatedAt": stamp, "comments": map[string]any{"totalCount": 0, "nodes": []any{}}, "blockedBy": map[string]any{"nodes": []any{map[string]any{"id": "B", "number": 2, "state": blockerState, "updatedAt": blockerStamp, "repository": map[string]any{"nameWithOwner": "fixture/rework"}}}}}
				case strings.Contains(req.Query, "LabelIssuePullRequestReferences"):
					if attempt < 3 {
						http.Error(w, "status unavailable", http.StatusBadGateway)
						return
					}
					data["nodes"] = []any{map[string]any{"__typename": "Issue", "id": "I1", "closedByPullRequestsReferences": map[string]any{"nodes": []any{}}}}
				case strings.Contains(req.Query, "CandidatePullRequest"):
					// Leave PR evidence unavailable to exercise the existing status fallback.
				default:
					boardReads++
					lane := "Human Review"
					if attempt == 3 && change == "lane changed" {
						lane = "Done"
					}
					data["node"] = map[string]any{"updatedAt": stamp, "items": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "P1", "statusValue": map[string]any{"name": lane}, "content": map[string]any{"__typename": "Issue", "id": "I1", "number": 1, "state": "OPEN", "repository": map[string]any{"nameWithOwner": "fixture/rework"}}}}}}
				}
				addHydratedProjectFields(data, req.Variables)
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/rework", ActiveStates: []string{"Human Review"}, TerminalStates: []string{"Done"}})
			for attempt = 1; attempt <= 3; attempt++ {
				result := c.FetchRefreshIssues(t.Context(), nil, []string{"Human Review", "Done"}, connector.IssueFilterHint{})
				if result.CandidateError != nil || (result.StatusError != nil) != (attempt < 3) || len(result.Statuses) != 1 {
					t.Fatalf("attempt %d: %+v", attempt, result)
				}
				if attempt >= 2 && change == "blocker closed" && result.Statuses[0].BlockedBy[0].State != "Done" {
					t.Errorf("stale blocker: %+v", result.Statuses[0].BlockedBy)
				}
				if attempt == 3 && change == "lane changed" && result.Statuses[0].State != "Done" {
					t.Errorf("stale lane: %s", result.Statuses[0].State)
				}
			}
			if boardReads != 3 {
				t.Errorf("board reads=%d want 3", boardReads)
			}
			wantHydrations := 1
			if change == "blocker closed" {
				wantHydrations = 2
			}
			if hydrations != wantHydrations {
				t.Errorf("hydrations=%d want %d", hydrations, wantHydrations)
			}
		})
	}
}

// The server honors the requested connection size, so lowering a limit cannot
// silently pass with an unrealistically complete fixture.
func TestCandidatePRLargeCollectionsRemainAuthoritative(t *testing.T) {
	for _, count := range []int{51, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			rest := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					rest++
					if strings.HasSuffix(r.URL.Path, "/check-runs") {
						fmt.Fprint(w, `{"check_runs":[]}`)
					} else {
						fmt.Fprint(w, "[]")
					}
					return
				}
				var req struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				snapshot := candidatePRFixtureSnapshot("fixture/rework", 1)
				collection := func(field string, node any) map[string]any {
					t.Helper()
					after := strings.Split(req.Query, field+"(first:")
					if len(after) != 2 {
						t.Errorf("missing %s connection", field)
						return nil
					}
					var limit int
					if _, err := fmt.Sscanf(after[1], "%d", &limit); err != nil {
						t.Error(err)
					}
					nodes := make([]any, min(count, limit))
					for i := range nodes {
						nodes[i] = node
					}
					return map[string]any{"totalCount": count, "pageInfo": map[string]any{"hasNextPage": count > limit}, "nodes": nodes}
				}
				snapshot["labels"] = collection("labels", map[string]any{"name": "label"})
				snapshot["comments"] = collection("comments", map[string]any{"id": "C", "body": "comment"})
				snapshot["reviews"] = collection("reviews", map[string]any{"state": "APPROVED", "author": map[string]any{"login": "reviewer"}})
				contexts := collection("contexts", map[string]any{"__typename": "CheckRun", "name": "test", "status": "COMPLETED", "conclusion": "SUCCESS", "annotations": collection("annotations", map[string]any{"path": "file.go", "message": "annotation", "annotationLevel": "NOTICE"})})
				snapshot["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"oid": "head1", "statusCheckRollup": map[string]any{"contexts": contexts}}}}}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"pr0": map[string]any{"pullRequest": snapshot}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/rework"})
			key := pullRequestKey{Repo: pullRequestRepo{Owner: "fixture", Name: "rework"}, Number: 101}
			for refresh := range 2 {
				evidence := map[string]githubIssueNode{"I1": {CandidatePR: &candidatePullRequestEvidence{}}}
				before := rest
				c.observeCandidatePullRequestStatus(t.Context(), map[pullRequestKey][]string{key: {"I1"}}, evidence)
				if !evidence["I1"].CandidatePR.complete {
					t.Fatal("large collections lost authoritative PR evidence")
				}
				if refresh == 1 && rest != before {
					t.Errorf("unchanged PR repeated REST: %d requests", rest-before)
				}
			}
		})
	}
}

func TestRefreshBlockerRevision(t *testing.T) {
	for _, scenario := range []string{"unchanged", "edited", "deleted", "missing retained timestamp", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			stamp := "2026-09-16T20:00:00Z"
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string
					Variables struct{ IDs []string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				calls++
				if len(req.Variables.IDs) > 25 {
					t.Errorf("unbounded revision batch: %d", len(req.Variables.IDs))
				}
				if calls == 2 && scenario == "interrupted" {
					http.Error(w, "interrupted", http.StatusBadGateway)
					return
				}
				nodes := []any{}
				for _, id := range req.Variables.IDs {
					updated := stamp
					if id == "B00" {
						if scenario == "deleted" {
							continue
						}
						if scenario == "edited" {
							updated = "2026-09-16T20:01:00Z"
						}
					}
					nodes = append(nodes, map[string]any{"id": id, "updatedAt": updated})
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "fixture/rework"})
			progress := projectItemsScanProgress{evidence: map[string]githubIssueNode{}, hydrated: map[string]bool{}}
			for i := range 26 {
				id := fmt.Sprintf("I%02d", i)
				updated := &stamp
				if i == 0 && scenario == "missing retained timestamp" {
					updated = nil
				}
				progress.evidence[id] = githubIssueNode{BlockedBy: &issueNodesConnection{Nodes: []githubIssueNode{{ID: fmt.Sprintf("B%02d", i), UpdatedAt: updated}}}}
				progress.hydrated[id] = true
			}
			err := c.validateRefreshBlockers(t.Context(), &progress)
			if (err != nil) != (scenario == "interrupted") {
				t.Fatalf("err=%v", err)
			}
			want := 26
			if scenario == "edited" || scenario == "deleted" || scenario == "missing retained timestamp" {
				want = 25
			}
			if len(progress.evidence) != want || len(progress.hydrated) != want || calls != 2 {
				t.Fatalf("evidence=%d hydrated=%d requests=%d", len(progress.evidence), len(progress.hydrated), calls)
			}
			if want == 25 && progress.hydrated["I00"] {
				t.Fatal("changed blocker evidence retained")
			}
		})
	}
}
