package github

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorMarkPullRequestReady(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		response string
		wantErr  bool
	}{
		{name: "ready", response: `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_42","isDraft":false}}}}`},
		{name: "still draft", response: `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_42","isDraft":true}}}}`, wantErr: true},
		{name: "missing result", response: `{"data":{}}`, wantErr: true},
		{name: "different PR", response: `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"PR_other","isDraft":false}}}}`, wantErr: true},
		{name: "API failure", response: `{"errors":[{"message":"denied"}]}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newGraphQLTestServer(t, []graphqlTestResponse{{body: tt.response}})
			c := newGitHubTestConnector(t, server, Config{})
			err := c.MarkPullRequestReady(t.Context(), connector.Issue{PullRequest: &connector.PullRequest{NodeID: "PR_42"}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error %v", err, tt.wantErr)
			}
			requests := server.requests()
			if len(requests) != 1 {
				t.Fatalf("requests = %d", len(requests))
			}
			if !strings.Contains(requests[0]["query"].(string), "markPullRequestReadyForReview") || requests[0]["variables"].(map[string]any)["pullRequestId"] != "PR_42" {
				t.Fatalf("request = %#v", requests[0])
			}
		})
	}
}
