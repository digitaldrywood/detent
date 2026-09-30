package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestHydrateMergingRulesetStatus(t *testing.T) {
	for _, tt := range []struct {
		name, statuses string
		classic        bool
		wantMissing    bool
	}{
		{"missing", `[]`, false, true},
		{"legacy status present", `[{"context":"Full CI","state":"success"}]`, false, false},
		{"classic missing", `[]`, true, true},
		{"classic present", `[{"context":"Full CI","state":"success"}]`, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rules, protection := `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Full CI"}]}}]`, `{}`
			if tt.classic {
				rules, protection = `[]`, `{"contexts":["Full CI"]}`
			}
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42", body: `{"number":42,"state":"open","head":{"sha":"current-head"},"base":{"ref":"main","sha":"base"}}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/check-runs?per_page=100", body: `{"check_runs":[]}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/statuses?per_page=100", body: tt.statuses},
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42/reviews?per_page=100", body: `[]`},
				emptyPullRequestCommentsResponse("example/repo", 42),
				{method: http.MethodGet, path: "/repos/example/repo/rules/branches/main?per_page=100&page=1", body: rules},
				{method: http.MethodGet, path: "/repos/example/repo/branches/main/protection/required_status_checks", body: protection},
			})
			c := newGitHubTestConnector(t, server, Config{})
			issue := connector.Issue{ID: "issue-1", Identifier: "example/repo#1", State: "Merging", PRNumber: new(42), PRRepository: "example/repo"}
			got, err := c.HydratePullRequest(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if got.PullRequest == nil {
				t.Fatal("missing PR")
			}
			checks := got.PullRequest.RequiredCheckFailures
			if (len(checks) > 0) != tt.wantMissing {
				t.Fatalf("checks=%+v", checks)
			}
			if tt.wantMissing && (checks[0].Name != "Full CI" || checks[0].Status != "missing") {
				t.Fatalf("checks=%+v", checks)
			}
		})
	}
}

func TestCandidateMergingRulesetStatus(t *testing.T) {
	for _, tt := range []struct {
		name, state string
		present     bool
		wantMissing bool
	}{
		{name: "complete ProjectV2 observation", state: "Merging", wantMissing: true},
		{name: "status already present", state: "Merging", present: true},
		{name: "other lane", state: "Human Review"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newGraphQLTestServer(t, nil)
			c := newGitHubTestConnector(t, server, Config{})
			c.cacheBranchMergePolicy("example/repo", BranchMergePolicy{Branch: "main", RequiredStatusChecks: []string{"Full CI"}})
			pr := &pullRequestNode{Number: 42, State: "open", HeadSHA: "head", BaseRefName: "main", CI: pullRequestCI{State: "none"}}
			if tt.present {
				pr.CI.Checks = []connector.PullRequestCheck{{Name: "Full CI", Status: "completed", Conclusion: "success"}}
			}
			evidence := githubIssueNode{CandidatePR: &candidatePullRequestEvidence{complete: true, repo: pullRequestRepo{Owner: "example", Name: "repo"}, pullRequest: pr}}
			issue := connector.Issue{ID: "issue-1", Identifier: "example/repo#1", State: tt.state}
			for range 2 {
				got, err := c.hydratePullRequestWithEvidence(t.Context(), issue, evidence, true)
				if err != nil {
					t.Fatal(err)
				}
				if got.PullRequest == nil {
					t.Fatal("missing PR")
				}
				checks := got.PullRequest.RequiredCheckFailures
				if (len(checks) > 0) != tt.wantMissing {
					t.Fatalf("checks=%+v", checks)
				}
				if tt.wantMissing && (len(checks) != 1 || checks[0].Name != "Full CI" || checks[0].Status != "missing") {
					t.Fatalf("checks=%+v", checks)
				}
			}
			if len(pr.CI.RequiredFailures) != 0 {
				t.Fatal("mutated shared observation")
			}
		})
	}
}

// Replays #3038 / PR #3043: clean against develop, command true, review off,
// no CI producer, and a retained completion waiting on ci_not_green.
func TestExplicitEmptyRequiredChecks(t *testing.T) {
	for _, tt := range []struct {
		name, statuses, runs string
		required             []string
		native               []string
		wantCI               string
		wantFailures         int
	}{
		{name: "recorded clean PR without CI", required: []string{}, wantCI: "pass"},
		{name: "omitted policy retains unknown", wantCI: ""},
		{name: "optional failure ignored", required: []string{}, statuses: `[{"context":"obsolete-CI","state":"failure"}]`, wantCI: "pass"},
		{name: "optional pending ignored", required: []string{}, runs: `[{"name":"obsolete-CI","status":"queued"}]`, wantCI: "pass"},
		{name: "omitted policy retains optional failure", statuses: `[{"context":"obsolete-CI","state":"failure"}]`, wantCI: "fail"},
		{name: "named policy retains optional failure", required: []string{"Native"}, statuses: `[{"context":"Native","state":"success"},{"context":"obsolete-CI","state":"failure"}]`, wantCI: "fail"},
		{name: "missing native check", required: []string{}, native: []string{"Native"}, wantCI: "pending", wantFailures: 1},
		{name: "failed native status", required: []string{}, native: []string{"Native"}, statuses: `[{"context":"Native","state":"failure"}]`, wantCI: "fail", wantFailures: 1},
		{name: "pending native status", required: []string{}, native: []string{"Native"}, statuses: `[{"context":"Native","state":"pending"}]`, wantCI: "pending", wantFailures: 1},
		{name: "failed native run", required: []string{}, native: []string{"Native"}, runs: `[{"name":"Native","status":"completed","conclusion":"failure"}]`, wantCI: "fail", wantFailures: 1},
		{name: "pending native run", required: []string{}, native: []string{"Native"}, runs: `[{"name":"Native","status":"in_progress"}]`, wantCI: "pending", wantFailures: 1},
		{name: "skipped native run", required: []string{}, native: []string{"Native"}, runs: `[{"name":"Native","status":"completed","conclusion":"skipped"}]`, wantCI: "pending", wantFailures: 1},
		{name: "successful native despite obsolete status", required: []string{}, native: []string{"Native"}, statuses: `[{"context":"Native","state":"success"},{"context":"obsolete-CI","state":"failure"}]`, wantCI: "pass"},
	} {
		for _, cached := range []bool{false, true} {
			for _, lane := range []string{"In Progress", "Rework", "Human Review", "Merging"} {
				t.Run(fmt.Sprintf("%s/cached=%t/%s", tt.name, cached, lane), func(t *testing.T) {
					runs, statuses := tt.runs, tt.statuses
					if runs == "" {
						runs = `[]`
					}
					if statuses == "" {
						statuses = `[]`
					}
					native, err := json.Marshal(struct {
						Contexts []string `json:"contexts"`
					}{tt.native})
					if err != nil {
						t.Fatal(err)
					}
					responses := []graphqlTestResponse{
						{method: http.MethodGet, path: "/repos/example/repo/pulls/42", body: `{"number":42,"state":"open","mergeable_state":"clean","head":{"sha":"current-head","ref":"issue-3038"},"base":{"ref":"develop","sha":"base"}}`},
						{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/check-runs?per_page=100", body: `{"check_runs":` + runs + `}`},
						{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/statuses?per_page=100", body: statuses},
						{method: http.MethodGet, path: "/repos/example/repo/pulls/42/reviews?per_page=100", body: `[]`},
						emptyPullRequestCommentsResponse("example/repo", 42),
						{method: http.MethodGet, path: "/repos/example/repo/rules/branches/develop?per_page=100&page=1", body: `[]`},
						{method: http.MethodGet, path: "/repos/example/repo/branches/develop/protection/required_status_checks", body: string(native)},
					}
					if tt.required == nil || len(tt.required) > 0 {
						if lane != "Merging" {
							responses = responses[:5]
						}
					}
					if cached {
						responses = nil
					}
					server := newGraphQLTestServer(t, responses)
					c := newGitHubTestConnector(t, server, Config{RequiredStatusChecks: tt.required})
					issue := connector.Issue{ID: "issue-3038", Identifier: "example/repo#3038", State: lane, PRNumber: new(42), PRRepository: "example/repo"}
					var pr *pullRequestNode
					if cached {
						c.cacheBranchMergePolicy("example/repo", BranchMergePolicy{Branch: "develop", RequiredStatusChecks: tt.native})
						var checkRuns []restCheckRun
						var commitStatuses []restCommitStatus
						if err := json.Unmarshal([]byte(runs), &checkRuns); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(statuses), &commitStatuses); err != nil {
							t.Fatal(err)
						}
						failures := requiredStatusCheckFailures(checkRuns, commitStatuses, c.requiredChecks)
						pr = &pullRequestNode{Number: 42, State: "open", MergeableState: "clean", HeadSHA: "current-head", BaseRefName: "develop"}
						applyPullRequestStatus(pr, pullRequestStatus{ci: pullRequestCI{State: combinedCIState(requiredStatusCheckState(failures), combinedCIState(checkRunsState(checkRuns), commitStatusesState(commitStatuses))), Checks: pullRequestCheckInventory(checkRuns, commitStatuses), RequiredFailures: failures}})
					}
					var originalCI pullRequestCI
					if cached {
						originalCI = pr.CI
						originalCI.Checks = slices.Clone(pr.CI.Checks)
					}
					repeats := 1
					if cached {
						repeats = 2
					}
					for range repeats {
						var got connector.Issue
						var err error
						if cached {
							got, err = c.hydratePullRequestWithEvidence(t.Context(), issue, githubIssueNode{CandidatePR: &candidatePullRequestEvidence{complete: true, repo: pullRequestRepo{Owner: "example", Name: "repo"}, pullRequest: pr}}, true)
						} else {
							got, err = c.HydratePullRequest(t.Context(), issue)
						}
						if err != nil {
							t.Fatal(err)
						}
						if got.PullRequest == nil || normalizePullRequestCIStatus(got.PullRequest.CIStatus) != tt.wantCI {
							t.Fatalf("PR=%+v, want CI %s", got.PullRequest, tt.wantCI)
						}
						if len(got.PullRequest.RequiredCheckFailures) != tt.wantFailures {
							t.Fatalf("failures=%+v, want %d", got.PullRequest.RequiredCheckFailures, tt.wantFailures)
						}
						decision := gate.Evaluate(gate.Config{Run: "true", RequireAutomatedReview: new(false)}, nil, gate.Summary{PullRequestPresent: true, CIStatus: got.PullRequest.CIStatus}, time.Now(), gate.EvaluationOptions{})
						if (decision.Reason == gate.ReasonCINotGreen) != (tt.wantCI != "pass") {
							t.Fatalf("decision=%+v for CI %s", decision, tt.wantCI)
						}
					}
					if cached && !reflect.DeepEqual(pr.CI, originalCI) {
						t.Fatal("mutated cached CI")
					}
				})
			}
		}
	}
}

func TestExplicitEmptyRequiredChecksPolicyUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cached bool
		status int
		body   string
	}{
		{name: "policy read fails", status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`},
		{name: "rules unavailable on plan", status: http.StatusForbidden, body: `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`},
		{name: "cached plan unavailable", cached: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var responses []graphqlTestResponse
			if !tt.cached {
				responses = []graphqlTestResponse{{method: http.MethodGet, path: "/repos/example/repo/rules/branches/develop?per_page=100&page=1", status: tt.status, body: tt.body}}
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{RequiredStatusChecks: []string{}})
			if tt.cached {
				c.cacheBranchMergePolicy("example/repo", BranchMergePolicy{Branch: "develop", RulesUnavailableOnPlan: true})
			}
			issue := connector.Issue{ID: "one", State: "In Progress", PRRepository: "example/repo", PRNumber: new(42), PullRequest: &connector.PullRequest{Number: 42, State: "open", HeadSHA: "head", BaseRef: "develop"}}
			if err := c.attachRequiredBranchChecks(t.Context(), &issue); err == nil {
				t.Fatal("unavailable policy authorized a pass")
			}
			if issue.PullRequest.CIStatus != "" {
				t.Fatalf("CIStatus=%q", issue.PullRequest.CIStatus)
			}
		})
	}
}
