package github

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Discovery must include idle active cards even behind a full page of closed
// cards. Every refresh starts at page one, independently of admission cursors.
func TestActiveLabelDiscoveryComplete(t *testing.T) {
	t.Parallel()
	for _, closedCount := range []int{0, 100, 200} {
		t.Run(fmt.Sprintf("closed_prefix_%d", closedCount), func(t *testing.T) {
			t.Parallel()
			var responses []graphqlTestResponse
			want := []string{"digitaldrywood/detent#2659", "digitaldrywood/detent#2660", "digitaldrywood/detent#2663"}
			for range 2 {
				for page := 1; page <= closedCount/100+1; page++ {
					var items []string
					if page <= closedCount/100 {
						for i := range 100 {
							n := (page-1)*100 + i + 1
							items = append(items, fmt.Sprintf(`{"node_id":"I_%d","number":%d,"state":"closed","labels":[{"name":"detent:rework"}]}`, n, n))
						}
					} else {
						for _, n := range []int{2659, 2660, 2663} {
							items = append(items, fmt.Sprintf(`{"node_id":"I_%d","number":%d,"state":"open","updated_at":"2026-09-14T21:30:00Z","labels":[{"name":"detent:rework"}]}`, n, n))
						}
					}
					responses = append(responses, graphqlTestResponse{method: http.MethodGet, path: fmt.Sprintf("/repos/digitaldrywood/detent/issues?labels=detent%%3Arework&page=%d&per_page=100&state=all", page), body: "[" + strings.Join(items, ",") + "]"})
				}
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent", ActiveStates: []string{"Rework"}})
			for refresh := range 2 {
				issues, err := c.fetchLabelIssuesByStates(t.Context(), []string{"Rework"}, 0)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, issue := range issues {
					got = append(got, issue.Identifier)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("refresh %d: candidates = %v, want %v", refresh, got, want)
				}
			}
			if got := len(server.requests()); got != len(responses) {
				t.Fatalf("request count = %d, want %d across both refreshes", got, len(responses))
			}

		})
	}
}
