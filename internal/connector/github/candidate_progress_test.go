package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestProjectCandidateRepairsRemainBatched(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		limit        int
		failNextPage bool
	}{
		{name: "complete scan", limit: 100},
		{name: "candidate limit", limit: 6},
		{name: "partial scan error", limit: 100, failNextPage: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				const total = 9
				release := make(chan struct{})
				var started, completed atomic.Int64
				items := make([]string, total)
				for i := range items {
					items[i] = fmt.Sprintf(`{"id":"PVTI_%d","content":{"__typename":"Issue","id":"I_%d","number":%d,"title":"Candidate","state":"OPEN","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":null}`, i, i, i+1)
				}
				c, err := NewConnector(Config{
					Endpoint: "https://candidate-repairs.test/graphql", APIKey: "token", ProjectSlug: "PVT_1",
					HTTPClient: recoveryHTTPClient(func(r *http.Request) (*http.Response, error) {
						body := `[]`
						if r.Method == http.MethodPost {
							var request struct {
								Query     string         `json:"query"`
								Variables map[string]any `json:"variables"`
							}
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
								return nil, err
							}
							switch {
							case strings.Contains(request.Query, "mutation"):
								started.Add(1)
								<-release
								completed.Add(1)
								body = `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"repaired"}}}}`
							case strings.Contains(request.Query, "items("):
								if request.Variables["after"] != nil {
									return nil, errors.New("page unavailable")
								}
								body = projectItemsPageResponseWithTotal(total, test.failNextPage, "next", items)
							default:
								body = `{"data":{"node":{"field":{"id":"STATUS","options":[{"id":"BACKLOG","name":"Backlog"}]}}}}`
							}
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				result, err := c.ReadCandidates(t.Context(), connector.CandidateRequest{Selector: connector.CandidateSelectorStates, States: []string{"Backlog"}, Limit: test.limit})
				if (err != nil) != test.failNextPage {
					t.Fatalf("ReadCandidates error = %v", err)
				}
				want := min(total, test.limit)
				if len(result.Issues) != want {
					t.Fatalf("candidates = %d, want %d", len(result.Issues), want)
				}
				if (result.NextCursor != "") != (test.failNextPage || test.limit < total) {
					t.Fatalf("cursor = %q", result.NextCursor)
				}
				synctest.Wait()
				if got := started.Load(); got != defaultProjectItemStatusWriteParallelism {
					t.Errorf("concurrent repair writes = %d, want %d", got, defaultProjectItemStatusWriteParallelism)
				}
				close(release)
				synctest.Wait()
				if got := completed.Load(); got != int64(want) {
					t.Errorf("completed repairs = %d, want %d", got, want)
				}
			})
		})
	}
}

func TestCandidateProgressUnderRESTBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		selector       connector.CandidateSelector
		field, restart bool
	}{
		{name: "state labels", selector: connector.CandidateSelectorStates},
		{name: "label union restart", selector: connector.CandidateSelectorLabels, restart: true},
		{name: "untracked", selector: connector.CandidateSelectorUntracked},
		{name: "issue field hydration restart", selector: connector.CandidateSelectorLabels, field: true, restart: true},
		{name: "issue field author search", selector: connector.CandidateSelectorStates, field: true, restart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			const total = 207
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if restFanoutEndpointFamily(restEndpointFamily(r.Method, r.URL.RequestURI())) {
					requests.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost:
					fmt.Fprint(w, `{"errors":[{"message":"GraphQL unavailable: exercise REST budget fallback"}]}`)
				case strings.Contains(r.URL.Path, "/dependencies/blocked_by"):
					fmt.Fprint(w, `[]`)
				case strings.HasPrefix(r.URL.Path, "/orgs/"):
					fmt.Fprint(w, `[{"id":10,"name":"Status","data_type":"single_select","options":[{"id":1,"name":"Backlog"}]}]`)
				case strings.Contains(r.URL.Path, "issue-field-values"):
					fmt.Fprint(w, `[{"issue_field_id":10,"data_type":"single_select","value":1,"single_select_option":{"id":1,"name":"Backlog"}}]`)
				default:
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
					if page < 1 || size < 1 {
						t.Errorf("unexpected path %s", r.URL)
						http.Error(w, "bad page", 400)
						return
					}
					items := []map[string]any{}
					for i := (page - 1) * size; i < min(page*size, total); i++ {
						labels := []map[string]string{}
						if test.selector != connector.CandidateSelectorUntracked {
							labels = append(labels, map[string]string{"name": "detent:backlog"})
						}
						items = append(items, map[string]any{"node_id": fmt.Sprintf("I_%d", i), "number": i + 1, "title": "Candidate", "state": "open", "user": map[string]string{"login": "alice"}, "labels": labels, "html_url": fmt.Sprintf("https://github.com/digitaldrywood/detent/issues/%d", i+1)})
					}
					var body any = items
					if r.URL.Path == "/search/issues" {
						if r.URL.Query().Get("per_page") != "1" && !strings.Contains(r.URL.Query().Get("q"), "author:alice") {
							t.Errorf("author pushdown lost: %s", r.URL)
						}
						body = map[string]any{"total_count": total, "items": items}
					}
					if err := json.NewEncoder(w).Encode(body); err != nil {
						t.Error(err)
					}
				}
			}))
			t.Cleanup(server.Close)
			cfg := Config{Endpoint: server.URL, APIKey: "token", HTTPClient: server.Client(), Repository: "digitaldrywood/detent", GitHubStatusSource: GitHubStatusSourceLabel, ObservedStates: []string{"Backlog"}, RESTFanoutMaxRequests: 4, DisableConditionalRequests: true}
			if test.field {
				cfg.GitHubStatusSource = GitHubStatusSourceIssueField
			}
			makeConnector := func() *Connector {
				c, err := NewConnector(cfg)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			c := makeConnector()
			request := connector.CandidateRequest{Selector: test.selector, States: []string{"Backlog"}, Labels: []string{"one", "two"}, Limit: 100, PageSize: 25}
			if test.field && test.selector == connector.CandidateSelectorStates {
				request.Authors = []string{"alice"}
			}
			// A page size below the read limit makes the remote scan exceed four
			// requests; issue-field hydration consumes this budget within a page.
			request.PageSize = 20
			seen := map[string]bool{}
			deferred := false
			// Cold field and native-dependency hydration can leave one completed
			// item per run; the union visits both label sources before exhaustion.
			for range 3 * total {
				if test.restart {
					c = makeConnector()
				}
				before := requests.Load()
				result, err := c.ReadCandidates(connector.WithRESTFanoutBudget(t.Context(), "candidates"), request)
				if err != nil && !errors.Is(err, ErrRESTFanoutDeferred) {
					t.Fatal(err)
				}
				deferred = deferred || err != nil
				if requests.Load()-before > 4 {
					t.Fatalf("requests = %d, budget = 4", requests.Load()-before)
				}
				if len(result.Issues) == 0 && result.NextCursor == request.Cursor {
					t.Fatalf("no progress: %#v %v", result, err)
				}
				if len(result.Issues) > 100 {
					t.Fatalf("unbounded results: %d", len(result.Issues))
				}
				for _, issue := range result.Issues {
					if issue.DependencySource != connector.BlockedRefSourceNative {
						t.Fatalf("candidate %s lost native dependency hydration", issue.ID)
					}
					seen[issue.ID] = true
				}
				request.Cursor = result.NextCursor
				if request.Cursor == "" {
					break
				}
			}
			if ((test.field || test.selector == connector.CandidateSelectorUntracked) && !deferred) || request.Cursor != "" || len(seen) != total {
				t.Fatalf("deferred=%t cursor=%q covered=%d, want %d", deferred, request.Cursor, len(seen), total)
			}
		})
	}
}
