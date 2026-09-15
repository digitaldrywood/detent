package github

import (
	"fmt"
	"net/http"
	"testing"
)

func TestRecordedPullRequestReference(t *testing.T) {
	for _, tt := range []struct{ name, state, merged, want string }{
		{"open", "open", "null", "OPEN"},
		{"closed", "closed", "null", "CLOSED"},
		{"merged", "closed", `"2026-09-14T18:00:00Z"`, "MERGED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/2635", body: fmt.Sprintf(`{"node_id":"PR_2635","number":2635,"state":%q,"html_url":"https://github.com/digitaldrywood/detent/pull/2635","pull_request":{},"labels":[]}`, tt.state)},
				{body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`},
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/2635", body: fmt.Sprintf(`{"number":2635,"state":%q,"merged_at":%s,"html_url":"https://github.com/digitaldrywood/detent/pull/2635","head":{"ref":"some-branch","sha":"abc"}}`, tt.state, tt.merged)},
			})
			c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})
			issues, err := c.FetchIssueStatesByIdentifiers(t.Context(), []string{"digitaldrywood/detent#2635"})
			if err != nil {
				t.Fatal(err)
			}
			if len(issues) != 1 || issues[0].PullRequest == nil {
				t.Fatalf("missing direct PR: %#v", issues)
			}
			if pr := issues[0].PullRequest; pr.Number != 2635 || pr.State != tt.want {
				t.Fatalf("PR = %#v; want 2635 %s", pr, tt.want)
			}
			if n := len(server.requests()); n != 3 {
				t.Fatalf("requests = %d; want 3, without dependency or branch discovery", n)
			}
		})
	}
}
