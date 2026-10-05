package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDiscoverIssuesReadOnlyFiltersAndAccess(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		labels  []string
		closed  bool
		status  int
		missing bool
		want    error
	}{
		{name: "omitted labels"},
		{name: "empty labels", labels: []string{}},
		{name: "nonempty labels", labels: []string{"bug"}},
		{name: "explicit history", labels: []string{"bug"}, closed: true},
		{name: "private access denied", labels: []string{"bug"}, status: 401, want: ErrAuthenticationFailed},
		{name: "rate limited", labels: []string{"bug"}, status: 429, want: ErrRateLimited},
		{name: "missing credential", labels: []string{"bug"}, missing: true, want: ErrMissingToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
					t.Errorf("unexpected source write/path: %s %s", r.Method, r.URL.Path)
				}
				var input struct {
					Query     string `json:"query"`
					Variables struct {
						States []string        `json:"states"`
						Labels json.RawMessage `json:"labels"`
						Cursor string          `json:"cursor"`
					} `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				wantLabels := "null"
				if len(test.labels) > 0 {
					wantLabels = `["bug"]`
				}
				if strings.Contains(input.Query, "mutation") || !strings.Contains(input.Query, "issues(first:100,") || len(input.Variables.States) != 1+btoi(test.closed) || input.Variables.States[0] != "OPEN" || string(input.Variables.Labels) != wantLabels || input.Variables.Cursor != "page2" {
					t.Errorf("discovery request=%#v", input)
				}
				if test.status != 0 {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(test.status)
					return
				}
				node := map[string]any{"id": "I_12", "number": 12, "url": "https://github.com/acme/orders/issues/12", "title": "Issue", "body": strings.Repeat("界", 2000), "closed": test.closed, "labels": map[string]any{"nodes": []any{map[string]string{"name": "bug"}}}}
				nodes := []any{node}
				total := 1
				nextCursor := ""
				switch string(input.Variables.Labels) {
				case "null":
					nodes = append(nodes, map[string]any{"id": "I_13", "number": 13, "url": "https://github.com/acme/orders/issues/13", "title": "Unlabeled issue", "body": "Preview", "closed": false, "labels": map[string]any{"nodes": []any{}}})
					total = 73
					nextCursor = "page3"
				case "[]":
					nodes = []any{}
					total = 0
				}
				response := map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "Acme/Orders", "issues": map[string]any{"totalCount": total, "nodes": nodes, "pageInfo": map[string]any{"hasNextPage": nextCursor != "", "endCursor": nextCursor}}}}}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			})
			httpClient := staticHTTPClient{do: func(r *http.Request) (*http.Response, error) {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				return response.Result(), nil
			}}
			token := "runner-read"
			if test.missing {
				token = ""
			}
			client, err := NewClient(ClientConfig{Endpoint: "https://github.test/graphql", TokenSource: StaticTokenSource(token), HTTPClient: httpClient})
			if err != nil {
				t.Fatal(err)
			}
			client.restBackoffs = newRESTBackoffRegistry()
			page, err := client.DiscoverIssues(t.Context(), tracker.GitHubDiscovery{Repository: "acme/orders", IncludeClosed: test.closed, Labels: test.labels, Cursor: "page2"})
			if test.want != nil {
				if !errors.Is(err, test.want) || len(page.Issues) != 0 {
					t.Fatalf("page=%#v error=%v", page, err)
				}
			} else {
				wantPage := tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{{ID: "I_12", Number: 12, URL: "https://github.com/acme/orders/issues/12", Title: "Issue", Body: strings.Repeat("界", 1000), Closed: test.closed, Labels: []string{"bug"}}}}
				if len(test.labels) == 0 {
					wantPage.Total = 73
					wantPage.NextCursor = "page3"
					wantPage.Issues = append(wantPage.Issues, tracker.GitHubIssuePreview{ID: "I_13", Number: 13, URL: "https://github.com/acme/orders/issues/13", Title: "Unlabeled issue", Body: "Preview", Labels: []string{}})
				}
				if err != nil || !reflect.DeepEqual(page, wantPage) {
					t.Fatalf("page=%#v want=%#v error=%v", page, wantPage, err)
				}
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
