package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorFetchCandidateIssuesNormalizesProjectItems(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw1","number":26,"title":"GitHub adapter","state":"CLOSED","stateReason":"COMPLETED","url":"https://github.com/digitaldrywood/detent/issues/26","labels":{"nodes":[{"name":"Bug"},{"name":" enhancement "}]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Ready"},"priorityValue":{"name":"P0"}},{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_kw2","number":27,"title":"Backlog item","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/27","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Backlog"},"priorityValue":{"name":"No priority"}}]}}}}`,
	}, {
		method: http.MethodGet,
		path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
		body:   `[]`,
	}})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
		StateMap:     map[string]string{"Todo": "Ready"},
		PriorityMap:  map[string]*int{"P0": new(1), "No priority": nil},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}

	priority := 1
	want := connector.Issue{
		ID:               "I_kw1",
		Identifier:       "digitaldrywood/detent#26",
		Title:            "GitHub adapter",
		Priority:         &priority,
		State:            "Todo",
		URL:              "https://github.com/digitaldrywood/detent/issues/26",
		Closed:           true,
		ClosedReason:     "COMPLETED",
		BlockedBy:        []connector.BlockedRef{},
		Labels:           []string{"bug", "enhancement"},
		Assignees:        []string{},
		Fields:           map[string]string{},
		AssignedToWorker: true,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("FetchCandidateIssues()[0] = %#v, want %#v", got[0], want)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	variables := requests[0]["variables"].(map[string]any)
	if variables["projectId"] != "PVT_1" {
		t.Fatalf("projectId = %v, want PVT_1", variables["projectId"])
	}
	if variables["first"] != float64(50) {
		t.Fatalf("first = %v, want 50", variables["first"])
	}
	if variables["query"] != "status:Ready" {
		t.Fatalf("query = %v, want status:Ready", variables["query"])
	}
	query := requests[0]["query"].(string)
	for _, forbidden := range []string{
		"author { login }",
		"assignees(",
		"body",
		"closedByPullRequestsReferences",
		"subIssues(",
		"trackedIssues(",
		"fieldValues(",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("project query contains %q:\n%s", forbidden, query)
		}
	}
	if !strings.Contains(query, "labels(first: 20)") {
		t.Fatalf("project query missing labels:\n%s", query)
	}
	if requests[1]["method"] != http.MethodGet || !strings.HasPrefix(requests[1]["path"].(string), "/repos/digitaldrywood/detent/pulls?") {
		t.Fatalf("pull request request = %#v, want REST pulls list", requests[1])
	}
}

func TestAllAssigneeLogins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		assignees nodeConnection[assignee]
		want      []string
	}{
		{
			name:      "returns all nonblank logins in order",
			assignees: nodeConnection[assignee]{Nodes: []assignee{{Login: " worker-1 "}, {Login: ""}, {Login: "worker-2"}}},
			want:      []string{"worker-1", "worker-2"},
		},
		{
			name:      "empty connection returns empty slice",
			assignees: nodeConnection[assignee]{},
			want:      []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := allAssigneeLogins(tt.assignees); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("allAssigneeLogins() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestProjectFieldValues(t *testing.T) {
	t.Parallel()

	number := 42.5
	tests := []struct {
		name   string
		values nodeConnection[projectFieldValue]
		want   map[string]string
	}{
		{
			name: "captures supported field values",
			values: nodeConnection[projectFieldValue]{Nodes: []projectFieldValue{
				{TypeName: "ProjectV2ItemFieldSingleSelectValue", Name: "In Progress", Field: projectField{Name: "Status"}},
				{TypeName: "ProjectV2ItemFieldTextValue", Text: "owner notes", Field: projectField{Name: "Notes"}},
				{TypeName: "ProjectV2ItemFieldNumberValue", Number: &number, Field: projectField{Name: "Rank"}},
				{TypeName: "ProjectV2ItemFieldDateValue", Field: projectField{Name: "Due"}},
				{TypeName: "ProjectV2ItemFieldTextValue", Text: "missing field"},
			}},
			want: map[string]string{"Notes": "owner notes", "Rank": "42.5", "Status": "In Progress"},
		},
		{
			name:   "empty values return empty map",
			values: nodeConnection[projectFieldValue]{},
			want:   map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := projectFieldValues(tt.values); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("projectFieldValues() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestProjectStatusQueryFormatsMappedStatuses(t *testing.T) {
	t.Parallel()

	c := &Connector{stateMap: map[string]string{
		"Todo":         "Ready",
		"Human Review": "In Review",
	}}

	got := c.projectStatusQuery([]string{"Todo", "In Progress", "Human Review", "Rework", "Blocked", "Merging"})
	want := `status:Ready,"In Progress","In Review",Rework,Blocked,Merging`
	if got != want {
		t.Fatalf("projectStatusQuery() = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"Backlog", "Done"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("projectStatusQuery() = %q, want no %s", got, forbidden)
		}
	}

	if got := c.projectStatusQuery([]string{"Backlog"}); got != "" {
		t.Fatalf("projectStatusQuery(Backlog) = %q, want empty query", got)
	}
}

func TestLinkedIssueProjectQueriesStayUnderGitHubNodeLimit(t *testing.T) {
	t.Parallel()

	const githubStaticNodeLimit = 500000
	tests := []struct {
		name     string
		query    string
		required []string
		budget   int
	}{
		{
			name:  "sub issue project fields",
			query: issueSubIssuesQuery,
			required: []string{
				"subIssues(first: $linkedIssuesFirst, after: $after)",
				"projectItems(first: $linkedProjectItemsFirst)",
				"fieldValues(first: $linkedProjectItemFieldValuesFirst)",
			},
			budget: linkedIssuePageSize * linkedIssueProjectItemsPageSize * linkedIssueProjectItemFieldValuesPageSize,
		},
		{
			name:  "tracked issue project fields",
			query: issueTrackedIssuesQuery,
			required: []string{
				"trackedIssues(first: $linkedIssuesFirst, after: $after)",
				"projectItems(first: $linkedProjectItemsFirst)",
				"fieldValues(first: $linkedProjectItemFieldValuesFirst)",
			},
			budget: linkedIssuePageSize * linkedIssueProjectItemsPageSize * linkedIssueProjectItemFieldValuesPageSize,
		},
		{
			name:  "tracked in issue project fields",
			query: issueParentsQuery,
			required: []string{
				"trackedInIssues(first: $linkedIssuesFirst, after: $trackedInAfter)",
				"projectItems(first: $projectItemsFirst)",
				"fieldValues(first: $projectItemFieldValuesFirst)",
			},
			budget: linkedIssuePageSize * projectItemsPerIssue * projectItemFieldValuesPageSize,
		},
		{
			name:  "tracked in issue linked children",
			query: issueParentsQuery,
			required: []string{
				"trackedInIssues(first: $linkedIssuesFirst, after: $trackedInAfter)",
				"subIssues(first: $linkedIssuesFirst)",
				"trackedIssues(first: $linkedIssuesFirst)",
				"projectItems(first: $linkedProjectItemsFirst)",
				"fieldValues(first: $linkedProjectItemFieldValuesFirst)",
			},
			budget: linkedIssuePageSize * linkedIssuePageSize * linkedIssueProjectItemsPageSize * linkedIssueProjectItemFieldValuesPageSize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.budget >= githubStaticNodeLimit {
				t.Fatalf("GraphQL static node budget = %d, want < %d", tt.budget, githubStaticNodeLimit)
			}
			for _, want := range tt.required {
				if !strings.Contains(tt.query, want) {
					t.Fatalf("query missing %q:\n%s", want, tt.query)
				}
			}
		})
	}
}

func TestConnectorFetchCandidateIssuesRequestsRateLimitSnapshot(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
	}})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
	})

	if _, err := c.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	query := requests[0]["query"].(string)
	for _, want := range []string{"rateLimit", "remaining", "resetAt", "cost"} {
		if !strings.Contains(query, want) {
			t.Fatalf("project items query missing %q:\n%s", want, query)
		}
	}
}

func TestConnectorFetchIssuesByStatesDefaultsBlankProjectStatusesToBacklog(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_blank","content":{"__typename":"Issue","id":"I_blank","number":30,"title":"Blank status","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/30","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":null,"priorityValue":null},{"id":"PVTI_todo","content":{"__typename":"Issue","id":"I_todo","number":31,"title":"Ready status","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/31","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"node":{"field":{"id":"PVTSSF_status","options":[{"id":"OPT_backlog","name":"Backlog"},{"id":"OPT_todo","name":"Todo"}]}}}}`,
		},
		{
			body: `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_blank"}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:    "PVT_1",
		ActiveStates:   []string{"Todo"},
		ObservedStates: []string{"Backlog"},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Backlog"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}
	if got[0].ID != "I_blank" || got[0].State != "Backlog" {
		t.Fatalf("defaulted issue = %#v, want blank issue in Backlog", got[0])
	}

	requests := waitForGraphQLRequests(t, server, 3)
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	updateVariables := requestVariables(t, requests[2])
	if updateVariables["projectId"] != "PVT_1" ||
		updateVariables["itemId"] != "PVTI_blank" ||
		updateVariables["fieldId"] != "PVTSSF_status" ||
		updateVariables["optionId"] != "OPT_backlog" {
		t.Fatalf("update variables = %#v, want blank item moved to Backlog", updateVariables)
	}
}

func TestConnectorFetchCandidateIssuesDoesNotBlockOnBlankProjectStatusDefaulting(t *testing.T) {
	t.Parallel()

	releaseDefaultWrite := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseDefaultWrite)
		})
	}
	defer release()
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_blank","content":{"__typename":"Issue","id":"I_blank","number":30,"title":"Blank status","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/30","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":null,"priorityValue":null}]}}}}`,
		},
		{
			release: releaseDefaultWrite,
			body:    `{"data":{"node":{"field":{"id":"PVTSSF_status","options":[{"id":"OPT_backlog","name":"Backlog"},{"id":"OPT_todo","name":"Todo"}]}}}}`,
		},
		{
			body: `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_blank"}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
	})

	type result struct {
		issues []connector.Issue
		err    error
	}
	results := make(chan result, 1)
	go func() {
		issues, err := c.FetchCandidateIssues(context.Background())
		results <- result{issues: issues, err: err}
	}()

	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("FetchCandidateIssues() error = %v", result.err)
		}
		if len(result.issues) != 0 {
			t.Fatalf("FetchCandidateIssues() len = %d, want 0", len(result.issues))
		}
	case <-time.After(200 * time.Millisecond):
		release()
		result := <-results
		t.Fatalf("FetchCandidateIssues() blocked on default status write; issues = %#v error = %v", result.issues, result.err)
	}

	release()
	requests := waitForGraphQLRequests(t, server, 3)
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	updateVariables := requestVariables(t, requests[2])
	if updateVariables["itemId"] != "PVTI_blank" || updateVariables["optionId"] != "OPT_backlog" {
		t.Fatalf("update variables = %#v, want blank item moved to Backlog", updateVariables)
	}
}

func TestConnectorFetchIssuesByStatesIgnoresBlankStatusDefaultWriteFailure(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_blank","content":{"__typename":"Issue","id":"I_blank","number":30,"title":"Blank status","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/30","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":null,"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"node":{"field":{"id":"PVTSSF_status","options":[{"id":"OPT_todo","name":"Todo"}]}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:    "PVT_1",
		ObservedStates: []string{"Backlog"},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Backlog"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}
	if got[0].ID != "I_blank" || got[0].State != "Backlog" {
		t.Fatalf("defaulted issue = %#v, want blank issue in Backlog", got[0])
	}

	requests := waitForGraphQLRequests(t, server, 2)
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
}

func TestConnectorFetchCandidateIssuesLeavesDependencyResolutionForHydration(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_candidate","number":26,"title":"Candidate","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/26","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Ready"},"priorityValue":null},{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_done","number":24,"title":"Done blocker","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/24","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Done"},"priorityValue":null},{"id":"PVTI_3","content":{"__typename":"Issue","id":"I_progress","number":25,"title":"Active blocker","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/25","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Working"},"priorityValue":null}]}}}}`,
	}, {
		method: http.MethodGet,
		path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
		body:   `[]`,
	}})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
		StateMap: map[string]string{
			"Todo":        "Ready",
			"In Progress": "Working",
		},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}

	if len(got[0].BlockedBy) != 0 {
		t.Fatalf("BlockedBy = %#v, want no dependency graph from lightweight poll", got[0].BlockedBy)
	}
}

