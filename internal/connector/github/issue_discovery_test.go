package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDiscoverIssuesReadOnlyFiltersAndAccess(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		closed  bool
		status  int
		missing bool
		want    error
	}{{name: "open default"}, {name: "explicit history", closed: true}, {name: "private access denied", status: 401, want: ErrAuthenticationFailed}, {name: "rate limited", status: 429, want: ErrRateLimited}, {name: "missing credential", missing: true, want: ErrMissingToken}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
					t.Errorf("unexpected source write/path: %s %s", r.Method, r.URL.Path)
				}
				var input struct {
					Query     string `json:"query"`
					Variables struct {
						States []string `json:"states"`
						Labels []string `json:"labels"`
						Cursor string   `json:"cursor"`
					} `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if strings.Contains(input.Query, "mutation") || len(input.Variables.States) != 1+btoi(test.closed) || input.Variables.States[0] != "OPEN" || len(input.Variables.Labels) != 1 || input.Variables.Labels[0] != "bug" || input.Variables.Cursor != "page2" {
					t.Errorf("discovery request=%#v", input)
				}
				if test.status != 0 {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(test.status)
					return
				}
				node := map[string]any{"id": "I_12", "number": 12, "url": "https://github.com/acme/orders/issues/12", "title": "Issue", "body": strings.Repeat("界", 2000), "closed": test.closed, "labels": map[string]any{"nodes": []any{map[string]string{"name": "bug"}}}}
				response := map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "Acme/Orders", "issues": map[string]any{"totalCount": 1, "nodes": []any{node}, "pageInfo": map[string]any{"hasNextPage": false}}}}}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			token := "runner-read"
			if test.missing {
				token = ""
			}
			client, err := NewClient(ClientConfig{Endpoint: server.URL + "/graphql", TokenSource: StaticTokenSource(token), HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			page, err := client.DiscoverIssues(t.Context(), tracker.GitHubDiscovery{Repository: "acme/orders", IncludeClosed: test.closed, Labels: []string{"bug"}, Cursor: "page2"})
			if test.want != nil {
				if !errors.Is(err, test.want) || len(page.Issues) != 0 {
					t.Fatalf("page=%#v error=%v", page, err)
				}
			} else if err != nil || len(page.Issues) != 1 || len([]rune(page.Issues[0].Body)) != 1000 || page.Issues[0].Closed != test.closed || page.Total != 1 {
				t.Fatalf("page=%#v error=%v", page, err)
			}
			expected := int64(1)
			if test.missing {
				expected = 0
			}
			if calls.Load() != expected {
				t.Fatalf("requests=%d expected=%d", calls.Load(), expected)
			}
		})
	}
}
func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}
