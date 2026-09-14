package github

import (
	"context"
	"net/http"
	"strings"
	"testing"
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
	if len(drift.LaneSignalCandidates[0].Fields) != 0 {
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
	if len(drift.LaneSignalCandidates) != 1 || drift.LaneSignalCandidates[0].Fields["Workflow Status"] != "Todo" {
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