func TestParseBlockedByRecognizesIssueReferences(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"Depends on: #24",
		"Blocked by: digitaldrywood/agent-runtime#25",
		"Depends on: https://github.com/digitaldrywood/detent/issues/26",
		"Depends on: https://github.com/digitaldrywood/detent/issues/26 and #24",
		"Mention only: #27",
	}, "\n")

	got := parseBlockedBy(body, "digitaldrywood/detent")
	want := []connector.BlockedRef{
		{Identifier: "digitaldrywood/detent#24"},
		{Identifier: "digitaldrywood/agent-runtime#25"},
		{Identifier: "digitaldrywood/detent#26"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseBlockedBy() = %#v, want %#v", got, want)
	}
}

func TestConnectorFetchCandidateIssuesCapturesLinkedChildIssues(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_epic","content":{"__typename":"Issue","id":"I_epic","number":258,"title":"Epic: release readiness","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/258","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
	}, {
		method: http.MethodGet,
		path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
		body:   `[]`,
	}})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}

	if got[0].ChildIssues != nil {
		t.Fatalf("ChildIssues = %#v, want nil from lightweight poll", got[0].ChildIssues)
	}

	query := server.requests()[0]["query"].(string)
	for _, forbidden := range []string{
		"subIssues(",
		"trackedIssues(",
		"fieldValues(",
	} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("project query contains %q:\n%s", forbidden, query)
		}
	}
	if !strings.Contains(query, "labels(first: 20)") {
		t.Fatalf("project query missing labels:\n%s", query)
	}
}

func TestConnectorFetchCandidateIssuesAttachesPullRequestByBranchPrefix(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"First issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null},{"id":"PVTI_18","content":{"__typename":"Issue","id":"I_18","number":18,"title":"Prefix neighbor","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/18","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","updated_at":"2026-06-05T11:30:00Z","head":{"ref":"detent/digitaldrywood_detent_182_followup","sha":"sha-187"}},{"number":188,"html_url":"https://github.com/digitaldrywood/detent/pull/188","state":"closed","head":{"ref":"detent/digitaldrywood_detent_181","sha":"sha-188"},"merged_at":"2026-06-01T00:00:00Z"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/187",
			body:   `{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","updated_at":"2026-06-05T11:30:00Z","head":{"ref":"detent/digitaldrywood_detent_182_followup","sha":"sha-187"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-187/check-runs?per_page=100",
			body:   `{"check_runs":[]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-187/statuses?per_page=100",
			body:   `[{"context":"ci/build","state":"success","created_at":"2026-06-05T11:00:00Z"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/187/reviews?per_page=100",
			body:   `[{"body":"[P1] Stale finding on the previous review.","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"sha-187","submitted_at":"2026-06-05T10:00:00Z"},{"body":"No blocking findings on the current head.","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"sha-187","submitted_at":"2026-06-05T11:00:00Z"}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 2", len(got))
	}

	byID := map[string]connector.Issue{}
	for _, issue := range got {
		byID[issue.ID] = issue
	}
	pr := byID["I_182"].PullRequest
	if pr == nil {
		t.Fatal("I_182 PullRequest = nil, want matching open PR")
	}
	if pr.Number != 187 || pr.State != "OPEN" || pr.BranchName != "detent/digitaldrywood_detent_182_followup" || pr.CIStatus != "pass" || pr.CodexReviewState != "COMMENTED" {
		t.Fatalf("I_182 PullRequest = %#v, want PR 187 open followup", pr)
	}
	wantActivityAt := time.Date(2026, 6, 5, 11, 30, 0, 0, time.UTC)
	if pr.ActivityAt == nil || !pr.ActivityAt.Equal(wantActivityAt) {
		t.Fatalf("I_182 PullRequest.ActivityAt = %v, want %v", pr.ActivityAt, wantActivityAt)
	}
	wantReviewSubmittedAt := time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC)
	if pr.CodexReviewSubmittedAt == nil || !pr.CodexReviewSubmittedAt.Equal(wantReviewSubmittedAt) {
		t.Fatalf("I_182 PullRequest.CodexReviewSubmittedAt = %v, want %v", pr.CodexReviewSubmittedAt, wantReviewSubmittedAt)
	}
	if len(pr.CodexReviewFindings) != 0 {
		t.Fatalf("I_182 PullRequest.CodexReviewFindings = %#v, want none", pr.CodexReviewFindings)
	}
	if byID["I_18"].PullRequest != nil {
		t.Fatalf("I_18 PullRequest = %#v, want nil", byID["I_18"].PullRequest)
	}
}

func TestConnectorFetchIssuesByStatesAttachesLinkedPullRequestBeforeBranchPrefix(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_370","content":{"__typename":"Issue","id":"I_370","number":370,"title":"Linked PR issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/370","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"bug"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":375,"url":"https://github.com/digitaldrywood/detent/pull/375","state":"CLOSED","repository":{"nameWithOwner":"digitaldrywood/detent"}},{"number":376,"url":"https://github.com/corylanou/detent/pull/376","state":"OPEN","repository":{"nameWithOwner":"corylanou/detent"}}]}},"statusValue":{"name":"Reviewing"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/corylanou/detent/pulls/376",
			body:   `{"number":376,"html_url":"https://github.com/corylanou/detent/pull/376","state":"open","head":{"ref":"detent/detent-digitaldrywood_detent_370-e71678a9ca7e","sha":"sha-376"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/corylanou/detent/commits/sha-376/check-runs?per_page=100",
			body:   `{"check_runs":[{"status":"completed","conclusion":"success"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/corylanou/detent/commits/sha-376/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/corylanou/detent/pulls/376/reviews?per_page=100",
			body:   `[{"body":"No blocking findings.","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"sha-376","submitted_at":"2026-06-05T11:00:00Z"}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap: map[string]string{
			"Human Review": "Reviewing",
		},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}

	pr := got[0].PullRequest
	if pr == nil {
		t.Fatal("PullRequest = nil, want linked PR")
	}
	if pr.Number != 376 || pr.URL != "https://github.com/corylanou/detent/pull/376" || pr.State != "OPEN" || pr.BranchName != "detent/detent-digitaldrywood_detent_370-e71678a9ca7e" || pr.CIStatus != "pass" || pr.CodexReviewState != "COMMENTED" {
		t.Fatalf("PullRequest = %#v, want linked PR 376 with hydrated status", pr)
	}
	if got[0].PRRepository != "corylanou/detent" {
		t.Fatalf("PRRepository = %q, want corylanou/detent", got[0].PRRepository)
	}

	requests := server.requests()
	if len(requests) != 5 {
		t.Fatalf("request count = %d, want observed query plus linked PR status requests", len(requests))
	}
	query := requests[0]["query"].(string)
	if !strings.Contains(query, "closedByPullRequestsReferences") {
		t.Fatalf("observed status query does not request linked pull requests:\n%s", query)
	}
	if !strings.Contains(query, "nodes { number url state repository { nameWithOwner } }") {
		t.Fatalf("observed status query does not request linked pull request states:\n%s", query)
	}
	for _, request := range requests {
		path, _ := request["path"].(string)
		if strings.Contains(path, "/pulls?") {
			t.Fatalf("request path = %q, want linked PR path without repository-wide pull list", path)
		}
	}
}

func TestConnectorFetchCandidateIssuesPaginatesPullRequestStatusRESTEndpoints(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"First issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-187"}}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/187",
			body:   `{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-187"}}`,
		},
		{
			method:  http.MethodGet,
			path:    "/repos/digitaldrywood/detent/commits/sha-187/check-runs?per_page=100",
			headers: map[string]string{"Link": `</repos/digitaldrywood/detent/commits/sha-187/check-runs?per_page=100&page=2>; rel="next"`},
			body:    `{"check_runs":[]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-187/check-runs?per_page=100&page=2",
			body:   `{"check_runs":[{"status":"completed","conclusion":"success"}]}`,
		},
		{
			method:  http.MethodGet,
			path:    "/repos/digitaldrywood/detent/commits/sha-187/statuses?per_page=100",
			headers: map[string]string{"Link": `</repos/digitaldrywood/detent/commits/sha-187/statuses?per_page=100&page=2>; rel="next"`},
			body:    `[{"context":"ci/build","state":"success","created_at":"2026-06-05T11:00:00Z"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-187/statuses?per_page=100&page=2",
			body:   `[{"context":"ci/build","state":"failure","created_at":"2026-06-05T12:00:00Z"}]`,
		},
		{
			method:  http.MethodGet,
			path:    "/repos/digitaldrywood/detent/pulls/187/reviews?per_page=100",
			headers: map[string]string{"Link": `</repos/digitaldrywood/detent/pulls/187/reviews?per_page=100&page=2>; rel="next"`},
			body:    `[{"body":"No blocking findings.","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"sha-187","submitted_at":"2026-06-05T10:00:00Z"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/187/reviews?per_page=100&page=2",
			body:   `[{"body":"[P1] Later paged finding.","html_url":"https://github.com/digitaldrywood/detent/pull/187#pullrequestreview-1","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"sha-187","submitted_at":"2026-06-05T12:00:00Z"}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}

	pr := got[0].PullRequest
	if pr == nil {
		t.Fatal("PullRequest = nil, want matching PR")
	}
	if pr.CIStatus != "fail" || pr.CodexReviewState != "P1" {
		t.Fatalf("PullRequest status = CI %q review %q, want fail/P1", pr.CIStatus, pr.CodexReviewState)
	}
	if len(pr.CodexReviewFindings) != 1 ||
		pr.CodexReviewFindings[0].Body != "[P1] Later paged finding." ||
		pr.CodexReviewFindings[0].URL != "https://github.com/digitaldrywood/detent/pull/187#pullrequestreview-1" {
		t.Fatalf("PullRequest.CodexReviewFindings = %#v, want P1 review finding", pr.CodexReviewFindings)
	}

	requests := server.requests()
	if len(requests) != 9 {
		t.Fatalf("request count = %d, want project query plus paged PR REST requests", len(requests))
	}
}

func TestConnectorCachesPullRequestStatusByHeadSHA(t *testing.T) {
	t.Parallel()

	projectBody := `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"First issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`
	pullsBody := `[{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-187"}}]`
	pullBody := `{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-187"}}`
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all", body: pullsBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/187", body: pullBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/sha-187/check-runs?per_page=100", body: `{"check_runs":[{"status":"completed","conclusion":"success"}]}`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/sha-187/statuses?per_page=100", body: `[]`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/187/reviews?per_page=100", body: `[]`},
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all", body: pullsBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/187", body: pullBody},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
	})

	for range 2 {
		got, err := c.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues() error = %v", err)
		}
		if len(got) != 1 || got[0].PullRequest == nil || got[0].PullRequest.CIStatus != "pass" {
			t.Fatalf("FetchCandidateIssues() = %#v, want cached hydrated PR", got)
		}
	}

	requests := server.requests()
	if len(requests) != 9 {
		t.Fatalf("request count = %d, want second fetch to reuse PR status cache", len(requests))
	}
	for _, pattern := range []string{"/check-runs", "/statuses", "/reviews"} {
		count := 0
		for _, request := range requests {
			path, _ := request["path"].(string)
			if strings.Contains(path, pattern) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s request count = %d, want 1; requests = %#v", pattern, count, requests)
		}
	}
}

func TestConnectorFetchCandidateIssuesMarksBranchPullRequestHydrationUnavailableWhenRESTBudgetReserved(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/rate_limit",
			headers: map[string]string{
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Remaining": "900",
				"X-RateLimit-Used":      "4100",
				"X-RateLimit-Resource":  "core",
			},
			body: `{}`,
		},
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"First issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:             "PVT_1",
		ActiveStates:            []string{"Todo"},
		RESTMinRemainingReserve: 1000,
	})
	if err := c.client.REST(context.Background(), http.MethodGet, "/rate_limit", nil, nil); err != nil {
		t.Fatalf("REST() seed rate limit error = %v", err)
	}

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}
	if got[0].PullRequest == nil {
		t.Fatal("PullRequest = nil, want hydration-unavailable marker")
	}
	if got[0].PullRequest.HydrationUnavailableReason != "rest_budget_reserved" {
		t.Fatalf("HydrationUnavailableReason = %q, want rest_budget_reserved", got[0].PullRequest.HydrationUnavailableReason)
	}
	if got[0].PullRequest.Number != 0 {
		t.Fatalf("PullRequest.Number = %d, want unknown PR number while branch matching is skipped", got[0].PullRequest.Number)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want rate limit seed plus project query", len(requests))
	}
	for _, request := range requests {
		path, _ := request["path"].(string)
		if strings.Contains(path, "/pulls?") {
			t.Fatalf("request path = %q, want pull request list skipped", path)
		}
	}
	usage := c.client.FlushRESTRateLimitUsage()
	if !usage.RateLimited {
		t.Fatalf("RESTRateLimitUsage.RateLimited = false, want reserved budget throttle")
	}
	if got := restEndpointUsageCount(usage.Requests, "pull requests"); got != 1 {
		t.Fatalf("pull requests usage count = %d, want synthetic throttle; usage = %#v", got, usage.Requests)
	}
}

