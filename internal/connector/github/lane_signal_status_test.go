package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestFetchStatusDriftKeepsLabelTrackingAvailableWithoutProjectsPermission(t *testing.T) {
	t.Parallel()
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues?page=1&per_page=100&state=open",
			body:   `[{"node_id":"I_1","number":1,"title":"Backlog","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1","assignees":[],"labels":[{"name":"detent:backlog"}]}]`,
		},
		{
			method: http.MethodGet,
			path:   "/search/issues?order=asc&page=1&per_page=100&q=repo%3Adigitaldrywood%2Fdetent+is%3Aissue+is%3Aclosed+label%3A%22detent%3Atodo%22&sort=created",
			body:   `{"total_count":0,"items":[]}`,
		},
		{
			body: `{"data":{},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo"},
		ObservedStates:     []string{"Backlog"},
		TerminalStates:     []string{"Done"},
	})

	drift, err := c.FetchStatusDrift(context.Background())
	if err != nil {
		t.Fatalf("FetchStatusDrift() error = %v, want optional diagnostic failure ignored", err)
	}
	if len(drift.LaneSignalCandidates) != 1 || drift.LaneSignalCandidates[0].State != "Backlog" {
		t.Fatalf("LaneSignalCandidates = %#v, want ordinary label-backed issue", drift.LaneSignalCandidates)
	}
	if len(drift.LaneSignalCandidates[0].LaneSignalStatuses) != 0 {
		t.Fatalf("Fields = %#v, want no synthetic Status", drift.LaneSignalCandidates[0].Fields)
	}
}

func TestFetchStatusDriftPaginatesIgnoredProjectStatuses(t *testing.T) {
	t.Parallel()
	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues?page=1&per_page=100&state=open",
			body:   `[{"node_id":"I_1","number":1,"title":"Backlog","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1","assignees":[],"labels":[{"name":"detent:backlog"}]}]`,
		},
		{
			method: http.MethodGet,
			path:   "/search/issues?order=asc&page=1&per_page=100&q=repo%3Adigitaldrywood%2Fdetent+is%3Aissue+is%3Aclosed+label%3A%22detent%3Atodo%22&sort=created",
			body:   `{"total_count":0,"items":[]}`,
		},
		{
			body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_1","projectItems":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"},"nodes":[{"project":{"id":"PVT_other"},"statusValue":{"name":"Not a lane"}}]}}]}}`,
		},
		{
			body: `{"data":{"node":{"__typename":"Issue","id":"I_1","projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"project":{"id":"PVT_target"},"statusValue":{"name":"Todo"}}]}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Workflow Status",
		ActiveStates:       []string{"Todo"},
		ObservedStates:     []string{"Backlog"},
		TerminalStates:     []string{"Done"},
	})

	drift, err := c.FetchStatusDrift(context.Background())
	if err != nil {
		t.Fatalf("FetchStatusDrift() error = %v", err)
	}
	if len(drift.LaneSignalCandidates) != 1 || len(drift.LaneSignalCandidates[0].LaneSignalStatuses) != 1 || drift.LaneSignalCandidates[0].LaneSignalStatuses[0].Value != "Todo" {
		t.Fatalf("LaneSignalCandidates = %#v, want paginated Todo Status", drift.LaneSignalCandidates)
	}
	requests := server.requests()
	if len(requests) != 4 || !strings.Contains(requests[3]["query"].(string), "DetentGitHubLaneSignalStatusesPage") {
		t.Fatalf("requests = %#v, want paginated lane signal query", requests)
	}
	for _, index := range []int{2, 3} {
		variables := requests[index]["variables"].(map[string]any)
		if variables["statusField"] != "Workflow Status" {
			t.Fatalf("request %d statusField = %v, want Workflow Status", index, variables["statusField"])
		}
	}
}

func TestFetchRefreshIssuesRetainsOutOfLaneDiagnostics(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"Triage", "Waiting for intake"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			node := projectIssueNode("PVTI_1", "I_1", 1, "Ignored Todo label", status)
			node = strings.Replace(node, `"labels":{"nodes":[]}`, `"labels":{"nodes":[{"name":"detent:todo"}]}`, 1)
			server := newGraphQLTestServer(t, []graphqlTestResponse{{
				body: projectItemsPageResponseWithTotal(1, false, "", []string{node}),
			}})
			c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1", ActiveStates: []string{"Todo"}})
			result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, []string{"Backlog"}, connector.IssueFilterHint{})
			if result.CandidateError != nil || result.StatusError != nil {
				t.Fatalf("refresh errors = %v, %v", result.CandidateError, result.StatusError)
			}
			if len(result.Candidates) != 0 || len(result.Statuses) != 0 {
				t.Fatalf("out-of-lane issue entered scheduling: %#v", result)
			}
			if len(result.LaneSignalCandidates) != 1 || result.LaneSignalCandidates[0].State != status || result.LaneSignalCandidates[0].Labels[0] != "detent:todo" {
				t.Fatalf("diagnostic candidates = %#v, want ignored Todo label in %s", result.LaneSignalCandidates, status)
			}
		})
	}
}

func TestFetchStatusDriftRetainsEveryProjectStatus(t *testing.T) {
	t.Parallel()
	for _, secondStatus := range []string{"Backlog", "Todo"} {
		t.Run(secondStatus, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues?page=1&per_page=100&state=open",
					body: `[{"node_id":"I_1","number":1,"state":"open","labels":[{"name":"detent:backlog"}]}]`},
				{method: http.MethodGet, path: "/search/issues?order=asc&page=1&per_page=100&q=repo%3Adigitaldrywood%2Fdetent+is%3Aissue+is%3Aclosed+label%3A%22detent%3Atodo%22&sort=created", body: `{"total_count":0,"items":[]}`},
				{body: fmt.Sprintf(`{"data":{"nodes":[{"__typename":"Issue","id":"I_1","projectItems":{"nodes":[
					{"project":{"id":"PVT_a","title":"Delivery","url":"https://github.com/orgs/example/projects/1"},"statusValue":{"name":"Todo"}},
					{"project":{"id":"PVT_b","title":"Intake","url":"https://github.com/orgs/example/projects/2"},"statusValue":{"name":%q}}
				]}}]}}`, secondStatus)},
			})
			c := newGitHubTestConnector(t, server, Config{
				GitHubStatusSource: GitHubStatusSourceLabel, Repository: "digitaldrywood/detent",
				ActiveStates: []string{"Todo"}, ObservedStates: []string{"Todo", "Backlog"},
			})
			drift, err := c.FetchStatusDrift(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(drift.LaneSignalCandidates) != 1 {
				t.Fatalf("candidates = %#v", drift.LaneSignalCandidates)
			}
			statuses := drift.LaneSignalCandidates[0].LaneSignalStatuses
			if len(statuses) != 2 || statuses[0].Value != "Todo" || statuses[1].Value != secondStatus ||
				statuses[0].ProjectTitle != "Delivery" || statuses[1].ProjectID != "PVT_b" || statuses[1].ProjectURL != "https://github.com/orgs/example/projects/2" {
				t.Fatalf("statuses = %#v, want both values with project context", statuses)
			}
		})
	}
}
