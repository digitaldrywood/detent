package github

import (
	"context"
	"encoding/json"
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
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw1","number":26,"title":"GitHub adapter","body":"Depends on: #24 digitaldrywood/detent#25\n<!-- model: gpt-5-codex-high -->","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/26","createdAt":"2026-05-31T01:02:03Z","updatedAt":"2026-05-31T02:03:04Z","author":{"login":"corylanou"},"assignees":{"nodes":[{"login":"worker-1"},{"login":"worker-2"}]},"labels":{"nodes":[{"name":"Enhancement"},{"name":"stage:S4"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[{"number":42,"url":"https://github.com/digitaldrywood/detent/pull/42"}]}},"statusValue":{"name":"Ready"},"priorityValue":{"name":"P0"},"fieldValues":{"nodes":[{"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"Ready","field":{"name":"Status"}},{"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"P0","field":{"name":"Priority"}},{"__typename":"ProjectV2ItemFieldTextValue","text":"release-ready","field":{"name":"Track"}},{"__typename":"ProjectV2ItemFieldNumberValue","number":7,"field":{"name":"Sort"}}]}},{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_kw2","number":27,"title":"Backlog item","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/27","createdAt":"2026-05-31T03:02:03Z","updatedAt":"2026-05-31T04:03:04Z","author":{"login":"octocat"},"assignees":{"nodes":[{"login":"worker-1"}]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":{"name":"Backlog"},"priorityValue":{"name":"No priority"},"fieldValues":{"nodes":[]}}]}}}}`,
	}, {
		body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
	}})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
		StateMap:     map[string]string{"Todo": "Ready"},
		PriorityMap:  map[string]*int{"P0": intPtr(1), "No priority": nil},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}

	createdAt := time.Date(2026, 5, 31, 1, 2, 3, 0, time.UTC)
	updatedAt := time.Date(2026, 5, 31, 2, 3, 4, 0, time.UTC)
	priority := 1
	prNumber := 42
	want := connector.Issue{
		ID:               "I_kw1",
		Identifier:       "digitaldrywood/detent#26",
		Title:            "GitHub adapter",
		Description:      "Depends on: #24 digitaldrywood/detent#25\n<!-- model: gpt-5-codex-high -->",
		Priority:         &priority,
		State:            "Todo",
		URL:              "https://github.com/digitaldrywood/detent/issues/26",
		PRNumber:         &prNumber,
		AuthorID:         "corylanou",
		AssigneeID:       "worker-1",
		Assignees:        []string{"worker-1", "worker-2"},
		BlockedBy:        []connector.BlockedRef{{Identifier: "digitaldrywood/detent#24"}, {Identifier: "digitaldrywood/detent#25"}},
		Labels:           []string{"enhancement", "stage:s4"},
		Fields:           map[string]string{"Priority": "P0", "Sort": "7", "Status": "Ready", "Track": "release-ready"},
		AssignedToWorker: true,
		CreatedAt:        &createdAt,
		UpdatedAt:        &updatedAt,
		ModelOverride:    "gpt-5-codex-high",
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
	prVariables := requests[1]["variables"].(map[string]any)
	if prVariables["owner"] != "digitaldrywood" || prVariables["name"] != "detent" {
		t.Fatalf("pull request repo variables = %#v, want digitaldrywood/detent", prVariables)
	}
	query := requests[0]["query"].(string)
	for _, want := range []string{"author { login }", "assignees(first: 100)", "fieldValues(first: 100)"} {
		if !strings.Contains(query, want) {
			t.Fatalf("project query missing %q:\n%s", want, query)
		}
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

func TestConnectorFetchCandidateIssuesResolvesBlockedByProjectState(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_candidate","number":26,"title":"Candidate","body":"Depends on: DigitalDryWood/Detent#24 digitaldrywood/detent#25 digitaldrywood/detent#999","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/26","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Ready"},"priorityValue":null},{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_done","number":24,"title":"Done blocker","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/24","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Done"},"priorityValue":null},{"id":"PVTI_3","content":{"__typename":"Issue","id":"I_progress","number":25,"title":"Active blocker","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/25","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Working"},"priorityValue":null}]}}}}`,
	}, {
		body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
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

	want := []connector.BlockedRef{
		{ID: "I_done", Identifier: "digitaldrywood/detent#24", State: "Done"},
		{ID: "I_progress", Identifier: "digitaldrywood/detent#25", State: "In Progress"},
		{Identifier: "digitaldrywood/detent#999"},
	}
	if !reflect.DeepEqual(got[0].BlockedBy, want) {
		t.Fatalf("BlockedBy = %#v, want %#v", got[0].BlockedBy, want)
	}
}

func TestConnectorFetchCandidateIssuesAttachesPullRequestByBranchPrefix(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"First issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null},{"id":"PVTI_18","content":{"__typename":"Issue","id":"I_18","number":18,"title":"Prefix neighbor","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/18","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"number":187,"url":"https://github.com/digitaldrywood/detent/pull/187","state":"OPEN","headRefName":"detent/digitaldrywood_detent_182_followup","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]},"latestReviews":{"nodes":[{"body":"[P2] The fallback path needs cleanup.","state":"COMMENTED","author":{"login":"codex"}}]}},{"number":188,"url":"https://github.com/digitaldrywood/detent/pull/188","state":"MERGED","headRefName":"detent/digitaldrywood_detent_181"}]}}}}`,
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
	if pr.Number != 187 || pr.State != "OPEN" || pr.BranchName != "detent/digitaldrywood_detent_182_followup" || pr.CIStatus != "pass" || pr.CodexReviewState != "P2" {
		t.Fatalf("I_182 PullRequest = %#v, want PR 187 open followup", pr)
	}
	if byID["I_18"].PullRequest != nil {
		t.Fatalf("I_18 PullRequest = %#v, want nil", byID["I_18"].PullRequest)
	}
}

func TestConnectorFetchIssuesByStatesAttachesPipelinePullRequest(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"Review issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":{"name":"Reviewing"},"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"number":190,"url":"https://github.com/digitaldrywood/detent/pull/190","state":"OPEN","headRefName":"detent/digitaldrywood_detent_182","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE"}}}]},"latestReviews":{"nodes":[{"body":"[P1] Unsafe migration.","state":"COMMENTED","author":{"login":"codex"}}]}}]}}}}`,
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
}

func TestConnectorFetchCandidateIssuesLimitsPullRequestPagination(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_1","number":1,"title":"Candidate","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/1","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"},"nodes":[]}}}}`,
		},
		{
			body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-2"},"nodes":[]}}}}`,
		},
		{
			body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-3"},"nodes":[]}}}}`,
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
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want project query plus 3 pull request pages", len(requests))
	}
	variables := requests[3]["variables"].(map[string]any)
	if variables["after"] != "cursor-2" {
		t.Fatalf("third pull request page after = %v, want cursor-2", variables["after"])
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
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw1","number":1,"title":"Done issue","body":"","state":"OPEN","url":"https://github.com/example/repo/issues/1","createdAt":"2026-05-31T01:02:03Z","updatedAt":"2026-05-31T02:03:04Z","assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"example/repo"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":{"name":"Done","updatedAt":"2026-06-01T12:30:00Z"},"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
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

	issueUpdatedAt := time.Date(2026, 5, 31, 2, 3, 4, 0, time.UTC)
	stageUpdatedAt := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	if got[0].UpdatedAt == nil || !got[0].UpdatedAt.Equal(issueUpdatedAt) {
		t.Fatalf("UpdatedAt = %v, want issue updatedAt %v", got[0].UpdatedAt, issueUpdatedAt)
	}
	if got[0].StageUpdatedAt == nil || !got[0].StageUpdatedAt.Equal(stageUpdatedAt) {
		t.Fatalf("StageUpdatedAt = %v, want status updatedAt %v", got[0].StageUpdatedAt, stageUpdatedAt)
	}
}

func TestConnectorFetchIssuesByStatesExtractsWorkpadHumanActionNeeded(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw98","number":98,"title":"Homebrew tap","body":"Depends on: #97","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/98","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Blocked"},"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw98","body":"Depends on: #97","comments":{"nodes":[{"body":"## Codex Workpad\n\n### Plan\n- Check prerequisites.\n\n### Human Action Needed\n- Create public repository ` + "`" + `digitaldrywood/homebrew-tap` + "`" + `.\n- Add repository Actions secret ` + "`" + `HOMEBREW_TAP_GITHUB_TOKEN` + "`" + `.\n\n### Validation Evidence\n- Not run."}]}}]}}`,
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
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if strings.Contains(requests[0]["query"].(string), "comments") {
		t.Fatalf("project query = %q, want no comments", requests[0]["query"])
	}
	if !strings.Contains(requests[1]["query"].(string), "comments") {
		t.Fatalf("comments query = %q, want comments", requests[1]["query"])
	}
}

func TestConnectorFetchIssueStatesByIDsUsesProjectStatusAndRequestOrder(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw1","number":1,"title":"First","body":"","state":"OPEN","url":"https://github.com/example/repo/issues/1","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"example/repo"},"closedByPullRequestsReferences":{"nodes":[{"number":81,"url":"https://github.com/example/repo/pull/81"}]},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Ready"},"priorityValue":{"name":"P1"}}]}},{"__typename":"Issue","id":"I_kw2","number":2,"title":"Second","body":"","state":"OPEN","url":"https://github.com/example/repo/issues/2","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"example/repo"},"closedByPullRequestsReferences":{"nodes":[]},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_2","project":{"id":"PVT_1"},"statusValue":{"name":"Reviewing"},"priorityValue":{"name":"No priority"}}]}}]}}`,
	}})

	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
		StateMap: map[string]string{
			"Todo":         "Ready",
			"Human Review": "Reviewing",
		},
		PriorityMap: map[string]*int{"P1": intPtr(2), "No priority": nil},
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
	if got[1].PRNumber == nil || *got[1].PRNumber != 81 {
		t.Fatalf("second PRNumber = %v, want 81", got[1].PRNumber)
	}
}