func TestConnectorFetchCandidateIssuesStopsBranchPullRequestHydrationAfterSecondaryThrottle(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 13, 0, 0, 0, time.UTC)
	projectBody := `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"First issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null},{"id":"PVTI_183","content":{"__typename":"Issue","id":"I_183","number":183,"title":"Second issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/183","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: projectBody},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-187"}},{"number":188,"html_url":"https://github.com/digitaldrywood/detent/pull/188","state":"open","head":{"ref":"detent/digitaldrywood_detent_183","sha":"sha-188"}}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/187",
			body:   `{"number":187,"html_url":"https://github.com/digitaldrywood/detent/pull/187","state":"open","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-187"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-187/check-runs?per_page=100",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After":           "120",
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Used":      "264",
				"X-RateLimit-Remaining": "4736",
				"X-RateLimit-Resource":  "core",
			},
			body: `{"message":"secondary rate limit"}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
		Now: func() time.Time {
			return now
		},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 2", len(got))
	}

	byID := map[string]connector.Issue{}
	for _, issue := range got {
		byID[issue.ID] = issue
	}
	for _, id := range []string{"I_182", "I_183"} {
		pr := byID[id].PullRequest
		if pr == nil {
			t.Fatalf("%s PullRequest = nil, want hydration marker", id)
		}
		if pr.HydrationUnavailableReason != connector.PullRequestHydrationReasonSecondaryThrottled {
			t.Fatalf("%s HydrationUnavailableReason = %q, want secondary_throttled", id, pr.HydrationUnavailableReason)
		}
		if pr.HydrationNextRetryAt == nil || !pr.HydrationNextRetryAt.After(now.Add(120*time.Second)) {
			t.Fatalf("%s HydrationNextRetryAt = %v, want retry-after plus jitter", id, pr.HydrationNextRetryAt)
		}
	}
	if byID["I_182"].PullRequest.Number != 187 {
		t.Fatalf("I_182 PullRequest.Number = %d, want hydrated PR number 187", byID["I_182"].PullRequest.Number)
	}
	if byID["I_183"].PullRequest.Number != 0 {
		t.Fatalf("I_183 PullRequest.Number = %d, want no second PR hydration after circuit trip", byID["I_183"].PullRequest.Number)
	}

	requests := server.requests()
	checkRunRequests := 0
	for _, request := range requests {
		path, _ := request["path"].(string)
		if strings.Contains(path, "/pulls/188") || strings.Contains(path, "sha-188") {
			t.Fatalf("unexpected request after circuit trip: %#v", request)
		}
		if strings.Contains(path, "/check-runs") {
			checkRunRequests++
		}
	}
	if checkRunRequests != 1 {
		t.Fatalf("check-runs requests = %d, want only first PR status attempt; requests = %#v", checkRunRequests, requests)
	}
}

func TestConnectorFetchIssuesByStatesRechecksPullRequestStatusForPromotion(t *testing.T) {
	t.Parallel()

	projectBody := `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_401","content":{"__typename":"Issue","id":"I_401","number":401,"title":"Human review issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/401","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":411,"url":"https://github.com/digitaldrywood/detent/pull/411","state":"OPEN","repository":{"nameWithOwner":"digitaldrywood/detent"}}]}},"statusValue":{"name":"Human Review"},"priorityValue":null}]}}}}`
	pullBody := `{"number":411,"html_url":"https://github.com/digitaldrywood/detent/pull/411","state":"open","head":{"ref":"detent/digitaldrywood_detent_401","sha":"head-current"}}`
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411", body: pullBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100", body: `{"check_runs":[{"status":"completed","conclusion":"success"}]}`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/statuses?per_page=100", body: `[]`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411/reviews?per_page=100", body: `[]`},
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411", body: pullBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100", body: `{"check_runs":[{"status":"completed","conclusion":"failure"}]}`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/statuses?per_page=100", body: `[]`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411/reviews?per_page=100", body: `[{"body":"[P1] New blocking finding.","html_url":"https://github.com/digitaldrywood/detent/pull/411#pullrequestreview-3","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"head-current","submitted_at":"2026-06-24T22:30:00Z"}]`},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	first, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() first error = %v", err)
	}
	if len(first) != 1 || first[0].PullRequest == nil {
		t.Fatalf("FetchIssuesByStates() first = %#v, want hydrated PR", first)
	}
	if first[0].PullRequest.CIStatus != "pass" || first[0].PullRequest.CodexReviewState != "" {
		t.Fatalf("first PullRequest = %#v, want pass with no review finding", first[0].PullRequest)
	}

	second, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() second error = %v", err)
	}
	if len(second) != 1 || second[0].PullRequest == nil {
		t.Fatalf("FetchIssuesByStates() second = %#v, want hydrated PR", second)
	}
	pr := second[0].PullRequest
	if pr.CIStatus != "fail" || pr.CodexReviewState != "P1" {
		t.Fatalf("second PullRequest status = CI %q review %q, want fail/P1", pr.CIStatus, pr.CodexReviewState)
	}
	if len(pr.CodexReviewFindings) != 1 || pr.CodexReviewFindings[0].Body != "[P1] New blocking finding." {
		t.Fatalf("second CodexReviewFindings = %#v, want new P1 finding", pr.CodexReviewFindings)
	}

	requests := server.requests()
	for _, pattern := range []string{"/check-runs", "/statuses", "/reviews"} {
		count := 0
		for _, request := range requests {
			path, _ := request["path"].(string)
			if strings.Contains(path, pattern) {
				count++
			}
		}
		if count != 2 {
			t.Fatalf("%s request count = %d, want fresh status fetch each call; requests = %#v", pattern, count, requests)
		}
	}
}

func TestConnectorFetchIssuesByStatesUsesCachedPullRequestStatusAfterRateLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	projectBody := `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_401","content":{"__typename":"Issue","id":"I_401","number":401,"title":"Human review issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/401","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":411,"url":"https://github.com/digitaldrywood/detent/pull/411","state":"OPEN","repository":{"nameWithOwner":"digitaldrywood/detent"}}]}},"statusValue":{"name":"Human Review"},"priorityValue":null}]}}}}`
	pullBody := `{"number":411,"html_url":"https://github.com/digitaldrywood/detent/pull/411","state":"open","head":{"ref":"detent/digitaldrywood_detent_401","sha":"head-current"}}`
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411", body: pullBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100", body: `{"check_runs":[{"status":"completed","conclusion":"success"}]}`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/statuses?per_page=100", body: `[]`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411/reviews?per_page=100", body: `[]`},
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411", body: pullBody},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After":           "120",
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Used":      "264",
				"X-RateLimit-Remaining": "4736",
				"X-RateLimit-Resource":  "core",
			},
			body: `{"message":"secondary rate limit"}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		Now: func() time.Time {
			return now
		},
	})

	first, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() first error = %v", err)
	}
	if len(first) != 1 || first[0].PullRequest == nil || first[0].PullRequest.CIStatus != "pass" {
		t.Fatalf("FetchIssuesByStates() first = %#v, want cached hydrated PR", first)
	}

	second, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() second error = %v", err)
	}
	if len(second) != 1 || second[0].PullRequest == nil {
		t.Fatalf("FetchIssuesByStates() second = %#v, want hydrated PR", second)
	}
	pr := second[0].PullRequest
	if pr.HydrationUnavailableReason != "" {
		t.Fatalf("HydrationUnavailableReason = %q, want cached status without unavailable marker", pr.HydrationUnavailableReason)
	}
	if pr.HydrationDegradedReason != connector.PullRequestHydrationReasonStaleCachedPullData {
		t.Fatalf("HydrationDegradedReason = %q, want stale cached pull request marker", pr.HydrationDegradedReason)
	}
	if pr.HydrationNextRetryAt == nil || !pr.HydrationNextRetryAt.After(now.Add(120*time.Second)) {
		t.Fatalf("HydrationNextRetryAt = %v, want retry-after plus jitter", pr.HydrationNextRetryAt)
	}
	if pr.CIStatus != "pass" {
		t.Fatalf("CIStatus = %q, want cached pass", pr.CIStatus)
	}
}

func TestConnectorFetchIssuesByStatesCircuitBreaksPullRequestHydrationAfterSecondaryThrottle(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 11, 0, 0, 0, time.UTC)
	projectBody := `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_401","content":{"__typename":"Issue","id":"I_401","number":401,"title":"Human review issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/401","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":411,"url":"https://github.com/digitaldrywood/detent/pull/411","state":"OPEN","repository":{"nameWithOwner":"digitaldrywood/detent"}}]}},"statusValue":{"name":"Human Review"},"priorityValue":null}]}}}}`
	pullBody := `{"number":411,"html_url":"https://github.com/digitaldrywood/detent/pull/411","state":"open","head":{"ref":"detent/digitaldrywood_detent_401","sha":"head-current"}}`
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411", body: pullBody},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After":           "120",
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Used":      "264",
				"X-RateLimit-Remaining": "4736",
				"X-RateLimit-Resource":  "core",
			},
			body: `{"message":"secondary rate limit"}`,
		},
		{body: projectBody},
		{body: projectBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411", body: pullBody},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100", body: `{"check_runs":[{"status":"completed","conclusion":"success"}]}`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/commits/head-current/statuses?per_page=100", body: `[]`},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/pulls/411/reviews?per_page=100", body: `[]`},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		Now: func() time.Time {
			return now
		},
	})

	first, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() first error = %v", err)
	}
	if len(first) != 1 || first[0].PullRequest == nil {
		t.Fatalf("FetchIssuesByStates() first = %#v, want PR hydration marker", first)
	}
	pr := first[0].PullRequest
	if pr.HydrationUnavailableReason != "secondary_throttled" {
		t.Fatalf("HydrationUnavailableReason = %q, want secondary_throttled", pr.HydrationUnavailableReason)
	}
	if pr.HydrationNextRetryAt == nil {
		t.Fatal("HydrationNextRetryAt = nil, want retry deadline")
	}
	retryAt := *pr.HydrationNextRetryAt
	if !retryAt.After(now.Add(120*time.Second)) || retryAt.After(now.Add(151*time.Second)) {
		t.Fatalf("HydrationNextRetryAt = %v, want retry-after plus jitter", retryAt)
	}

	beforeCooldownRequests := len(server.requests())
	second, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() second error = %v", err)
	}
	if len(second) != 1 || second[0].PullRequest == nil {
		t.Fatalf("FetchIssuesByStates() second = %#v, want PR hydration marker", second)
	}
	if second[0].PullRequest.HydrationUnavailableReason != "secondary_throttled" {
		t.Fatalf("second HydrationUnavailableReason = %q, want secondary_throttled", second[0].PullRequest.HydrationUnavailableReason)
	}
	if len(server.requests()) != beforeCooldownRequests+1 {
		t.Fatalf("request count after cooldown skip = %d, want one project refresh; requests = %#v", len(server.requests()), server.requests())
	}

	now = retryAt.Add(time.Second)
	third, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() third error = %v", err)
	}
	if len(third) != 1 || third[0].PullRequest == nil {
		t.Fatalf("FetchIssuesByStates() third = %#v, want hydrated PR", third)
	}
	pr = third[0].PullRequest
	if pr.HydrationUnavailableReason != "" || pr.HydrationNextRetryAt != nil {
		t.Fatalf("third hydration state = reason %q retry %v, want cleared", pr.HydrationUnavailableReason, pr.HydrationNextRetryAt)
	}
	if pr.CIStatus != "pass" {
		t.Fatalf("third CIStatus = %q, want pass", pr.CIStatus)
	}

	requests := server.requests()
	checkRunRequests := 0
	for _, request := range requests {
		path, _ := request["path"].(string)
		if strings.Contains(path, "/check-runs") {
			checkRunRequests++
		}
	}
	if checkRunRequests != 2 {
		t.Fatalf("check-runs requests = %d, want one failing call and one retry after cooldown; requests = %#v", checkRunRequests, requests)
	}
}

