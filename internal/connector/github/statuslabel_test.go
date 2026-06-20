package github

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorFetchCandidateIssuesUsesStatusLabels(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues?labels=detent%3Aready&page=1&per_page=100&state=all",
			body:   `[{"node_id":"I_485","number":485,"title":"Installer packages","body":"Ship packages","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/485","assignees":[],"labels":[{"name":"detent:ready"},{"name":"enhancement"}]}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo"},
		StateMap:           map[string]string{"Todo": "Ready"},
	})

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(got))
	}
	want := connector.Issue{
		ID:               "I_485",
		Identifier:       "digitaldrywood/detent#485",
		Title:            "Installer packages",
		Description:      "Ship packages",
		State:            "Todo",
		URL:              "https://github.com/digitaldrywood/detent/issues/485",
		BlockedBy:        []connector.BlockedRef{},
		Labels:           []string{"detent:ready", "enhancement"},
		Assignees:        []string{},
		Fields:           map[string]string{"Status": "Ready"},
		AssignedToWorker: true,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("FetchCandidateIssues()[0] = %#v, want %#v", got[0], want)
	}
}

func TestConnectorFetchLabelIssuesByStatesAttachesCurrentAgentBranchPullRequest(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/digitaldrywood/issues?labels=detent%3Ahuman-review&page=1&per_page=100&state=all",
			body:   `[{"node_id":"I_433","number":433,"title":"Human Review issue","body":"","state":"open","html_url":"https://github.com/digitaldrywood/digitaldrywood/issues/433","assignees":[],"labels":[{"name":"detent:human-review"},{"name":"bug"}]}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/digitaldrywood/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[{"number":434,"html_url":"https://github.com/digitaldrywood/digitaldrywood/pull/434","state":"open","head":{"ref":"detent/digitaldrywood-digitaldrywood_digitaldrywood_433-a212db2634a4","sha":"sha-434"}}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/digitaldrywood/commits/sha-434/check-runs?per_page=100",
			body:   `{"check_runs":[{"status":"completed","conclusion":"success"}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/digitaldrywood/commits/sha-434/statuses?per_page=100",
			body:   `[]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/digitaldrywood/pulls/434/reviews?per_page=100",
			body:   `[]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/digitaldrywood",
	})

	got, err := c.FetchIssuesByStatesLimit(context.Background(), []string{"Human Review"}, 1)
	if err != nil {
		t.Fatalf("FetchIssuesByStatesLimit() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStatesLimit() len = %d, want 1", len(got))
	}
	if got[0].PRNumber == nil || *got[0].PRNumber != 434 {
		t.Fatalf("PRNumber = %v, want 434", got[0].PRNumber)
	}
	if got[0].PullRequest == nil || got[0].PullRequest.Number != 434 || got[0].PullRequest.CIStatus != "pass" {
		t.Fatalf("PullRequest = %#v, want hydrated PR 434", got[0].PullRequest)
	}
}

func TestConnectorFetchLabelIssuesPrefersCanonicalDoneOverCancelledAlias(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues?labels=detent%3Adone&page=1&per_page=100&state=all",
			body:   `[{"node_id":"I_485","number":485,"title":"Installer packages","body":"","state":"closed","state_reason":"completed","html_url":"https://github.com/digitaldrywood/detent/issues/485","assignees":[],"labels":[{"name":"detent:done"},{"name":"release"}]}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		TerminalStates:     []string{"Done", "Cancelled"},
		StateMap:           map[string]string{"Cancelled": "Done"},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(got))
	}
	if got[0].State != "Done" {
		t.Fatalf("State = %q, want Done", got[0].State)
	}
	if !got[0].Closed || got[0].ClosedReason != "completed" {
		t.Fatalf("closed metadata = (%v, %q), want closed completed", got[0].Closed, got[0].ClosedReason)
	}
}

func TestConnectorFetchLabelIssueStateByIDMapsClosedCompletedToDone(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_487","number":487,"repository":{"nameWithOwner":"digitaldrywood/detent"}}]}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/487",
			body:   `{"node_id":"I_487","number":487,"title":"Windows packages","body":"","state":"closed","state_reason":"completed","html_url":"https://github.com/digitaldrywood/detent/issues/487","assignees":[],"labels":[{"name":"release"}]}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		TerminalStates:     []string{"Done", "Cancelled"},
		StateMap:           map[string]string{"Cancelled": "Done"},
	})

	got, err := c.FetchIssueStatesByIDs(context.Background(), []string{"I_487"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueStatesByIDs() len = %d, want 1", len(got))
	}
	if got[0].State != "Done" {
		t.Fatalf("State = %q, want Done", got[0].State)
	}
	if !got[0].Closed || got[0].ClosedReason != "completed" {
		t.Fatalf("closed metadata = (%v, %q), want closed completed", got[0].Closed, got[0].ClosedReason)
	}
}

func TestConnectorUpdateIssueStateReplacesStatusLabels(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_485","number":485,"repository":{"nameWithOwner":"digitaldrywood/detent"}}]}}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/485",
			body:   `{"node_id":"I_485","number":485,"title":"Installer packages","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/485","assignees":[],"labels":[{"name":"detent:todo"},{"name":"bug"}]}`,
		},
		{
			method: http.MethodDelete,
			path:   "/repos/digitaldrywood/detent/issues/485/labels/detent:todo",
			body:   `[{"name":"bug"}]`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/issues/485/labels",
			body:   `[{"name":"detent:in-progress"}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo", "In Progress"},
	})

	if err := c.UpdateIssueState(context.Background(), "I_485", "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want 4", len(requests))
	}
	body := requests[3]["body"].(map[string]any)
	labels := body["labels"].([]any)
	if !reflect.DeepEqual(labels, []any{"detent:in-progress"}) {
		t.Fatalf("labels body = %#v, want detent:in-progress", labels)
	}
}

