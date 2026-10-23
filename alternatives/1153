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
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDiscoverIssuesReadOnlyFiltersAndAccess(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		project          bool
		projectPages     bool
		projectAbsent    bool
		projectStatus    string
		projectMissing   bool
		projectBadCursor bool
		labels           []string
		snapshots        bool
		closed           bool
		status           int
		missing          bool
		want             error
	}{
		{name: "project status", labels: []string{"bug"}, project: true, projectStatus: "Ready"},
		{name: "project status beyond first membership page", labels: []string{"bug"}, project: true, projectPages: true, projectStatus: "Ready"},
		{name: "project status unset", labels: []string{"bug"}, project: true},
		{name: "issue absent from project", labels: []string{"bug"}, project: true, projectAbsent: true},
		{name: "project inaccessible", labels: []string{"bug"}, project: true, projectMissing: true, want: ErrNotFound},
		{name: "project membership cursor repeated", labels: []string{"bug"}, project: true, projectPages: true, projectBadCursor: true, want: ErrInvalidResponse},
		{name: "omitted labels"},
		{name: "empty labels", labels: []string{}},
		{name: "complete page snapshots", labels: []string{"bug"}, snapshots: true},
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
						States    []string        `json:"states"`
						ProjectV2 bool            `json:"projectV2"`
						ProjectID string          `json:"projectID"`
						ID        string          `json:"id"`
						Labels    json.RawMessage `json:"labels"`
						Cursor    string          `json:"cursor"`
					} `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				projectItems := func(target bool) map[string]any {
					items := []any{map[string]any{"project": map[string]string{"id": "PVT_other"}, "fieldValueByName": map[string]string{"name": "Other status"}}}
					if target && !test.projectAbsent {
						var status any
						if test.projectStatus != "" {
							status = map[string]string{"name": test.projectStatus}
						}
						items = append(items, map[string]any{"project": map[string]string{"id": "PVT_source"}, "fieldValueByName": status})
					}
					return map[string]any{"nodes": items, "pageInfo": map[string]any{"hasNextPage": !target, "endCursor": "project-page2"}}
				}
				if input.Variables.ID != "" {
					if !test.projectPages || input.Variables.ID != "I_12" || input.Variables.Cursor != "project-page2" || strings.Contains(input.Query, "mutation") || !strings.Contains(input.Query, "includeArchived:true") {
						t.Errorf("unexpected membership page: %+v", input)
					}
					if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{"projectItems": projectItems(!test.projectBadCursor)}}}); err != nil {
						t.Error(err)
					}
					return
				}
				if input.Variables.ProjectV2 != test.project || test.project && input.Variables.ProjectID != "PVT_source" || !test.project && input.Variables.ProjectID != "" {
					t.Errorf("optional project read: %+v", input.Variables)
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
				node := map[string]any{"id": "I_12", "number": 12, "url": "https://github.com/example-org/example-project/issues/12", "title": "Issue", "body": strings.Repeat("界", 2000), "closed": test.closed, "labels": map[string]any{"nodes": []any{map[string]string{"name": "bug"}}}}
				if test.project {
					node["projectItems"] = projectItems(!test.projectPages)
					if !strings.Contains(input.Query, `fieldValueByName(name:"Status")`) || !strings.Contains(input.Query, "includeArchived:true") {
						t.Error("project Status read missing")
					}
				}
				if test.snapshots {
					node["createdAt"], node["updatedAt"], node["author"] = "2020-01-01T00:00:00Z", "2021-01-01T00:00:00Z", map[string]string{"login": "source-user"}
					node["comments"] = map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "C_12", "body": "Full discussion", "createdAt": "2020-01-01T00:00:00Z", "updatedAt": "2021-01-01T00:00:00Z", "author": map[string]string{"login": "comment-user"}}}, "pageInfo": map[string]bool{"hasNextPage": false}}
					if !strings.Contains(input.Query, "comments(first:100)") {
						t.Error("page omitted discussion")
					}
				}
				nodes := []any{node}
				total := 1
				nextCursor := ""
				switch string(input.Variables.Labels) {
				case "null":
					nodes = append(nodes, map[string]any{"id": "I_13", "number": 13, "url": "https://github.com/example-org/example-project/issues/13", "title": "Unlabeled issue", "body": "Preview", "closed": false, "labels": map[string]any{"nodes": []any{}}})
					total = 73
					nextCursor = "page3"
				case "[]":
					nodes = []any{}
					total = 0
				}
				response := map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "Example-Org/Example-Project", "issues": map[string]any{"totalCount": total, "nodes": nodes, "pageInfo": map[string]any{"hasNextPage": nextCursor != "", "endCursor": nextCursor}}}}}
				if test.project && !test.projectMissing {
					response["data"].(map[string]any)["projectSource"] = map[string]string{"id": "PVT_source"}
				}
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
			var source *tracker.GitHubProjectV2Source
			if test.project {
				source = &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}
			}
			page, err := client.DiscoverIssues(t.Context(), tracker.GitHubDiscovery{ProjectV2: source, Repository: "example-org/example-project", IncludeClosed: test.closed, Labels: test.labels, Cursor: "page2", Snapshots: test.snapshots})
			if test.want != nil {
				if !errors.Is(err, test.want) || len(page.Issues) != 0 {
					t.Fatalf("page=%#v error=%v", page, err)
				}
			} else {
				if test.snapshots {
					if err != nil || len(page.Issues) != 1 || page.Issues[0].Snapshot == nil {
						t.Fatalf("complete page=%+v %v", page, err)
					}
					snapshot := page.Issues[0].Snapshot
					if snapshot.Body != strings.Repeat("界", 2000) || snapshot.Provenance.ExternalID != "I_12" || snapshot.Provenance.AuthorID != "source-user" || snapshot.Provenance.CreatedAt != time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) || snapshot.Provenance.ObservedAt.IsZero() || len(snapshot.Comments) != 1 || snapshot.Comments[0].Body != "Full discussion" || snapshot.Comments[0].Provenance.ExternalID != "C_12" {
						t.Fatalf("snapshot=%+v", snapshot)
					}
					page.Issues[0].Snapshot = nil
					page.Issues[0].DiscussionCount = 0
				}
				wantPage := tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{{ID: "I_12", Number: 12, URL: "https://github.com/example-org/example-project/issues/12", Title: "Issue", Body: strings.Repeat("界", 1000), Closed: test.closed, Labels: []string{"bug"}}}}
				wantPage.Issues[0].ProjectStatus = test.projectStatus
				if len(test.labels) == 0 {
					wantPage.Total = 73
					wantPage.NextCursor = "page3"
					wantPage.Issues = append(wantPage.Issues, tracker.GitHubIssuePreview{ID: "I_13", Number: 13, URL: "https://github.com/example-org/example-project/issues/13", Title: "Unlabeled issue", Body: "Preview", Labels: []string{}})
				}
				if err != nil || !reflect.DeepEqual(page, wantPage) {
					t.Fatalf("page=%#v want=%#v error=%v", page, wantPage, err)
				}
			}
			expected := int64(1)
			if test.projectPages {
				expected++
			}
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