func TestConnectorFetchIssuesByStatesMarksLinkedPullRequestHydrationSecondaryThrottled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	projectBody := `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_401","content":{"__typename":"Issue","id":"I_401","number":401,"title":"Human review issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/401","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":411,"url":"https://github.com/digitaldrywood/detent/pull/411","state":"OPEN","repository":{"nameWithOwner":"digitaldrywood/detent"}}]}},"statusValue":{"name":"Human Review"},"priorityValue":null}]}}}}`
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: projectBody},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/411",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After":           "120",
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Used":      "264",
				"X-RateLimit-Remaining": "4736",
				"X-RateLimit-Resource":  "core",
			},
			body: `{"message":"secondary rate limit"}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		Now: func() time.Time {
			return now
		},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}
	pr := got[0].PullRequest
	if pr == nil {
		t.Fatal("PullRequest = nil, want retained linked PR shell")
	}
	if pr.Number != 411 {
		t.Fatalf("PullRequest.Number = %d, want 411", pr.Number)
	}
	if pr.HydrationUnavailableReason != connector.PullRequestHydrationReasonSecondaryThrottled {
		t.Fatalf("HydrationUnavailableReason = %q, want secondary_throttled", pr.HydrationUnavailableReason)
	}
	if pr.HydrationNextRetryAt == nil || !pr.HydrationNextRetryAt.After(now.Add(120*time.Second)) {
		t.Fatalf("HydrationNextRetryAt = %v, want retry-after plus jitter", pr.HydrationNextRetryAt)
	}
	if got[0].PRNumber == nil || *got[0].PRNumber != 411 {
		t.Fatalf("PRNumber = %v, want 411", got[0].PRNumber)
	}
}

func TestConnectorFetchIssuesByStatesAttachesPipelinePullRequest(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"Review issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":{"name":"Reviewing"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[{"number":190,"html_url":"https://github.com/digitaldrywood/detent/pull/190","state":"open","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-190"}}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/190",
			body:   `{"number":190,"html_url":"https://github.com/digitaldrywood/detent/pull/190","state":"open","mergeable_state":"dirty","head":{"ref":"detent/digitaldrywood_detent_182","sha":"sha-190"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-190/check-runs?per_page=100",
			body:   `{"check_runs":[{"name":"Verify (ubuntu-latest)","status":"completed","conclusion":"success","created_at":"2026-06-05T10:59:00Z","started_at":"2026-06-05T11:00:00Z","completed_at":"2026-06-05T11:03:00Z"},{"name":"GoReleaser Snapshot","status":"completed","conclusion":"failure","created_at":"2026-06-05T11:00:30Z","started_at":"2026-06-05T11:03:30Z","completed_at":"2026-06-05T11:11:30Z"},{"name":"Windows Core","status":"in_progress","conclusion":"","created_at":"2026-06-05T11:04:00Z","started_at":"2026-06-05T11:05:00Z","completed_at":null}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-190/statuses?per_page=100",
			body:   `[{"context":"ci/build","state":"success","created_at":"2026-06-05T11:00:00Z"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/190/reviews?per_page=100",
			body:   `[{"body":"[P1] Unsafe migration.","html_url":"https://github.com/digitaldrywood/detent/pull/190#pullrequestreview-2","state":"COMMENTED","user":{"login":"codex"},"commit_id":"sha-190","submitted_at":"2026-06-05T11:00:00Z"}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap: map[string]string{
			"Human Review": "Reviewing",
		},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}
	pr := got[0].PullRequest
	if pr == nil || pr.Number != 190 || pr.CIStatus != "fail" || pr.CodexReviewState != "P1" {
		t.Fatalf("PullRequest = %#v, want PR 190 with failing CI and P1 review", pr)
	}
	if pr.MergeableState != "dirty" {
		t.Fatalf("MergeableState = %q, want dirty from hydrated PR", pr.MergeableState)
	}
	if pr.CIDurationSeconds != 0 {
		t.Fatalf("CIDurationSeconds = %d, want 0 while checks are running", pr.CIDurationSeconds)
	}
	if pr.CIQueueSeconds != 60 {
		t.Fatalf("CIQueueSeconds = %d, want 60", pr.CIQueueSeconds)
	}
	if len(pr.SlowChecks) != 2 {
		t.Fatalf("SlowChecks len = %d, want 2: %#v", len(pr.SlowChecks), pr.SlowChecks)
	}
	if pr.SlowChecks[0].Name != "GoReleaser Snapshot" || pr.SlowChecks[0].DurationSeconds != 480 || pr.SlowChecks[0].QueueSeconds != 180 {
		t.Fatalf("SlowChecks[0] = %#v, want GoReleaser Snapshot 480s active and 180s queued", pr.SlowChecks[0])
	}
	if pr.SlowChecks[1].Name != "Verify (ubuntu-latest)" || pr.SlowChecks[1].DurationSeconds != 180 || pr.SlowChecks[1].QueueSeconds != 60 {
		t.Fatalf("SlowChecks[1] = %#v, want Verify 180s active and 60s queued", pr.SlowChecks[1])
	}
	if len(pr.RunningChecks) != 1 || pr.RunningChecks[0] != "Windows Core" {
		t.Fatalf("RunningChecks = %#v, want Windows Core", pr.RunningChecks)
	}
	if len(pr.CodexReviewFindings) != 1 ||
		pr.CodexReviewFindings[0].Body != "[P1] Unsafe migration." ||
		pr.CodexReviewFindings[0].URL != "https://github.com/digitaldrywood/detent/pull/190#pullrequestreview-2" {
		t.Fatalf("PullRequest.CodexReviewFindings = %#v, want P1 review finding", pr.CodexReviewFindings)
	}
}

func TestCheckRunTelemetryReportsQueueAndCompletedSpan(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 6, 5, 10, 58, 0, 0, time.UTC)
	start := time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC)
	verifyDone := start.Add(3 * time.Minute)
	snapshotCreated := start.Add(time.Minute)
	snapshotStart := verifyDone.Add(30 * time.Second)
	snapshotDone := snapshotStart.Add(8 * time.Minute)

	summary := checkRunTelemetry([]restCheckRun{
		{Name: "Verify (ubuntu-latest)", Status: "completed", Conclusion: "success", CreatedAt: &created, StartedAt: &start, CompletedAt: &verifyDone},
		{Name: "GoReleaser Snapshot", Status: "completed", Conclusion: "success", CreatedAt: &snapshotCreated, StartedAt: &snapshotStart, CompletedAt: &snapshotDone},
	}, nil)

	if summary.QueueSeconds != 120 {
		t.Fatalf("QueueSeconds = %d, want 120", summary.QueueSeconds)
	}
	if summary.DurationSeconds != 690 {
		t.Fatalf("DurationSeconds = %d, want 690", summary.DurationSeconds)
	}
	if len(summary.SlowChecks) != 2 || summary.SlowChecks[0].Name != "GoReleaser Snapshot" || summary.SlowChecks[0].QueueSeconds != 150 {
		t.Fatalf("SlowChecks = %#v, want snapshot first with queued runtime", summary.SlowChecks)
	}
	if len(summary.RunningChecks) != 0 {
		t.Fatalf("RunningChecks = %#v, want none", summary.RunningChecks)
	}
}

func TestCheckRunTelemetryUsesWorkflowRunTimingForQueue(t *testing.T) {
	t.Parallel()

	runCreated := time.Date(2026, 6, 5, 10, 58, 0, 0, time.UTC)
	runStarted := runCreated.Add(90 * time.Second)
	checkStarted := time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC)
	checkCompleted := checkStarted.Add(3 * time.Minute)

	summary := checkRunTelemetry([]restCheckRun{
		{Name: "Verify (ubuntu-latest)", Status: "completed", Conclusion: "success", StartedAt: &checkStarted, CompletedAt: &checkCompleted},
	}, []restWorkflowRun{
		{ID: 28196652213, CreatedAt: &runCreated, RunStartedAt: &runStarted},
	})

	if summary.QueueSeconds != 90 {
		t.Fatalf("QueueSeconds = %d, want 90", summary.QueueSeconds)
	}
	if summary.DurationSeconds != 180 {
		t.Fatalf("DurationSeconds = %d, want 180", summary.DurationSeconds)
	}
}

func TestCheckRunWorkflowRunIDs(t *testing.T) {
	t.Parallel()

	got := checkRunWorkflowRunIDs([]restCheckRun{
		{DetailsURL: "https://github.com/digitaldrywood/detent/actions/runs/28196652213/job/83525095026"},
		{DetailsURL: "https://github.com/digitaldrywood/detent/actions/runs/28196652213/job/83525095027"},
		{DetailsURL: "https://github.com/digitaldrywood/detent/actions/runs/28196652214"},
		{DetailsURL: "https://example.com/not-actions"},
	})

	want := []int64{28196652213, 28196652214}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkRunWorkflowRunIDs() = %#v, want %#v", got, want)
	}
}

func TestConnectorPullRequestStatusCacheDebugLog(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	c := &Connector{
		pullRequests: newPullRequestStatusCache(time.Minute, func() time.Time { return now }),
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
	repo := pullRequestRepo{Owner: "digitaldrywood", Name: "detent"}
	status := pullRequestStatus{
		ci: pullRequestCI{
			State:         "SUCCESS",
			CheckRunCount: 2,
		},
	}
	c.pullRequests.Set(repo, 726, "head-sha", status)
	pullRequest := pullRequestNode{Number: 726, HeadSHA: "head-sha"}

	if err := c.populatePullRequestStatus(context.Background(), repo, &pullRequest, true); err != nil {
		t.Fatalf("populatePullRequestStatus() error = %v", err)
	}

	logText := logs.String()
	for _, fragment := range []string{
		"github pull request status cache",
		"endpoint_family=pull_request_status_cache",
		"request_purpose=hydrate_pull_request_status",
		"repository=digitaldrywood/detent",
		"pr_number=726",
		"cache_hit=true",
		"avoidable_request=true",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs missing %q:\n%s", fragment, logText)
		}
	}
	if pullRequest.CI.CheckRunCount != 2 {
		t.Fatalf("CheckRunCount = %d, want cached status", pullRequest.CI.CheckRunCount)
	}
}

func TestConnectorFetchIssuesByStatesSurfacesStaleCodexReview(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_401","content":{"__typename":"Issue","id":"I_401","number":401,"title":"Human review issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/401","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"enhancement"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":411,"url":"https://github.com/digitaldrywood/detent/pull/411","state":"OPEN","repository":{"nameWithOwner":"digitaldrywood/detent"}}]}},"statusValue":{"name":"Human Review"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/411",
			body:   `{"number":411,"html_url":"https://github.com/digitaldrywood/detent/pull/411","state":"open","head":{"ref":"detent/digitaldrywood_detent_401","sha":"head-current"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100",
			body:   `{"check_runs":[{"status":"completed","conclusion":"success"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/head-current/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/411/reviews?per_page=100",
			body:   `[{"body":"No blocking findings on an older head.","state":"COMMENTED","user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"head-previous","submitted_at":"2026-06-12T11:40:00Z"}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})
	got, err := c.FetchIssuesByStates(context.Background(), []string{"Human Review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}

	pr := got[0].PullRequest
	if pr == nil {
		t.Fatal("PullRequest = nil, want linked PR")
	}
	if pr.HeadSHA != "head-current" {
		t.Fatalf("HeadSHA = %q, want head-current", pr.HeadSHA)
	}
	if pr.CIStatus != "pass" {
		t.Fatalf("CIStatus = %q, want pass", pr.CIStatus)
	}
	if pr.CodexReviewState != "" || pr.CodexReviewSubmittedAt != nil {
		t.Fatalf("current-head Codex review = %q at %v, want none", pr.CodexReviewState, pr.CodexReviewSubmittedAt)
	}
	if pr.LatestCodexReviewState != "COMMENTED" || pr.LatestCodexReviewCommitSHA != "head-previous" {
		t.Fatalf("latest Codex review = state %q commit %q, want COMMENTED/head-previous", pr.LatestCodexReviewState, pr.LatestCodexReviewCommitSHA)
	}
	wantSubmittedAt := time.Date(2026, 6, 12, 11, 40, 0, 0, time.UTC)
	if pr.LatestCodexReviewSubmittedAt == nil || !pr.LatestCodexReviewSubmittedAt.Equal(wantSubmittedAt) {
		t.Fatalf("LatestCodexReviewSubmittedAt = %v, want %v", pr.LatestCodexReviewSubmittedAt, wantSubmittedAt)
	}
}

func TestConnectorFetchIssuesByStatesLimitStopsAfterSample(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[{"id":"PVTI_370","content":{"__typename":"Issue","id":"I_370","number":370,"title":"Review issue","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/370","repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":371,"url":"https://github.com/digitaldrywood/detent/pull/371"}]}},"statusValue":{"name":"Human Review"},"priorityValue":null},{"id":"PVTI_387","content":{"__typename":"Issue","id":"I_387","number":387,"title":"Review issue 2","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/387","repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":{"name":"Human Review"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/371",
			body:   `{"number":371,"html_url":"https://github.com/digitaldrywood/detent/pull/371","state":"open","head":{"ref":"detent/digitaldrywood_detent_370","sha":"sha-371"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-371/check-runs?per_page=100",
			body:   `{"check_runs":[{"status":"completed","conclusion":"success"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/sha-371/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/371/reviews?per_page=100",
			body:   `[]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssuesByStatesLimit(context.Background(), []string{"Human Review"}, 1)
	if err != nil {
		t.Fatalf("FetchIssuesByStatesLimit() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStatesLimit() len = %d, want 1", len(got))
	}
	if got[0].PRNumber == nil || *got[0].PRNumber != 371 {
		t.Fatalf("PRNumber = %v, want linked PR 371", got[0].PRNumber)
	}
	pr := got[0].PullRequest
	if pr == nil || pr.Number != 371 || pr.CIStatus != "pass" {
		t.Fatalf("PullRequest = %#v, want hydrated linked PR 371 with passing CI", pr)
	}

	requests := server.requests()
	if len(requests) != 5 {
		t.Fatalf("request count = %d, want one project page and linked PR status requests", len(requests))
	}
	if requests[0]["variables"].(map[string]any)["after"] != nil {
		t.Fatalf("first project request after = %v, want nil", requests[0]["variables"].(map[string]any)["after"])
	}
}

func TestBranchMatchesIssuePrefixAcceptsCurrentAgentBranchShape(t *testing.T) {
	t.Parallel()

	prefix := detentIssueBranchPrefix("digitaldrywood/detent#506")
	if prefix != "detent/digitaldrywood_detent_506" {
		t.Fatalf("detentIssueBranchPrefix() = %q, want detent/digitaldrywood_detent_506", prefix)
	}

	for _, branch := range []string{
		"detent/digitaldrywood_detent_506",
		"detent/digitaldrywood_detent_506-fix",
		"detent/detent-digitaldrywood_detent_506-6bd1bec3c6d3",
		"detent/digitaldrywood-digitaldrywood_detent_506-6bd1bec3c6d3",
		"detent/506",
		"detent/506-fix",
	} {
		if !branchMatchesIssuePrefix(branch, prefix) {
			t.Fatalf("branchMatchesIssuePrefix(%q, %q) = false, want true", branch, prefix)
		}
	}

	for _, branch := range []string{
		"detent/digitaldrywood-digitaldrywood_detent_5060-6bd1bec3c6d3",
		"detent/digitaldrywood-digitaldrywood_detent_50-6bd1bec3c6d3",
		"detent/foo-digitaldrywood_detent_506-digitaldrywood_detent_123-6bd1bec3c6d3",
		"detent/digitaldrywood_detent_5060",
	} {
		if branchMatchesIssuePrefix(branch, prefix) {
			t.Fatalf("branchMatchesIssuePrefix(%q, %q) = true, want false", branch, prefix)
		}
	}
}

func TestConnectorFetchCandidateIssuesLimitsPullRequestPagination(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_1","number":1,"title":"Candidate","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/1","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want project query plus pull request page", len(requests))
	}
	if requests[1]["method"] != http.MethodGet || !strings.HasPrefix(requests[1]["path"].(string), "/repos/digitaldrywood/detent/pulls?") {
		t.Fatalf("pull request request = %#v, want REST pulls list", requests[1])
	}
}

func TestConnectorFetchIssuesByStatesFiltersMappedStates(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw1","number":1,"title":"Ready issue","body":"","state":"OPEN","url":"https://github.com/example/repo/issues/1","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"example/repo"}},"statusValue":{"name":"Ready"},"priorityValue":null},{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_kw2","number":2,"title":"Review issue","body":"","state":"OPEN","url":"https://github.com/example/repo/issues/2","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"example/repo"}},"statusValue":{"name":"Reviewing"},"priorityValue":null}]}}}}`,
	}})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap: map[string]string{
			"Todo":         "Ready",
			"Human Review": "Reviewing",
		},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"todo"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if ids := githubIssueIDs(got); !reflect.DeepEqual(ids, []string{"I_kw1"}) {
		t.Fatalf("FetchIssuesByStates() ids = %#v, want [I_kw1]", ids)
	}
	requests := server.requests()
	queryVariables := requests[0]["variables"].(map[string]any)
	if queryVariables["query"] != "status:Ready" {
		t.Fatalf("query = %v, want status:Ready", queryVariables["query"])
	}

	requestsBeforeEmpty := len(server.requests())
	got, err = c.FetchIssuesByStates(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchIssuesByStates(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("FetchIssuesByStates(nil) len = %d, want 0", len(got))
	}
	if len(server.requests()) != requestsBeforeEmpty {
		t.Fatalf("FetchIssuesByStates(nil) made a request")
	}
}

func TestConnectorFetchIssuesByStatesUsesStatusUpdatedAtForStage(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw1","number":1,"title":"Done issue","state":"OPEN","url":"https://github.com/example/repo/issues/1","repository":{"nameWithOwner":"example/repo"}},"statusValue":{"name":"Done","updatedAt":"2026-06-01T12:30:00Z"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}

	stageUpdatedAt := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	if got[0].UpdatedAt != nil {
		t.Fatalf("UpdatedAt = %v, want nil from lightweight poll", got[0].UpdatedAt)
	}
	if got[0].StageUpdatedAt == nil || !got[0].StageUpdatedAt.Equal(stageUpdatedAt) {
		t.Fatalf("StageUpdatedAt = %v, want status updatedAt %v", got[0].StageUpdatedAt, stageUpdatedAt)
	}
}

func TestConnectorFetchIssuesByStatesExtractsWorkpadHumanActionNeeded(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw98","number":98,"title":"Homebrew tap","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/98","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Blocked"},"priorityValue":null}]}}}}`,
		},
		{
			method:  http.MethodGet,
			path:    "/repos/digitaldrywood/detent/issues/98/comments?per_page=100",
			headers: map[string]string{"Link": `</repos/digitaldrywood/detent/issues/98/comments?per_page=100&page=2>; rel="next"`},
			body:    `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/98/comments?per_page=100&page=2",
			body:   `[{"body":"## Codex Workpad\n\n### Plan\n- Check prerequisites.\n\n### Human Action Needed\n- Create public repository ` + "`" + `digitaldrywood/homebrew-tap` + "`" + `.\n- Add repository Actions secret ` + "`" + `HOMEBREW_TAP_GITHUB_TOKEN` + "`" + `.\n\n### Validation Evidence\n- Not run."}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Blocked"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}

	want := "Create public repository `digitaldrywood/homebrew-tap`.; Add repository Actions secret `HOMEBREW_TAP_GITHUB_TOKEN`."
	if got[0].BlockerReason != want {
		t.Fatalf("BlockerReason = %q, want %q", got[0].BlockerReason, want)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	if strings.Contains(requests[0]["query"].(string), "comments") {
		t.Fatalf("project query = %q, want no comments", requests[0]["query"])
	}
	if requests[1]["method"] != http.MethodGet || requests[1]["path"] != "/repos/digitaldrywood/detent/issues/98/comments?per_page=100" {
		t.Fatalf("comments request = %#v, want REST issue comments", requests[1])
	}
}

func TestConnectorFetchIssuesByStatesExtractsWorkpadBlockedByRefs(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw416","number":416,"title":"Blocked workpad dependency","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/416","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Blocked"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/416/comments?per_page=100",
			body:   `[{"body":"## Codex Workpad\n\n### Blockers\n- Blocked by: #415\n- Waiting for digitaldrywood/agent-runtime#25\n- Human action needed: merge #415, then move #416 back to Todo.\n\n### Validation\n- Pending."}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Blocked"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}

	want := []connector.BlockedRef{
		{Identifier: "digitaldrywood/detent#415"},
		{Identifier: "digitaldrywood/agent-runtime#25"},
	}
	if !reflect.DeepEqual(got[0].BlockedBy, want) {
		t.Fatalf("BlockedBy = %#v, want %#v", got[0].BlockedBy, want)
	}
}

func TestConnectorFetchIssuesByStatesIgnoresHumanActionIssueMentions(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw417","number":417,"title":"Human blocked reference","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/417","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Blocked"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/417/comments?per_page=100",
			body:   `[{"body":"## Codex Workpad\n\n### Human Action Needed\n- Need product approval based on #123 before continuing.\n\n### Validation\n- Pending."}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Blocked"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}
	if len(got[0].BlockedBy) != 0 {
		t.Fatalf("BlockedBy = %#v, want no dependency refs from Human Action Needed prose", got[0].BlockedBy)
	}
}

func TestConnectorFetchIssuesByStatesAttachesBlockedPullRequest(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_396","content":{"__typename":"Issue","id":"I_396","number":396,"title":"Blocked PR maintenance","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/396","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"bug"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":426,"url":"https://github.com/digitaldrywood/detent/pull/426","state":"OPEN","repository":{"nameWithOwner":"digitaldrywood/detent"}}]}},"statusValue":{"name":"Blocked"},"priorityValue":null}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/396/comments?per_page=100",
			body:   `[{"body":"## Codex Workpad\n\n### Human Action Needed\n- PR #426 latest head has no check-runs and conflicts with main."}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/426",
			body:   `{"number":426,"html_url":"https://github.com/digitaldrywood/detent/pull/426","state":"open","mergeable_state":"dirty","head":{"ref":"detent/digitaldrywood_detent_396","sha":"head-current"}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/head-current/check-runs?per_page=100",
			body:   `{"check_runs":[]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/head-current/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls/426/reviews?per_page=100",
			body:   `[]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Blocked"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}
	pr := got[0].PullRequest
	if pr == nil {
		t.Fatal("PullRequest = nil, want linked blocked PR")
	}
	if pr.Number != 426 || pr.State != "OPEN" || pr.HeadSHA != "head-current" || pr.MergeableState != "dirty" || pr.CIStatus != "" || pr.CheckRunCount != 0 {
		t.Fatalf("PullRequest = %#v, want dirty PR with no current-head checks", pr)
	}
}

func TestConnectorFetchIssueStatesByIDsUsesProjectStatusAndRequestOrder(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw1","number":1,"repository":{"nameWithOwner":"example/repo"}},{"__typename":"Issue","id":"I_kw2","number":2,"repository":{"nameWithOwner":"example/repo"}}]}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/issues/2",
			body:   `{"node_id":"I_kw2","number":2,"title":"Second","body":"","state":"open","html_url":"https://github.com/example/repo/issues/2","assignees":[],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_2","project":{"id":"PVT_1"},"statusValue":{"name":"Reviewing"},"priorityValue":{"name":"No priority"},"fieldValues":{"nodes":[]}}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/issues/1",
			body:   `{"node_id":"I_kw1","number":1,"title":"First","body":"","state":"open","html_url":"https://github.com/example/repo/issues/1","assignees":[],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Ready"},"priorityValue":{"name":"P1"},"fieldValues":{"nodes":[]}}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap: map[string]string{
			"Todo":         "Ready",
			"Human Review": "Reviewing",
		},
		PriorityMap: map[string]*int{"P1": new(2), "No priority": nil},
	})

	got, err := c.FetchIssueStatesByIDs(context.Background(), []string{"I_kw2", "I_kw1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if ids := githubIssueIDs(got); !reflect.DeepEqual(ids, []string{"I_kw2", "I_kw1"}) {
		t.Fatalf("FetchIssueStatesByIDs() ids = %#v, want [I_kw2 I_kw1]", ids)
	}
	if got[0].State != "Human Review" {
		t.Fatalf("first State = %q, want Human Review", got[0].State)
	}
	if got[1].Priority == nil || *got[1].Priority != 2 {
		t.Fatalf("second Priority = %v, want 2", got[1].Priority)
	}
}

func TestConnectorFetchIssueStatesByIDsCapturesIssueMetadata(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw1","number":1,"repository":{"nameWithOwner":"example/repo"}}]}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/issues/1",
			body:   `{"node_id":"I_kw1","number":1,"title":"First","body":"","state":"closed","state_reason":"not_planned","html_url":"https://github.com/example/repo/issues/1","user":{"login":"author-1"},"assignees":[{"node_id":"U_1","login":"worker-1"},{"node_id":"U_2","login":"worker-2"}],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Ready"},"priorityValue":{"name":"P1"},"fieldValues":{"nodes":[{"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"Ready","field":{"name":"Status"}},{"__typename":"ProjectV2ItemFieldTextValue","text":"team-a","field":{"name":"Owner"}},{"__typename":"ProjectV2ItemFieldNumberValue","number":3,"field":{"name":"Weight"}}]}}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap:    map[string]string{"Todo": "Ready"},
	})

	got, err := c.FetchIssueStatesByIDs(context.Background(), []string{"I_kw1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueStatesByIDs() len = %d, want 1", len(got))
	}

	issue := got[0]
	if issue.AuthorID != "author-1" {
		t.Fatalf("AuthorID = %q, want author-1", issue.AuthorID)
	}
	if !issue.Closed || issue.ClosedReason != "not_planned" {
		t.Fatalf("closed metadata = (%v, %q), want closed not_planned", issue.Closed, issue.ClosedReason)
	}
	if !reflect.DeepEqual(issue.Assignees, []string{"worker-1", "worker-2"}) {
		t.Fatalf("Assignees = %#v, want worker-1 and worker-2", issue.Assignees)
	}
	wantFields := map[string]string{"Owner": "team-a", "Status": "Ready", "Weight": "3"}
	if !reflect.DeepEqual(issue.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", issue.Fields, wantFields)
	}
	if issue.AssigneeID != "worker-1" {
		t.Fatalf("AssigneeID = %q, want worker-1", issue.AssigneeID)
	}
}

func TestConnectorFetchIssueStatesByIDsPaginatesProjectItems(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw1","number":1,"repository":{"nameWithOwner":"example/repo"}}]}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/example/repo/issues/1",
			body:   `{"node_id":"I_kw1","number":1,"title":"Later project","body":"","state":"open","html_url":"https://github.com/example/repo/issues/1","assignees":[],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"},"nodes":[{"id":"PVTI_other","project":{"id":"PVT_other"},"statusValue":{"name":"Open"},"priorityValue":{"name":"P1"},"fieldValues":{"nodes":[]}}]}}}}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Reviewing"},"priorityValue":{"name":"P2"}}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap: map[string]string{
			"Human Review": "Reviewing",
		},
		PriorityMap: map[string]*int{"P2": new(3)},
	})

	got, err := c.FetchIssueStatesByIDs(context.Background(), []string{"I_kw1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueStatesByIDs() len = %d, want 1", len(got))
	}
	if got[0].State != "Human Review" {
		t.Fatalf("State = %q, want Human Review", got[0].State)
	}
	if got[0].Priority == nil || *got[0].Priority != 3 {
		t.Fatalf("Priority = %v, want 3", got[0].Priority)
	}

	requests := server.requests()
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want identity, REST issue, and 2 project item pages", len(requests))
	}
	variables := requests[3]["variables"].(map[string]any)
	if variables["after"] != "cursor-1" {
		t.Fatalf("after = %v, want cursor-1", variables["after"])
	}
	if variables["issueId"] != "I_kw1" {
		t.Fatalf("issueId = %v, want I_kw1", variables["issueId"])
	}
}

func TestConnectorFetchIssueStatesByIdentifiersResolvesDependencyReadinessSignals(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/251",
			body:   `{"node_id":"I_closed","number":251,"title":"Closed child","body":"","state":"closed","html_url":"https://github.com/digitaldrywood/detent/issues/251","assignees":[],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/252",
			body:   `{"node_id":"I_done","number":252,"title":"Done child","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/252","assignees":[],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_done","project":{"id":"PVT_1"},"statusValue":{"name":"Done"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/253",
			body:   `{"node_id":"I_merged_pr","number":253,"title":"Merged PR child","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/253","assignees":[],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_merged_pr","project":{"id":"PVT_1"},"statusValue":{"name":"In Progress"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[{"number":254,"html_url":"https://github.com/digitaldrywood/detent/pull/254","state":"closed","merged_at":"2026-06-12T16:00:00Z","head":{"ref":"detent/digitaldrywood_detent_253-autounblock","sha":"abc123"}}]`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueStatesByIdentifiers(context.Background(), []string{"digitaldrywood/detent#251", "digitaldrywood/detent#252", "digitaldrywood/detent#253"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers() error = %v", err)
	}
	if ids := githubIssueIDs(got); !reflect.DeepEqual(ids, []string{"I_closed", "I_done", "I_merged_pr"}) {
		t.Fatalf("FetchIssueStatesByIdentifiers() ids = %#v, want [I_closed I_done I_merged_pr]", ids)
	}
	if !got[0].Closed || got[0].State != "Done" {
		t.Fatalf("closed child = %#v, want Closed true and State Done", got[0])
	}
	if got[1].Closed || got[1].State != "Done" {
		t.Fatalf("project done child = %#v, want open issue with State Done", got[1])
	}
	if got[2].Closed || got[2].State != "In Progress" {
		t.Fatalf("merged PR child = %#v, want open issue still In Progress", got[2])
	}
	if got[2].PullRequest == nil || got[2].PullRequest.State != "MERGED" || got[2].PullRequest.Number != 254 {
		t.Fatalf("merged PR child PullRequest = %#v, want merged PR 254", got[2].PullRequest)
	}

	requests := server.requests()
	if len(requests) != 7 {
		t.Fatalf("request count = %d, want REST issue and project field reads for each identifier plus PR list", len(requests))
	}
	if requests[0]["method"] != http.MethodGet || requests[0]["path"] != "/repos/digitaldrywood/detent/issues/251" {
		t.Fatalf("first request = %#v, want REST issue lookup", requests[0])
	}
	if requests[6]["method"] != http.MethodGet || requests[6]["path"] != "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all" {
		t.Fatalf("PR request = %#v, want REST pull request list", requests[6])
	}
}

func TestConnectorFetchIssueChildrenPaginatesLinkedIssues(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"subIssues":{"pageInfo":{"hasNextPage":true,"endCursor":"sub-cursor-1"},"nodes":[{"id":"I_sub_1","number":251,"title":"Sub child","state":"CLOSED","url":"https://github.com/digitaldrywood/detent/issues/251","repository":{"nameWithOwner":"digitaldrywood/detent"},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[]}}]}}}}`,
		},
		{
			body: `{"data":{"node":{"subIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_sub_2","number":252,"title":"Sub child 2","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/252","repository":{"nameWithOwner":"digitaldrywood/detent"},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_sub_2","project":{"id":"PVT_1"},"statusValue":{"name":"Done"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}]}}}}`,
		},
		{
			body: `{"data":{"node":{"trackedIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_tracked","number":253,"title":"Tracked child","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/253","repository":{"nameWithOwner":"digitaldrywood/detent"},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_tracked","project":{"id":"PVT_1"},"statusValue":{"name":"In Progress"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueChildren(context.Background(), "I_epic")
	if err != nil {
		t.Fatalf("FetchIssueChildren() error = %v", err)
	}
	want := []connector.BlockedRef{
		{ID: "I_sub_1", Identifier: "digitaldrywood/detent#251", State: "Done"},
		{ID: "I_sub_2", Identifier: "digitaldrywood/detent#252", State: "Done"},
		{ID: "I_tracked", Identifier: "digitaldrywood/detent#253", State: "In Progress"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueChildren() = %#v, want %#v", got, want)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	firstVariables := requests[0]["variables"].(map[string]any)
	if firstVariables["linkedIssuesFirst"] != float64(linkedIssuePageSize) {
		t.Fatalf("linkedIssuesFirst = %v, want %d", firstVariables["linkedIssuesFirst"], linkedIssuePageSize)
	}
	if firstVariables["linkedProjectItemsFirst"] != float64(linkedIssueProjectItemsPageSize) {
		t.Fatalf("linkedProjectItemsFirst = %v, want %d", firstVariables["linkedProjectItemsFirst"], linkedIssueProjectItemsPageSize)
	}
	if firstVariables["linkedProjectItemFieldValuesFirst"] != float64(linkedIssueProjectItemFieldValuesPageSize) {
		t.Fatalf("linkedProjectItemFieldValuesFirst = %v, want %d", firstVariables["linkedProjectItemFieldValuesFirst"], linkedIssueProjectItemFieldValuesPageSize)
	}
	secondVariables := requests[1]["variables"].(map[string]any)
	if secondVariables["after"] != "sub-cursor-1" {
		t.Fatalf("second after = %v, want sub-cursor-1", secondVariables["after"])
	}
	if !strings.Contains(requests[2]["query"].(string), "trackedIssues") {
		t.Fatalf("third query = %q, want trackedIssues", requests[2]["query"])
	}
}

func TestConnectorFetchIssueParentsReturnsParentAndTrackedInIssues(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"parent":{"__typename":"Issue","id":"I_parent","number":258,"title":"Epic: Parent","body":"- [ ] #251","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/258","createdAt":null,"updatedAt":null,"author":{"login":"corylanou"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"epic"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]},"subIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_child","number":251,"title":"Child","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/251","repository":{"nameWithOwner":"digitaldrywood/detent"},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_child","project":{"id":"PVT_1"},"statusValue":{"name":"Done"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}]},"trackedIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_parent","project":{"id":"PVT_1"},"statusValue":{"name":"Todo","updatedAt":"2026-06-02T16:00:00Z"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}},"trackedInIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"__typename":"Issue","id":"I_tracked_parent","number":259,"title":"Epic: Tracked parent","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/259","createdAt":null,"updatedAt":null,"author":{"login":"corylanou"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"epic"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]},"subIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},"trackedIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_child","number":251,"title":"Child","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/251","repository":{"nameWithOwner":"digitaldrywood/detent"},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_child","project":{"id":"PVT_1"},"statusValue":{"name":"Done"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}]},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_tracked_parent","project":{"id":"PVT_1"},"statusValue":{"name":"In Progress","updatedAt":"2026-06-02T16:01:00Z"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}]}}}}`,
	}})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueParents(context.Background(), "I_child")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FetchIssueParents() len = %d, want 2", len(got))
	}
	if got[0].ID != "I_parent" || got[0].Identifier != "digitaldrywood/detent#258" || got[0].State != "Todo" {
		t.Fatalf("first parent = %#v", got[0])
	}
	if got[1].ID != "I_tracked_parent" || got[1].Identifier != "digitaldrywood/detent#259" || got[1].State != "In Progress" {
		t.Fatalf("second parent = %#v", got[1])
	}
	if got[0].ChildIssues[0] != (connector.BlockedRef{ID: "I_child", Identifier: "digitaldrywood/detent#251", State: "Done"}) {
		t.Fatalf("first parent child issues = %#v", got[0].ChildIssues)
	}
	if got[1].ChildIssues[0] != (connector.BlockedRef{ID: "I_child", Identifier: "digitaldrywood/detent#251", State: "Done"}) {
		t.Fatalf("second parent child issues = %#v", got[1].ChildIssues)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	variables := requests[0]["variables"].(map[string]any)
	if variables["issueId"] != "I_child" {
		t.Fatalf("issueId = %v, want I_child", variables["issueId"])
	}
	query := requests[0]["query"].(string)
	for _, want := range []string{"parent", "trackedInIssues", "subIssues(first: $linkedIssuesFirst)", "trackedIssues(first: $linkedIssuesFirst)"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
	if variables["linkedIssuesFirst"] != float64(linkedIssuePageSize) {
		t.Fatalf("linkedIssuesFirst = %v, want %d", variables["linkedIssuesFirst"], linkedIssuePageSize)
	}
}

func TestConnectorFetchIssueParentsReturnsBodyReferencedEpic(t *testing.T) {
	t.Parallel()

	body := "- [ ] #251"
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"id":"I_child","number":251,"repository":{"nameWithOwner":"digitaldrywood/detent"},"parent":null,"trackedInIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
		},
		{
			method: http.MethodGet,
			body:   `{"items":[{"number":258}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/258",
			body:   `{"node_id":"I_epic","number":258,"title":"Epic: Parent","body":"` + body + `","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/258","assignees":[],"labels":[{"name":"epic"}]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_parent","project":{"id":"PVT_1"},"statusValue":{"name":"Todo"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueParents(context.Background(), "I_child")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueParents() len = %d, want 1", len(got))
	}
	if got[0].ID != "I_epic" || got[0].Identifier != "digitaldrywood/detent#258" || got[0].State != "Todo" {
		t.Fatalf("body parent = %#v", got[0])
	}
	if got[0].Description != body {
		t.Fatalf("body parent description = %q, want %q", got[0].Description, body)
	}

	requests := server.requests()
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want parent lookup, search, REST issue, project item", len(requests))
	}
	if requests[1]["method"] != http.MethodGet || !strings.HasPrefix(requests[1]["path"].(string), "/search/issues?") {
		t.Fatalf("search request = %#v, want REST issue search", requests[1])
	}
	if !strings.Contains(requests[1]["path"].(string), "251") {
		t.Fatalf("search path = %q, want child issue number", requests[1]["path"])
	}
}

func TestConnectorFetchIssueParentsReturnsCrossRepoBodyReferencedEpic(t *testing.T) {
	t.Parallel()

	body := "Depends on: digitaldrywood/agent-runtime#251"
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"id":"I_child","number":251,"repository":{"nameWithOwner":"digitaldrywood/agent-runtime"},"parent":null,"trackedInIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
		},
		{
			method: http.MethodGet,
			body:   `{"total_count":1,"items":[{"number":258,"html_url":"https://github.com/digitaldrywood/detent/issues/258"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/258",
			body:   `{"node_id":"I_epic","number":258,"title":"Epic: Parent","body":"` + body + `","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/258","assignees":[],"labels":[{"name":"epic"}]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_parent","project":{"id":"PVT_1"},"statusValue":{"name":"Todo"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueParents(context.Background(), "I_child")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueParents() len = %d, want 1", len(got))
	}
	if got[0].ID != "I_epic" || got[0].Identifier != "digitaldrywood/detent#258" || got[0].Description != body {
		t.Fatalf("cross-repo body parent = %#v", got[0])
	}

	requests := server.requests()
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want parent lookup, search, REST issue, project item", len(requests))
	}
	searchPath := requests[1]["path"].(string)
	if !strings.Contains(searchPath, "user%3Adigitaldrywood") || strings.Contains(searchPath, "repo%3A") {
		t.Fatalf("search path = %q, want owner-scoped search", searchPath)
	}
	if requests[2]["path"] != "/repos/digitaldrywood/detent/issues/258" {
		t.Fatalf("REST issue path = %#v, want cross-repo epic issue", requests[2])
	}
}

func TestConnectorFetchIssueParentsPaginatesBodyReferencedEpicSearch(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"id":"I_child","number":251,"repository":{"nameWithOwner":"digitaldrywood/detent"},"parent":null,"trackedInIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
		},
		{
			method: http.MethodGet,
			body:   `{"total_count":101,"items":[{"number":251}]}`,
		},
		{
			method: http.MethodGet,
			body:   `{"total_count":101,"items":[{"number":258}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/258",
			body:   `{"node_id":"I_epic","number":258,"title":"Epic: Parent","body":"Depends on: #251","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/258","assignees":[],"labels":[]}`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_parent","project":{"id":"PVT_1"},"statusValue":{"name":"Todo"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}}}`,
		},
	})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueParents(context.Background(), "I_child")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "I_epic" {
		t.Fatalf("FetchIssueParents() = %#v, want body referenced epic", got)
	}

	requests := server.requests()
	if len(requests) != 5 {
		t.Fatalf("request count = %d, want parent lookup, 2 search pages, REST issue, project item", len(requests))
	}
	firstSearch := requests[1]["path"].(string)
	secondSearch := requests[2]["path"].(string)
	if !strings.Contains(firstSearch, "page=1") || !strings.Contains(secondSearch, "page=2") {
		t.Fatalf("search paths = %q, %q; want page 1 then page 2", firstSearch, secondSearch)
	}
}

func TestConnectorFetchIssueParentsLeavesPagedLinkedChildStateUnresolved(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"parent":{"__typename":"Issue","id":"I_parent","number":258,"title":"Epic: Parent","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/258","repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]},"subIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_child","number":251,"title":"Child","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/251","repository":{"nameWithOwner":"digitaldrywood/detent"},"projectItems":{"pageInfo":{"hasNextPage":true,"endCursor":"project-cursor-1"},"nodes":[{"id":"PVTI_other","project":{"id":"PVT_other"},"statusValue":{"name":"Todo"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}}]},"trackedIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_parent","project":{"id":"PVT_1"},"statusValue":{"name":"Todo"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}},"trackedInIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
	}})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueParents(context.Background(), "I_child")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueParents() len = %d, want 1", len(got))
	}
	want := connector.BlockedRef{ID: "I_child", Identifier: "digitaldrywood/detent#251"}
	if got[0].ChildIssues[0] != want {
		t.Fatalf("parent child issue = %#v, want %#v", got[0].ChildIssues[0], want)
	}
}

