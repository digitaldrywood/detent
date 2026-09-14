package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestIssueFieldDiagnosticsPreserveRefreshRESTBudget(t *testing.T) {
	t.Parallel()
	for _, count := range []int{80, 101} {
		for _, conditional := range []bool{false, true} {
			t.Run(fmt.Sprintf("issues=%d/conditional=%t", count, conditional), func(t *testing.T) {
				t.Parallel()
				var fieldReads, graphReads atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodPost:
						graphReads.Add(1)
						var request struct {
							Variables struct {
								IDs []string `json:"issueIds"`
							} `json:"variables"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
							return
						}
						if len(request.Variables.IDs) > 100 {
							t.Errorf("oversized diagnostic batch: %d", len(request.Variables.IDs))
						}
						nodes := make([]string, 0, len(request.Variables.IDs))
						for _, id := range request.Variables.IDs {
							nodes = append(nodes, fmt.Sprintf(`{"__typename":"Issue","id":%q,"issueFieldValues":{"nodes":[{"name":"Triage","field":{"name":"Status"}}],"pageInfo":{"hasNextPage":false}}}`, id))
						}
						fmt.Fprintf(w, `{"data":{"nodes":[%s]}}`, strings.Join(nodes, ","))
					case r.URL.Path == "/repos/digitaldrywood/detent/issues":
						page, _ := strconv.Atoi(r.URL.Query().Get("page"))
						items := []string{}
						for i := (page-1)*100 + 1; i <= min(page*100, count); i++ {
							items = append(items, fmt.Sprintf(`{"node_id":"I_%d","number":%d,"state":"open","labels":[{"name":"detent:todo"}]}`, i, i))
						}
						fmt.Fprintf(w, `[%s]`, strings.Join(items, ","))
					case r.URL.Path == "/orgs/digitaldrywood/issue-fields":
						fmt.Fprint(w, `[{"id":10,"name":"Status","data_type":"single_select","options":[{"id":1,"name":"Todo"}]}]`)
					case r.URL.Path == "/search/issues":
						fmt.Fprint(w, `{"total_count":1,"items":[{"node_id":"I_999","number":999,"state":"open"}]}`)
					case strings.Contains(r.URL.Path, "/issue-field-values"):
						fieldReads.Add(1)
						status := "Triage"
						if strings.Contains(r.URL.Path, "/999/") {
							status = "Todo"
						}
						fmt.Fprintf(w, `[{"issue_field_id":10,"data_type":"single_select","single_select_option":{"name":%q}}]`, status)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(server.Close)
				c, err := NewConnector(Config{Endpoint: server.URL, APIKey: "token", HTTPClient: server.Client(),
					GitHubStatusSource: GitHubStatusSourceIssueField, Repository: "digitaldrywood/detent", ActiveStates: []string{"Todo"},
					RESTFanoutMaxRequests: 80, DisableConditionalRequests: !conditional})
				if err != nil {
					t.Fatal(err)
				}
				for tick := range 2 {
					ctx := connector.WithRESTFanoutBudget(t.Context(), "refresh")
					drift, driftErr := c.FetchStatusDrift(ctx)
					issues, readErr := c.fetchIssueFieldIssuesByStates(ctx, []string{"Todo"}, 0, connector.IssueFilterHint{})
					if driftErr != nil || readErr != nil {
						t.Fatalf("tick %d: diagnostic error = %v, normal read error = %v", tick, driftErr, readErr)
					}
					if got := fieldReads.Load(); got != int64(tick+1) {
						t.Fatalf("field REST requests = %d, want only normal scheduling reads", got)
					}
					if got := graphReads.Load(); got != int64((tick+1)*((count+99)/100)) {
						t.Fatalf("GraphQL requests = %d, want one per batch", got)
					}
					if len(drift.LaneSignalCandidates) != count || len(issues) != 1 || issues[0].ID != "I_999" {
						t.Fatalf("tick %d: diagnostics = %d, normal issues = %#v", tick, len(drift.LaneSignalCandidates), issues)
					}
				}
			})
		}
	}
}
