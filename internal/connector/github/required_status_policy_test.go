package github

import (
	"net/http"
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

func TestExplicitEmptyRequiredStatusChecksUsesBranchPolicy(t *testing.T) {
	for _, tt := range []struct {
		name       string
		lane       string
		ci         string
		required   []string
		checks     []connector.PullRequestCheck
		wantCI     string
		wantFailed int
	}{
		{name: "no CI producer", lane: "In Progress", wantCI: "success"},
		{name: "reported failed optional CI", lane: "Rework", ci: "failure", checks: []connector.PullRequestCheck{{Name: "Old CI", Status: "completed", Conclusion: "failure"}}, wantCI: "failure"},
		{name: "optional CI still running", lane: "Human Review", ci: "pending", wantCI: "success"},
		{name: "native check missing before merging", lane: "Human Review", required: []string{"Required"}, wantCI: "pending", wantFailed: 1},
		{name: "native check failed", lane: "Merging", required: []string{"Required"}, checks: []connector.PullRequestCheck{{Name: "Required", Status: "completed", Conclusion: "failure"}}, wantCI: "failure", wantFailed: 1},
		{name: "native check running", lane: "In Progress", required: []string{"Required"}, checks: []connector.PullRequestCheck{{Name: "Required", Status: "in_progress"}}, wantCI: "pending", wantFailed: 1},
		{name: "native check passes but reported failure remains", lane: "Merging", ci: "failure", required: []string{"Required"}, checks: []connector.PullRequestCheck{{Name: "Required", Status: "completed", Conclusion: "success"}, {Name: "Optional", Status: "completed", Conclusion: "failure"}}, wantCI: "failure"},
		{name: "native legacy status failed", lane: "Merging", required: []string{"Required"}, checks: []connector.PullRequestCheck{{Name: "Required", Status: "failure", Conclusion: "failure"}}, wantCI: "failure", wantFailed: 1},
		{name: "native legacy status passed", lane: "Merging", required: []string{"Required"}, checks: []connector.PullRequestCheck{{Name: "Required", Status: "success", Conclusion: "success"}}, wantCI: "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newGitHubTestConnector(t, newGraphQLTestServer(t, nil), Config{RequiredStatusChecks: []string{}})
			c.cacheBranchMergePolicy("example/repo", BranchMergePolicy{Branch: "main", RequiredStatusChecks: tt.required})
			original := &connector.PullRequest{Number: 42, State: "open", BaseRef: "main", HeadSHA: "head", CIStatus: tt.ci, Checks: tt.checks}
			issue := connector.Issue{Identifier: "example/repo#1", State: tt.lane, PullRequest: original}
			if err := c.attachRequiredBranchChecks(t.Context(), &issue); err != nil {
				t.Fatal(err)
			}
			if issue.PullRequest.CIStatus != tt.wantCI || len(issue.PullRequest.RequiredCheckFailures) != tt.wantFailed {
				t.Fatalf("CI=%q failures=%+v, want CI=%q failures=%d", issue.PullRequest.CIStatus, issue.PullRequest.RequiredCheckFailures, tt.wantCI, tt.wantFailed)
			}
			if original.CIStatus != tt.ci || len(original.RequiredCheckFailures) != 0 {
				t.Fatal("mutated shared PR observation")
			}
		})
	}
}

func TestExplicitEmptyRequiredChecksCandidateObservation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		checks []string
		want   string
	}{
		{name: "omitted keeps aggregate wait"},
		{name: "explicit empty advances", checks: []string{}, want: "success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newGitHubTestConnector(t, newGraphQLTestServer(t, nil), Config{RequiredStatusChecks: tt.checks})
			c.cacheBranchMergePolicy("example/repo", BranchMergePolicy{Branch: "main"})
			pr := &pullRequestNode{Number: 42, State: "open", HeadSHA: "head", BaseRefName: "main", CI: pullRequestCI{State: ""}}
			node := githubIssueNode{CandidatePR: &candidatePullRequestEvidence{complete: true, repo: pullRequestRepo{Owner: "example", Name: "repo"}, pullRequest: pr}}
			issue := connector.Issue{ID: "issue", Identifier: "example/repo#1", State: "Human Review"}
			got, err := c.hydratePullRequestWithEvidence(t.Context(), issue, node, true)
			if err != nil {
				t.Fatal(err)
			}
			if got.PullRequest == nil || got.PullRequest.CIStatus != tt.want {
				t.Fatalf("PR=%+v, want CI %q", got.PullRequest, tt.want)
			}
			if pr.CI.State != "" {
				t.Fatal("mutated cached candidate")
			}
		})
	}
}