func TestConnectorFetchIssueParentsSkipsParentsOutsideProject(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"parent":{"__typename":"Issue","id":"I_outside_parent","number":260,"title":"Outside epic","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/260","repository":{"nameWithOwner":"digitaldrywood/detent"},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_other","project":{"id":"PVT_other"},"statusValue":{"name":"Todo"},"priorityValue":null,"fieldValues":{"nodes":[]}}]}},"trackedInIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
	}})

	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.FetchIssueParents(context.Background(), "I_child")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("FetchIssueParents() = %#v, want no out-of-project parents", got)
	}
}

func TestConnectorCreateCommentCallsAddComment(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodPost,
		path:   "/repos/example/repo/issues/1/comments",
		body:   `{"node_id":"IC_kw1"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})
	c.projectCache.SetIssueRef("I_kw1", issueRef{Owner: "example", Name: "repo", Number: 1})

	if err := c.CreateComment(context.Background(), "I_kw1", "hello"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if requests[0]["method"] != http.MethodPost || requests[0]["path"] != "/repos/example/repo/issues/1/comments" {
		t.Fatalf("comment request = %#v, want REST issue comment", requests[0])
	}
	body := requests[0]["body"].(map[string]any)
	if body["body"] != "hello" {
		t.Fatalf("body = %v, want hello", body["body"])
	}
}

func TestConnectorCreatePullRequestCommentUsesIssueCommentsEndpoint(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodPost,
		path:   "/repos/example/repo/issues/42/comments",
		body:   `{"node_id":"IC_pr"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})

	if err := c.CreatePullRequestComment(context.Background(), "example/repo", 42, "ship it"); err != nil {
		t.Fatalf("CreatePullRequestComment() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	body := requests[0]["body"].(map[string]any)
	if body["body"] != "ship it" {
		t.Fatalf("body = %v, want ship it", body["body"])
	}
}

func TestConnectorSetIssueFieldUsesIssueFieldEndpoint(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw28","number":28,"repository":{"nameWithOwner":"digitaldrywood/detent"}}]}}`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/issues/28/issue-field-values",
			body:   `[{"issue_field_id":123,"node_id":"IFSS_status","data_type":"single_select","value":1,"single_select_option":{"id":1,"name":"In Progress","color":"blue"}}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})

	if err := c.SetIssueField(context.Background(), "I_kw28", 123, "In Progress"); err != nil {
		t.Fatalf("SetIssueField() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[1]["method"] != http.MethodPost || requests[1]["path"] != "/repos/digitaldrywood/detent/issues/28/issue-field-values" {
		t.Fatalf("issue field request = %#v, want REST issue field update", requests[1])
	}
	body := requests[1]["body"].(map[string]any)
	values := body["issue_field_values"].([]any)
	if len(values) != 1 {
		t.Fatalf("issue_field_values len = %d, want 1", len(values))
	}
	value := values[0].(map[string]any)
	if value["field_id"] != float64(123) || value["value"] != "In Progress" {
		t.Fatalf("issue field value = %#v, want field_id 123 value In Progress", value)
	}
}

func TestConnectorCloseIssueCallsCloseIssue(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodPatch,
		path:   "/repos/example/repo/issues/1",
		body:   `{"node_id":"I_kw1","state":"closed"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})
	c.projectCache.SetIssueRef("I_kw1", issueRef{Owner: "example", Name: "repo", Number: 1})

	if err := c.CloseIssue(context.Background(), " I_kw1 "); err != nil {
		t.Fatalf("CloseIssue() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if requests[0]["method"] != http.MethodPatch || requests[0]["path"] != "/repos/example/repo/issues/1" {
		t.Fatalf("close request = %#v, want REST issue patch", requests[0])
	}
	body := requests[0]["body"].(map[string]any)
	if body["state"] != "closed" || body["state_reason"] != "completed" {
		t.Fatalf("close body = %#v, want closed/completed", body)
	}
}

func TestConnectorUpdateIssueStateWritesStatusOptionID(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"}}]}}}}`},
		{body: `{"data":{"node":{"field":{"id":"PVTSSF_status","options":[{"id":"OPT_ready","name":"Ready"},{"id":"OPT_todo","name":"Todo"}]}}}}`},
		{body: `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_1"}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap:    map[string]string{"Todo": "Ready"},
	})

	if err := c.UpdateIssueState(context.Background(), "I_kw1", "Todo"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	updateQuery := requests[2]["query"].(string)
	if !strings.Contains(updateQuery, "updateProjectV2ItemFieldValue") {
		t.Fatalf("query = %q, want updateProjectV2ItemFieldValue", updateQuery)
	}
	if strings.Contains(updateQuery, "rateLimit") {
		t.Fatalf("query = %q, want no rateLimit on mutation root", updateQuery)
	}
	variables := requests[2]["variables"].(map[string]any)
	want := map[string]any{
		"projectId": "PVT_1",
		"itemId":    "PVTI_1",
		"fieldId":   "PVTSSF_status",
		"optionId":  "OPT_ready",
	}
	for key, value := range want {
		if variables[key] != value {
			t.Fatalf("%s = %v, want %v", key, variables[key], value)
		}
	}
	if variables["optionId"] == "Ready" {
		t.Fatal("optionId used the option name, want option id")
	}
}

func TestConnectorRemoveIssueFromProjectDeletesProjectV2Item(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Todo"}}]}}}}`},
		{body: `{"data":{"deleteProjectV2Item":{"deletedItemId":"PVTI_1"}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
	})

	if err := c.RemoveIssueFromProject(context.Background(), "I_kw1"); err != nil {
		t.Fatalf("RemoveIssueFromProject() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want project item lookup and delete", len(requests))
	}
	deleteQuery := requests[1]["query"].(string)
	if !strings.Contains(deleteQuery, "deleteProjectV2Item") {
		t.Fatalf("query = %q, want deleteProjectV2Item", deleteQuery)
	}
	if strings.Contains(deleteQuery, "rateLimit") {
		t.Fatalf("query = %q, want no rateLimit on mutation root", deleteQuery)
	}
	variables := requests[1]["variables"].(map[string]any)
	want := map[string]any{
		"projectId": "PVT_1",
		"itemId":    "PVTI_1",
	}
	for key, value := range want {
		if variables[key] != value {
			t.Fatalf("%s = %v, want %v", key, variables[key], value)
		}
	}
}

func TestConnectorVerifyStatusOptionsChecksMappedStatusOptions(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"node":{"field":{"id":"PVTSSF_status","options":[{"id":"OPT_review","name":"Reviewing"}]}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap:    map[string]string{"Human Review": "Reviewing"},
	})

	err := c.VerifyStatusOptions(context.Background(), []string{"Human Review", "Merging"})
	if err == nil {
		t.Fatal("VerifyStatusOptions() error = nil, want ErrStatusOptionNotFound")
		return
	}
	if !errors.Is(err, ErrStatusOptionNotFound) {
		t.Fatalf("VerifyStatusOptions() error = %v, want ErrStatusOptionNotFound", err)
	}
	if !strings.Contains(err.Error(), "Merging") {
		t.Fatalf("VerifyStatusOptions() error = %q, want missing Merging detail", err.Error())
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if strings.Contains(requests[0]["query"].(string), "updateProjectV2ItemFieldValue") {
		t.Fatalf("VerifyStatusOptions issued mutation: %q", requests[0]["query"])
	}
}

func TestConnectorUpdateIssueStateSkipsTerminalToActiveTransition(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Done"}}]}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:    "PVT_1",
		TerminalStates: []string{"Done", "Cancelled"},
	})

	if err := c.UpdateIssueState(context.Background(), "I_kw1", "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if strings.Contains(requests[0]["query"].(string), "updateProjectV2ItemFieldValue") {
		t.Fatalf("terminal guard issued update mutation: %q", requests[0]["query"])
	}
}

func TestConnectorSetAssigneeAddsUserByLogin(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: "/repos/example/repo/issues/1", body: `{"node_id":"I_kw1","number":1,"title":"Issue","state":"open","html_url":"https://github.com/example/repo/issues/1","assignees":[],"labels":[]}`},
		{method: http.MethodPost, path: "/repos/example/repo/issues/1/assignees", body: `{"node_id":"I_kw1"}`},
	})
	c := newGitHubTestConnector(t, server, Config{})
	c.projectCache.SetIssueRef("I_kw1", issueRef{Owner: "example", Name: "repo", Number: 1})

	if err := c.SetAssignee(context.Background(), " I_kw1 ", " worker-1 "); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	assignBody := requests[1]["body"].(map[string]any)
	assignees, ok := assignBody["assignees"].([]any)
	if !ok || len(assignees) != 1 || assignees[0] != "worker-1" {
		t.Fatalf("assignees = %#v, want [worker-1]", assignBody["assignees"])
	}
}

func TestConnectorSetAssigneeReplacesExistingAssignees(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: "/repos/example/repo/issues/1", body: `{"node_id":"I_kw1","number":1,"title":"Issue","state":"open","html_url":"https://github.com/example/repo/issues/1","assignees":[{"node_id":"U_old","login":"old-owner"},{"node_id":"U_worker","login":"worker-1"}],"labels":[]}`},
		{method: http.MethodDelete, path: "/repos/example/repo/issues/1/assignees", body: `{"node_id":"I_kw1"}`},
	})
	c := newGitHubTestConnector(t, server, Config{})
	c.projectCache.SetIssueRef("I_kw1", issueRef{Owner: "example", Name: "repo", Number: 1})

	if err := c.SetAssignee(context.Background(), "I_kw1", "worker-1"); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	removeBody := requests[1]["body"].(map[string]any)
	assignees, ok := removeBody["assignees"].([]any)
	if !ok || len(assignees) != 1 || assignees[0] != "old-owner" {
		t.Fatalf("removed assignees = %#v, want [old-owner]", removeBody["assignees"])
	}
}

func TestConnectorSetAssigneeAddsReplacementBeforeRemovingExistingAssignees(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: "/repos/example/repo/issues/1", body: `{"node_id":"I_kw1","number":1,"title":"Issue","state":"open","html_url":"https://github.com/example/repo/issues/1","assignees":[{"node_id":"U_old","login":"old-owner"}],"labels":[]}`},
		{method: http.MethodPost, path: "/repos/example/repo/issues/1/assignees", body: `{"node_id":"I_kw1"}`},
		{method: http.MethodDelete, path: "/repos/example/repo/issues/1/assignees", body: `{"node_id":"I_kw1"}`},
	})
	c := newGitHubTestConnector(t, server, Config{})
	c.projectCache.SetIssueRef("I_kw1", issueRef{Owner: "example", Name: "repo", Number: 1})

	if err := c.SetAssignee(context.Background(), "I_kw1", "worker-1"); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	if requests[1]["method"] != http.MethodPost {
		t.Fatalf("second request = %#v, want assignee add", requests[1])
	}
	if requests[2]["method"] != http.MethodDelete {
		t.Fatalf("third request = %#v, want assignee remove", requests[2])
	}
}

func TestConnectorSetAssigneeDoesNotRemoveExistingAssigneesWhenAddFails(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{method: http.MethodGet, path: "/repos/example/repo/issues/1", body: `{"node_id":"I_kw1","number":1,"title":"Issue","state":"open","html_url":"https://github.com/example/repo/issues/1","assignees":[{"node_id":"U_old","login":"old-owner"}],"labels":[]}`},
		{method: http.MethodPost, path: "/repos/example/repo/issues/1/assignees", status: http.StatusUnprocessableEntity, body: `{"message":"not assignable"}`},
	})
	c := newGitHubTestConnector(t, server, Config{})
	c.projectCache.SetIssueRef("I_kw1", issueRef{Owner: "example", Name: "repo", Number: 1})

	if err := c.SetAssignee(context.Background(), "I_kw1", "worker-1"); err == nil {
		t.Fatal("SetAssignee() error = nil, want error")
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[1]["method"] != http.MethodPost {
		t.Fatalf("second request = %#v, want assignee add", requests[1])
	}
}

func TestConnectorSetFieldProvisionsOwnerOptionAndWritesProjectValue(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"}}]}}}}`},
		{body: `{"data":{"node":{"__typename":"ProjectV2","field":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_owner","options":[{"id":"OPT_other","name":"worker-0","color":"BLUE","description":"Existing owner."}]}}}}`},
		{body: `{"data":{"node":{"__typename":"ProjectV2","field":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_owner","options":[{"id":"OPT_other","name":"worker-0","color":"BLUE","description":"Existing owner."},{"id":"OPT_concurrent","name":"worker-2","color":"BLUE","description":"Concurrent owner."}]}}}}`},
		{body: `{"data":{"updateProjectV2Field":{"projectV2Field":{"options":[{"id":"OPT_other","name":"worker-0","color":"BLUE","description":"Existing owner."},{"id":"OPT_concurrent","name":"worker-2","color":"BLUE","description":"Concurrent owner."},{"id":"OPT_worker","name":"worker-1","color":"BLUE","description":"Detent ownership identity."}]}}}}`},
		{body: `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_1"}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	if err := c.SetField(context.Background(), "I_kw1", " Owner ", " worker-1 "); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 5 {
		t.Fatalf("request count = %d, want 5", len(requests))
	}
	fieldVariables := requests[1]["variables"].(map[string]any)
	if fieldVariables["fieldName"] != "Owner" {
		t.Fatalf("fieldName = %v, want Owner", fieldVariables["fieldName"])
	}
	refetchVariables := requests[2]["variables"].(map[string]any)
	if refetchVariables["fieldName"] != "Owner" {
		t.Fatalf("refetch fieldName = %v, want Owner", refetchVariables["fieldName"])
	}
	input := graphQLInput(t, requests[3])
	if input["fieldId"] != "PVTSSF_owner" {
		t.Fatalf("fieldId = %v, want PVTSSF_owner", input["fieldId"])
	}
	options := graphQLOptions(t, input)
	if got := optionNames(options); !reflect.DeepEqual(got, []string{"worker-0", "worker-2", "worker-1"}) {
		t.Fatalf("option names = %#v, want worker-0, worker-2, worker-1", got)
	}
	updateVariables := requests[4]["variables"].(map[string]any)
	want := map[string]any{
		"projectId": "PVT_1",
		"itemId":    "PVTI_1",
		"fieldId":   "PVTSSF_owner",
		"optionId":  "OPT_worker",
	}
	for key, value := range want {
		if updateVariables[key] != value {
			t.Fatalf("%s = %v, want %v", key, updateVariables[key], value)
		}
	}
	if !strings.Contains(requests[4]["query"].(string), "updateProjectV2ItemFieldValue") {
		t.Fatalf("update query = %q, want updateProjectV2ItemFieldValue", requests[4]["query"])
	}
}

func TestConnectorSetFieldWritesTextProjectValue(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"}}]}}}}`},
		{body: `{"data":{"node":{"__typename":"ProjectV2","field":{"__typename":"ProjectV2Field","id":"PVTF_lease","dataType":"TEXT"}}}}`},
		{body: `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_1"}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	if err := c.SetField(context.Background(), "I_kw1", "Detent Lease", "2026-06-02T15:00:00Z"); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	fieldVariables := requests[1]["variables"].(map[string]any)
	if fieldVariables["fieldName"] != "Detent Lease" {
		t.Fatalf("fieldName = %v, want Detent Lease", fieldVariables["fieldName"])
	}
	updateVariables := requests[2]["variables"].(map[string]any)
	want := map[string]any{
		"projectId": "PVT_1",
		"itemId":    "PVTI_1",
		"fieldId":   "PVTF_lease",
		"text":      "2026-06-02T15:00:00Z",
	}
	for key, value := range want {
		if updateVariables[key] != value {
			t.Fatalf("%s = %v, want %v", key, updateVariables[key], value)
		}
	}
	if !strings.Contains(requests[2]["query"].(string), "text") {
		t.Fatalf("update query = %q, want text field mutation", requests[2]["query"])
	}
}

type graphqlTestServer struct {
	*httptest.Server
	t         *testing.T
	mu        sync.Mutex
	responses []graphqlTestResponse
	seen      []map[string]any
}

type graphqlTestResponse struct {
	status  int
	method  string
	path    string
	headers map[string]string
	body    string
	release <-chan struct{}
}

func newGraphQLTestServer(t *testing.T, responses []graphqlTestResponse) *graphqlTestServer {
	t.Helper()

	server := &graphqlTestServer{t: t, responses: responses}
	server.Server = httptest.NewServer(http.HandlerFunc(server.serveHTTP))
	t.Cleanup(server.Close)
	return server
}

func (s *graphqlTestServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"method": r.Method,
		"path":   r.URL.RequestURI(),
	}
	if r.Method == http.MethodPost && r.URL.Path == "/" {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			s.t.Fatalf("Decode() error = %v", err)
		}
		payload["method"] = r.Method
		payload["path"] = r.URL.RequestURI()
	} else {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Fatalf("ReadAll() error = %v", err)
		}
		if len(raw) > 0 {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				s.t.Fatalf("Unmarshal() error = %v", err)
			}
			payload["body"] = body
		}
	}

	s.mu.Lock()
	s.seen = append(s.seen, payload)
	if len(s.responses) == 0 {
		s.mu.Unlock()
		s.t.Fatalf("unexpected GraphQL request: %v", payload)
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	s.mu.Unlock()
	if response.method != "" && response.method != r.Method {
		s.t.Fatalf("method = %s, want %s", r.Method, response.method)
	}
	if response.path != "" && response.path != r.URL.RequestURI() {
		s.t.Fatalf("path = %s, want %s", r.URL.RequestURI(), response.path)
	}

	if response.release != nil {
		<-response.release
	}

	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	for key, value := range response.headers {
		w.Header().Set(key, value)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(response.body))
}

func (s *graphqlTestServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]map[string]any, len(s.seen))
	copy(out, s.seen)
	return out
}

func waitForGraphQLRequests(t *testing.T, server *graphqlTestServer, want int) []map[string]any {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		requests := server.requests()
		if len(requests) >= want {
			return requests
		}
		time.Sleep(10 * time.Millisecond)
	}
	requests := server.requests()
	t.Fatalf("request count = %d, want at least %d", len(requests), want)
	return nil
}

func newGitHubTestConnector(t *testing.T, server *graphqlTestServer, cfg Config) *Connector {
	t.Helper()

	cfg.Endpoint = server.URL
	cfg.APIKey = "token"
	cfg.HTTPClient = server.Client()
	cfg.GHToken = func(context.Context, string) (string, error) {
		t.Fatal("gh token fallback should not run")
		return "", nil
	}
	c, err := NewConnector(cfg)
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	return c
}

func githubIssueIDs(issues []connector.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}
