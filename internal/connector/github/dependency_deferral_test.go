package github

import (
	"errors"
	"net/http"
	"testing"
)

func TestDependencyIdentifierHydrationPropagatesRESTDeferral(t *testing.T) {
	t.Parallel()
	for _, cap := range []int{1, 2} {
		name := "native dependencies"
		if cap == 2 {
			name = "linked pull request"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			responses := []graphqlTestResponse{{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1", body: `{"node_id":"I_1","number":1,"title":"Blocked","state":"open","labels":[{"name":"detent:blocked"}]}`}}
			if cap == 2 {
				responses = append(responses, graphqlTestResponse{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1/dependencies/blocked_by?per_page=100", body: `[]`})
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent", RESTFanoutMaxRequests: cap})
			issues, err := c.FetchIssueStatesByIdentifiers(t.Context(), []string{"digitaldrywood/detent#1"})
			if !errors.Is(err, ErrRESTFanoutDeferred) || len(issues) != 0 {
				t.Fatalf("issues = %v, error = %v, want no partial evidence and fanout deferral", issues, err)
			}
		})
	}
}
