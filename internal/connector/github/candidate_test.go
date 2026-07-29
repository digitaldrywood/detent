package github

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorReadProjectCandidatesStopsAtBoundAndSorts(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: projectItemsPageResponseWithTotal(3, true, "cursor-1", []string{
				`{"id":"PVTI_1","content":{"__typename":"Issue","id":"I_1","number":1,"title":"First","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/1","createdAt":"2026-01-02T00:00:00Z","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Backlog"}}`,
			}),
		},
		{
			body: projectItemsPageResponseWithTotal(3, true, "cursor-2", []string{
				`{"id":"PVTI_2","content":{"__typename":"Issue","id":"I_2","number":2,"title":"Second","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/2","createdAt":"2026-01-01T00:00:00Z","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Backlog"}}`,
			}),
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
	})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Backlog"},
		Limit:    1,
		PageSize: 1,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if ids := githubIssueIDs(got.Issues); !reflect.DeepEqual(ids, []string{"I_2"}) {
		t.Fatalf("candidate IDs = %#v, want [I_2]", ids)
	}
	if !got.Truncated || got.PagesRead != 2 {
		t.Fatalf("result = %#v, want bounded two-page truncation", got)
	}
	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if first := requests[0]["variables"].(map[string]any)["first"]; first != float64(1) {
		t.Fatalf("first page size = %#v, want 1", first)
	}
	if after := requests[1]["variables"].(map[string]any)["after"]; after != "cursor-1" {
		t.Fatalf("second page cursor = %#v, want cursor-1", after)
	}
	query := requests[0]["query"].(string)
	for _, want := range []string{"createdAt", "orderBy: {field: POSITION, direction: ASC}"} {
		if !strings.Contains(query, want) {
			t.Fatalf("candidate query missing %q", want)
		}
	}
}

func TestConnectorReadProjectCandidatesBoundsEmptyCursorPages(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{body: projectItemsPageResponseWithTotal(3, true, "cursor-1", nil)},
		{body: projectItemsPageResponseWithTotal(3, true, "cursor-2", nil)},
	})
	c := newGitHubTestConnector(t, server, Config{ProjectSlug: "PVT_1"})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Backlog"},
		Limit:    1,
		PageSize: 1,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if len(got.Issues) != 0 || !got.Truncated || got.PagesRead != 2 {
		t.Fatalf("result = %#v, want empty bounded truncation after two pages", got)
	}
}

func TestConnectorReadLabelCandidatesFiltersConflictedStateLocally(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodGet,
		body: `[{"node_id":"I_1","number":1,"title":"Conflicted","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1",
			"labels":[{"name":"detent:todo"},{"name":"detent:blocked"}]}]`,
	}})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo"},
		ObservedStates:     []string{"Blocked"},
	})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Todo"},
		Limit:    10,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if len(got.Issues) != 0 || got.Truncated {
		t.Fatalf("result = %#v, want honestly filtered complete result", got)
	}
}

func TestConnectorReadLabelCandidatesKeepsNumericPageSizeAtBound(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			body: `[
				{"node_id":"I_2","number":2,"title":"Second","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/2","labels":[{"name":"detent:todo"}]},
				{"node_id":"I_3","number":3,"title":"Third","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/3","labels":[{"name":"detent:todo"}]}
			]`,
		},
		{
			method: http.MethodGet,
			body: `[
				{"node_id":"I_1","number":1,"title":"First","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1","labels":[{"name":"detent:todo"}]},
				{"node_id":"I_4","number":4,"title":"Fourth","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/4","labels":[{"name":"detent:todo"}]}
			]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo"},
	})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Todo"},
		Limit:    2,
		PageSize: 2,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if ids := githubIssueIDs(got.Issues); !reflect.DeepEqual(ids, []string{"I_1", "I_2"}) {
		t.Fatalf("candidate IDs = %#v, want [I_1 I_2]", ids)
	}
	if !got.Truncated || got.PagesRead != 2 {
		t.Fatalf("result = %#v, want bounded two-page truncation", got)
	}
	requests := server.requests()
	for index, want := range []string{"page=1&per_page=2", "page=2&per_page=2"} {
		if path := requests[index]["path"].(string); !strings.Contains(path, want) {
			t.Fatalf("request %d path = %q, missing %q", index, path, want)
		}
	}
}

func TestConnectorReadLabelCandidatesSharesBoundAcrossStates(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			body: `[
				{"node_id":"I_2","number":2,"title":"Todo second","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/2","created_at":"2026-01-02T00:00:00Z","labels":[{"name":"detent:todo"}]},
				{"node_id":"I_3","number":3,"title":"Todo third","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/3","created_at":"2026-01-03T00:00:00Z","labels":[{"name":"detent:todo"}]}
			]`,
		},
		{
			method: http.MethodGet,
			body: `[
				{"node_id":"I_1","number":1,"title":"Backlog first","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1","created_at":"2026-01-01T00:00:00Z","labels":[{"name":"detent:backlog"}]}
			]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceLabel,
		Repository:         "digitaldrywood/detent",
		ActiveStates:       []string{"Todo"},
		ObservedStates:     []string{"Backlog"},
	})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Todo", "Backlog"},
		Limit:    1,
		PageSize: 2,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if ids := githubIssueIDs(got.Issues); !reflect.DeepEqual(ids, []string{"I_1"}) {
		t.Fatalf("candidate IDs = %#v, want [I_1]", ids)
	}
	if !got.Truncated || got.PagesRead != 2 {
		t.Fatalf("result = %#v, want bounded reads from both states", got)
	}
	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	for index, want := range []string{"labels=detent%3Atodo", "labels=detent%3Abacklog"} {
		if path := requests[index]["path"].(string); !strings.Contains(path, want) {
			t.Fatalf("request %d path = %q, missing %q", index, path, want)
		}
	}
}

func TestConnectorReadIssueFieldCandidatesFiltersAndOrdersSearch(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
			body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":1,"name":"Ready","color":"green"}]}]`,
		},
		{
			method: http.MethodGet,
			body:   `{"total_count":1,"items":[{"node_id":"I_1","number":1,"title":"Ready issue","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1","labels":[]}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values?per_page=100",
			body:   `[{"issue_field_id":10,"node_id":"IFV_1","data_type":"single_select","value":1,"single_select_option":{"id":1,"name":"Ready","color":"green"}}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
		StateMap:           map[string]string{"Todo": "Ready"},
	})

	got, err := c.ReadCandidates(context.Background(), connector.CandidateRequest{
		Selector: connector.CandidateSelectorStates,
		States:   []string{"Todo"},
		Limit:    10,
		PageSize: 5,
	})
	if err != nil {
		t.Fatalf("ReadCandidates() error = %v", err)
	}
	if ids := githubIssueIDs(got.Issues); !reflect.DeepEqual(ids, []string{"I_1"}) {
		t.Fatalf("candidate IDs = %#v, want [I_1]", ids)
	}
	searchPath := server.requests()[1]["path"].(string)
	for _, want := range []string{"order=asc", "sort=created", "per_page=5"} {
		if !strings.Contains(searchPath, want) {
			t.Fatalf("search path = %q, missing %q", searchPath, want)
		}
	}
}