func TestConnectorFetchIssueStatesByIDsCapturesIssueMetadata(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw1","number":1,"title":"First","body":"","state":"OPEN","url":"https://github.com/example/repo/issues/1","createdAt":null,"updatedAt":null,"author":{"login":"author-1"},"assignees":{"nodes":[{"login":"worker-1"},{"login":"worker-2"}]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"example/repo"},"closedByPullRequestsReferences":{"nodes":[]},"projectItems":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Ready"},"priorityValue":{"name":"P1"},"fieldValues":{"nodes":[{"__typename":"ProjectV2ItemFieldSingleSelectValue","name":"Ready","field":{"name":"Status"}},{"__typename":"ProjectV2ItemFieldTextValue","text":"team-a","field":{"name":"Owner"}},{"__typename":"ProjectV2ItemFieldNumberValue","number":3,"field":{"name":"Weight"}}]}}]}}]}}`,
	}})

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
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_kw1","number":1,"title":"Later project","body":"","state":"OPEN","url":"https://github.com/example/repo/issues/1","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"example/repo"},"projectItems":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"},"nodes":[{"id":"PVTI_other","project":{"id":"PVT_other"},"statusValue":{"name":"Open"},"priorityValue":{"name":"P1"}}]}}]}}`,
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
		PriorityMap: map[string]*int{"P2": intPtr(3)},
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
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	variables := requests[1]["variables"].(map[string]any)
	if variables["after"] != "cursor-1" {
		t.Fatalf("after = %v, want cursor-1", variables["after"])
	}
	if variables["issueId"] != "I_kw1" {
		t.Fatalf("issueId = %v, want I_kw1", variables["issueId"])
	}
}

