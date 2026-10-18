package github

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestFetchValidationIssueReadsCurrentTask(t *testing.T) {
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"node":{"id":"I_200","title":"Revised task","body":"Unproven cause acceptable","comments":{"nodes":[{"id":"C1","body":"decision","url":"https://github.com/example/repo/issues/200#issuecomment-1"}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}`},
		{body: `{"data":{"node":{"id":"I_200","title":"Revised task","body":"Unproven cause acceptable","comments":{"nodes":[{"id":"C2","body":"## Codex Workpad"}],"pageInfo":{"hasNextPage":false}}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{})
	issue := connector.Issue{ID: "I_200", Identifier: "example/repo#200", Title: "Old title", Description: "Require reproduction", PullRequest: &connector.PullRequest{Number: 207, HeadSHA: "head", BaseSHA: "base"}}
	current, err := c.FetchValidationIssue(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if current.Title != "Revised task" || current.Description != "Unproven cause acceptable" || current.PullRequest != issue.PullRequest {
		t.Fatalf("current task/provenance=%+v", current)
	}
	if len(current.Comments) != 2 || current.Comments[1].ID != "C2" {
		t.Fatalf("paginated task comments=%+v", current.Comments)
	}
}
