package github

import (
	"testing"
)

func TestFetchWorkflowSources(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       int
		wantErr    bool
	}{
		{"workflow", `{"data":{"repository":{"defaultBranchRef":{"name":"main"},"object":{"entries":[{"name":"ci.yml","object":{"text":"on: push","isTruncated":false}},{"name":"README.md","object":{}}]}}}}`, 1, false},
		{"missing directory", `{"data":{"repository":{"defaultBranchRef":{"name":"main"},"object":null}}}`, 0, false},
		{"missing repo", `{"data":{"repository":null}}`, 0, true},
		{"truncated", `{"data":{"repository":{"defaultBranchRef":{"name":"main"},"object":{"entries":[{"name":"ci.yml","object":{"text":"partial","isTruncated":true}}]}}}}`, 0, true},
		{"binary", `{"data":{"repository":{"defaultBranchRef":{"name":"main"},"object":{"entries":[{"name":"ci.yml","object":{"text":null}}]}}}}`, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newGraphQLTestServer(t, []graphqlTestResponse{{body: tt.body}})
			c := newGitHubTestConnector(t, server, Config{})
			got, err := c.FetchWorkflowSources(t.Context(), "owner/repo")
			if (err != nil) != tt.wantErr || len(got) != tt.want {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}