func TestConnectorCreateCommentCallsAddComment(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"addComment":{"commentEdge":{"node":{"id":"IC_kw1"}}}}}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})

	if err := c.CreateComment(context.Background(), "I_kw1", "hello"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	query := requests[0]["query"].(string)
	if !strings.Contains(query, "addComment") {
		t.Fatalf("query = %q, want addComment", query)
	}
	if strings.Contains(query, "rateLimit") {
		t.Fatalf("query = %q, want no rateLimit on mutation root", query)
	}
	variables := requests[0]["variables"].(map[string]any)
	if variables["subjectId"] != "I_kw1" {
		t.Fatalf("subjectId = %v, want I_kw1", variables["subjectId"])
	}
	if variables["body"] != "hello" {
		t.Fatalf("body = %v, want hello", variables["body"])
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
		{body: `{"data":{"user":{"id":"U_worker"}}}`},
		{body: `{"data":{"node":{"assignees":{"nodes":[]}}}}`},
		{body: `{"data":{"addAssigneesToAssignable":{"assignable":{"id":"I_kw1"}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{})

	if err := c.SetAssignee(context.Background(), " I_kw1 ", " worker-1 "); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	userVariables := requests[0]["variables"].(map[string]any)
	if userVariables["login"] != "worker-1" {
		t.Fatalf("login = %v, want worker-1", userVariables["login"])
	}
	currentVariables := requests[1]["variables"].(map[string]any)
	if currentVariables["issueId"] != "I_kw1" {
		t.Fatalf("current issueId = %v, want I_kw1", currentVariables["issueId"])
	}
	assignVariables := requests[2]["variables"].(map[string]any)
	if assignVariables["assignableId"] != "I_kw1" {
		t.Fatalf("assignableId = %v, want I_kw1", assignVariables["assignableId"])
	}
	assigneeIDs, ok := assignVariables["assigneeIds"].([]any)
	if !ok || len(assigneeIDs) != 1 || assigneeIDs[0] != "U_worker" {
		t.Fatalf("assigneeIds = %#v, want [U_worker]", assignVariables["assigneeIds"])
	}
	if !strings.Contains(requests[2]["query"].(string), "addAssigneesToAssignable") {
		t.Fatalf("assign query = %q, want addAssigneesToAssignable", requests[2]["query"])
	}
}

func TestConnectorSetAssigneeReplacesExistingAssignees(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: `{"data":{"user":{"id":"U_worker"}}}`},
		{body: `{"data":{"node":{"assignees":{"nodes":[{"id":"U_old","login":"old-owner"},{"id":"U_worker","login":"worker-1"}]}}}}`},
		{body: `{"data":{"removeAssigneesFromAssignable":{"assignable":{"id":"I_kw1"}}}}`},
	})
	c := newGitHubTestConnector(t, server, Config{})

	if err := c.SetAssignee(context.Background(), "I_kw1", "worker-1"); err != nil {
		t.Fatalf("SetAssignee() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	removeVariables := requests[2]["variables"].(map[string]any)
	if removeVariables["assignableId"] != "I_kw1" {
		t.Fatalf("remove assignableId = %v, want I_kw1", removeVariables["assignableId"])
	}
	assigneeIDs, ok := removeVariables["assigneeIds"].([]any)
	if !ok || len(assigneeIDs) != 1 || assigneeIDs[0] != "U_old" {
		t.Fatalf("removed assigneeIds = %#v, want [U_old]", removeVariables["assigneeIds"])
	}
	if strings.Contains(requests[2]["query"].(string), "addAssigneesToAssignable") {
		t.Fatalf("replace query added already assigned target: %q", requests[2]["query"])
	}
	if !strings.Contains(requests[2]["query"].(string), "removeAssigneesFromAssignable") {
		t.Fatalf("replace query = %q, want removeAssigneesFromAssignable", requests[2]["query"])
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

type graphqlTestServer struct {
	*httptest.Server
	t         *testing.T
	mu        sync.Mutex
	responses []graphqlTestResponse
	seen      []map[string]any
}

type graphqlTestResponse struct {
	status int
	body   string
}

func newGraphQLTestServer(t *testing.T, responses []graphqlTestResponse) *graphqlTestServer {
	t.Helper()

	server := &graphqlTestServer{t: t, responses: responses}
	server.Server = httptest.NewServer(http.HandlerFunc(server.serveHTTP))
	t.Cleanup(server.Close)
	return server
}

func (s *graphqlTestServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.t.Fatalf("Decode() error = %v", err)
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

	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
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

func intPtr(value int) *int {
	return &value
}