func TestConnectorEnsureLabelStateOptionsCreatesMissingLabels(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/labels?per_page=100",
			body:   `[{"name":"detent:todo"}]`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/labels",
			body:   `{"name":"detent:in-progress"}`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/labels",
			body:   `{"name":"detent:reviewing"}`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/labels",
			body:   `{"name":"detent:done"}`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/labels",
			body:   `{"name":"detent:backlog"}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo", "In Progress"},
		ObservedStates:     []string{"Human Review"},
		TerminalStates:     []string{"Done"},
		StateMap:           map[string]string{"Human Review": "Reviewing"},
	})

	if err := c.EnsureStateOptions(context.Background()); err != nil {
		t.Fatalf("EnsureStateOptions() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 5 {
		t.Fatalf("request count = %d, want 5", len(requests))
	}
	got := []string{}
	for _, request := range requests[1:] {
		body := request["body"].(map[string]any)
		got = append(got, body["name"].(string))
	}
	want := []string{"detent:in-progress", "detent:reviewing", "detent:done", "detent:backlog"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("created labels = %#v, want %#v", got, want)
	}
}

func TestConnectorFetchIssueChildrenLabelModeAvoidsProjectItems(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"subIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_done","number":251,"title":"Done child","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/251","labels":{"nodes":[{"name":"detent:done"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"}}]}}}}`,
		},
		{
			body: `{"data":{"node":{"trackedIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_todo","number":252,"title":"Todo child","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/252","labels":{"nodes":[{"name":"detent:todo"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"}}]}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo"},
		TerminalStates:     []string{"Done"},
	})

	got, err := c.FetchIssueChildren(context.Background(), "I_parent")
	if err != nil {
		t.Fatalf("FetchIssueChildren() error = %v", err)
	}
	want := []connector.BlockedRef{
		{ID: "I_done", Identifier: "digitaldrywood/detent#251", State: "Done"},
		{ID: "I_todo", Identifier: "digitaldrywood/detent#252", State: "Todo"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueChildren() = %#v, want %#v", got, want)
	}

	for index, request := range server.requests() {
		if query := request["query"].(string); containsProjectItems(query) {
			t.Fatalf("request %d query contains projectItems:\n%s", index, query)
		}
	}
}

func TestConnectorFetchIssueParentsLabelModeAvoidsProjectItems(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"id":"I_child","number":251,"repository":{"nameWithOwner":"digitaldrywood/detent"},"parent":{"__typename":"Issue","id":"I_parent","number":258,"title":"Epic: Parent","body":"- [ ] #251","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/258","createdAt":null,"updatedAt":null,"author":{"login":"corylanou"},"assignees":{"nodes":[]},"labels":{"nodes":[{"name":"detent:todo"},{"name":"epic"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"},"closedByPullRequestsReferences":{"nodes":[]},"subIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"I_child","number":251,"title":"Child","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/251","labels":{"nodes":[{"name":"detent:done"}]},"repository":{"nameWithOwner":"digitaldrywood/detent"}}]},"trackedIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}},"trackedInIssues":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`,
		},
		{
			method: http.MethodGet,
			path:   "/search/issues?page=1&per_page=100&q=user%3Adigitaldrywood+is%3Aissue+is%3Aopen+251",
			body:   `{"total_count":0,"items":[]}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo"},
		TerminalStates:     []string{"Done"},
	})

	got, err := c.FetchIssueParents(context.Background(), "I_child")
	if err != nil {
		t.Fatalf("FetchIssueParents() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueParents() len = %d, want 1", len(got))
	}
	if got[0].ID != "I_parent" || got[0].State != "Todo" {
		t.Fatalf("parent = %#v, want label-backed Todo parent", got[0])
	}
	if got[0].ChildIssues[0] != (connector.BlockedRef{ID: "I_child", Identifier: "digitaldrywood/detent#251", State: "Done"}) {
		t.Fatalf("parent child issue = %#v, want label-backed Done child", got[0].ChildIssues[0])
	}
	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want parent query and body-reference search", len(requests))
	}
	if query := requests[0]["query"].(string); containsProjectItems(query) {
		t.Fatalf("parent query contains projectItems:\n%s", query)
	}
}

func containsProjectItems(query string) bool {
	return strings.Contains(query, "projectItems")
}
