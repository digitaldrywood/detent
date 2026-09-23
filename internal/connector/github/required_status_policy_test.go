package github

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestHydrateMergingRulesetStatus(t *testing.T) {
	for _, tt := range []struct {
		name, statuses string
		wantMissing    bool
	}{
		{"missing", `[]`, true},
		{"legacy status present", `[{"context":"Full CI","state":"success"}]`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42", body: `{"number":42,"state":"open","head":{"sha":"current-head"},"base":{"ref":"main","sha":"base"}}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/check-runs?per_page=100", body: `{"check_runs":[]}`},
				{method: http.MethodGet, path: "/repos/example/repo/commits/current-head/statuses?per_page=100", body: tt.statuses},
				{method: http.MethodGet, path: "/repos/example/repo/pulls/42/reviews?per_page=100", body: `[]`},
				emptyPullRequestCommentsResponse("example/repo", 42),
				{method: http.MethodGet, path: "/repos/example/repo/rules/branches/main?per_page=100&page=1", body: `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Full CI"}]}}]`},
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
