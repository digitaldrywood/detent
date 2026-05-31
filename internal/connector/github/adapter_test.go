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

	"github.com/digitaldrywood/symphony/internal/connector"
)

func TestConnectorFetchCandidateIssuesNormalizesProjectItems(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw1","number":26,"title":"GitHub adapter","body":"Depends on: #24 digitaldrywood/symphony#25\n<!-- model: gpt-5-codex-high -->","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/26","createdAt":"2026-05-31T01:02:03Z","updatedAt":"2026-05-31T02:03:04Z","assignees":{"nodes":[{"login":"worker-1"}]},"labels":{"nodes":[{"name":"Enhancement"},{"name":"stage:S4"}]},"repository":{"nameWithOwner":"digitaldrywood/symphony"},"closedByPullRequestsReferences":{"nodes":[{"number":42,"url":"https://github.com/digitaldrywood/symphony/pull/42"}]}},"statusValue":{"name":"Ready"},"priorityValue":{"name":"P0"}},{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_kw2","number":27,"title":"Backlog item","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/27","createdAt":"2026-05-31T03:02:03Z","updatedAt":"2026-05-31T04:03:04Z","assignees":{"nodes":[{"login":"worker-1"}]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"},"closedByPullRequestsReferences":{"nodes":[]}},"statusValue":{"name":"Backlog"},"priorityValue":{"name":"No priority"}}]}}}}`,
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
		Identifier:       "digitaldrywood/symphony#26",
		Title:            "GitHub adapter",
		Description:      "Depends on: #24 digitaldrywood/symphony#25\n<!-- model: gpt-5-codex-high -->",
		Priority:         &priority,
		State:            "Todo",
		URL:              "https://github.com/digitaldrywood/symphony/issues/26",
		PRNumber:         &prNumber,
		AssigneeID:       "worker-1",
		BlockedBy:        []connector.BlockedRef{{Identifier: "digitaldrywood/symphony#24"}, {Identifier: "digitaldrywood/symphony#25"}},
		Labels:           []string{"enhancement", "stage:s4"},
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
	if prVariables["owner"] != "digitaldrywood" || prVariables["name"] != "symphony" {
		t.Fatalf("pull request repo variables = %#v, want digitaldrywood/symphony", prVariables)
	}
}

func TestConnectorFetchCandidateIssuesResolvesBlockedByProjectState(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_candidate","number":26,"title":"Candidate","body":"Depends on: DigitalDryWood/Symphony#24 digitaldrywood/symphony#25 digitaldrywood/symphony#999","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/26","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"}},"statusValue":{"name":"Ready"},"priorityValue":null},{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_done","number":24,"title":"Done blocker","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/24","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"}},"statusValue":{"name":"Done"},"priorityValue":null},{"id":"PVTI_3","content":{"__typename":"Issue","id":"I_progress","number":25,"title":"Active blocker","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/25","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"}},"statusValue":{"name":"Working"},"priorityValue":null}]}}}}`,
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
		{ID: "I_done", Identifier: "digitaldrywood/symphony#24", State: "Done"},
		{ID: "I_progress", Identifier: "digitaldrywood/symphony#25", State: "In Progress"},
		{Identifier: "digitaldrywood/symphony#999"},
	}
	if !reflect.DeepEqual(got[0].BlockedBy, want) {
		t.Fatalf("BlockedBy = %#v, want %#v", got[0].BlockedBy, want)
	}
}

func TestConnectorFetchCandidateIssuesAttachesPullRequestByBranchPrefix(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_182","content":{"__typename":"Issue","id":"I_182","number":182,"title":"First issue","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/182","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"}},"statusValue":{"name":"Todo"},"priorityValue":null},{"id":"PVTI_18","content":{"__typename":"Issue","id":"I_18","number":18,"title":"Prefix neighbor","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/18","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
		},
		{
			body: `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"number":187,"url":"https://github.com/digitaldrywood/symphony/pull/187","state":"OPEN","headRefName":"symphony/digitaldrywood_symphony_182_followup"},{"number":188,"url":"https://github.com/digitaldrywood/symphony/pull/188","state":"MERGED","headRefName":"symphony/digitaldrywood_symphony_181"}]}}}}`,
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
	if pr.Number != 187 || pr.State != "OPEN" || pr.BranchName != "symphony/digitaldrywood_symphony_182_followup" {
		t.Fatalf("I_182 PullRequest = %#v, want PR 187 open followup", pr)
	}
	if byID["I_18"].PullRequest != nil {
		t.Fatalf("I_18 PullRequest = %#v, want nil", byID["I_18"].PullRequest)
	}
}

func TestConnectorFetchCandidateIssuesLimitsPullRequestPagination(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_1","number":1,"title":"Candidate","body":"","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/1","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"}},"statusValue":{"name":"Todo"},"priorityValue":null}]}}}}`,
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

func TestConnectorFetchIssuesByStatesExtractsWorkpadHumanActionNeeded(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_kw98","number":98,"title":"Homebrew tap","body":"Depends on: #97","state":"OPEN","url":"https://github.com/digitaldrywood/symphony/issues/98","createdAt":null,"updatedAt":null,"assignees":{"nodes":[]},"labels":{"nodes":[]},"repository":{"nameWithOwner":"digitaldrywood/symphony"}},"statusValue":{"name":"Blocked"},"priorityValue":null}]}}}}`,
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
