package github

import (
	"context"
	"errors"
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

func TestFetchStatusDriftRetainsIssueFieldDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		status   string
		label    string
		want     bool
		prefix   string
		stateMap map[string]string
	}{
		{name: "unconfigured triage", status: "Triage", label: "detent:todo", want: true},
		{name: "unconfigured waiting", status: "Waiting for intake", label: "detent:todo", want: true},
		{name: "configured lane", status: "Todo", label: "detent:todo", want: true},
		{name: "mapped lane and custom prefix", status: "Triage", label: "workflow:ready", prefix: "workflow:", stateMap: map[string]string{"Todo": "Ready"}, want: true},
		{name: "unrelated label", status: "Triage", label: "bug"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			responses := []graphqlTestResponse{{method: http.MethodGet,
				path: "/repos/digitaldrywood/detent/issues?page=1&per_page=100&state=open",
				body: fmt.Sprintf(`[{"node_id":"I_1","number":1,"state":"open","user":{"login":"alice"},"labels":[{"name":%q}]},{"node_id":"PR_2","number":2,"state":"open","pull_request":{},"labels":[{"name":"detent:todo"}]}]`, tt.label)}}
			if tt.want {
				responses = append(responses, graphqlTestResponse{body: fmt.Sprintf(`{"data":{"nodes":[{"__typename":"Issue","id":"I_1","issueFieldValues":{"nodes":[{"name":%q,"field":{"name":"Status"}}]}}]}}`, tt.status)})
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceIssueField,
				Repository: "digitaldrywood/detent", StatusLabelPrefix: tt.prefix, StateMap: tt.stateMap, ActiveStates: []string{"Todo"}, ObservedStates: []string{"Backlog", "Todo"}})
			drift, err := c.FetchStatusDrift(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if tt.want {
				if len(drift.LaneSignalCandidates) != 1 {
					t.Fatalf("diagnostic candidates = %#v, want one", drift.LaneSignalCandidates)
				}
				issue := drift.LaneSignalCandidates[0]
				if issue.State != tt.status || issue.Fields["Status"] != tt.status || issue.Labels[0] != tt.label || issue.AuthorID != "alice" {
					t.Fatalf("diagnostic = %#v, want original field state, label and author", issue)
				}
			} else if len(drift.LaneSignalCandidates) != 0 {
				t.Fatalf("unexpected diagnostics: %#v", drift.LaneSignalCandidates)
			}
			if len(drift.UntrackedOpen)+len(drift.OpenTerminal)+len(drift.ClosedActive) != 0 {
				t.Fatalf("issue-field diagnostics became label drift: %#v", drift)
			}
			if got := len(server.requests()); got != len(responses) {
				t.Fatalf("requests = %d, want %d read-only requests", got, len(responses))
			}
		})
	}
}

func TestFetchStatusDriftIssueFieldPermissionErrors(t *testing.T) {
	t.Parallel()
	for _, denied := range []string{"repository", "values"} {
		t.Run(denied, func(t *testing.T) {
			t.Parallel()
			responses := []graphqlTestResponse{
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues?page=1&per_page=100&state=open",
					body: `[{"node_id":"I_1","number":1,"state":"open","labels":[{"name":"detent:todo"}]}]`},
				{},
			}
			index := map[string]int{"repository": 0, "values": 1}[denied]
			responses = responses[:index+1]
			responses[index].status = http.StatusForbidden
			responses[index].body = `{"message":"Resource not accessible by integration"}`
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceIssueField,
				Repository: "digitaldrywood/detent", ActiveStates: []string{"Todo"}})
			drift, err := c.FetchStatusDrift(t.Context())
			var statusErr *StatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusForbidden {
				t.Fatalf("FetchStatusDrift error = %v, want preserved forbidden error", err)
			}
			if len(drift.LaneSignalCandidates) != 0 {
				t.Fatalf("failed read returned diagnostics: %#v", drift)
			}
		})
	}
}

func TestIssueFieldNormalReadsExcludeDiagnosticStates(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"Triage", "Waiting for intake"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			server := newGraphQLTestServer(t, []graphqlTestResponse{
				{method: http.MethodGet, path: "/orgs/digitaldrywood/issue-fields?per_page=100",
					body: `[{"id":10,"name":"Status","data_type":"single_select","options":[{"id":1,"name":"Todo"}]}]`},
				{method: http.MethodGet,
					body: `{"total_count":1,"items":[{"node_id":"I_1","number":1,"state":"open","labels":[{"name":"detent:todo"}]}]}`},
				{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/1/issue-field-values?per_page=100",
					body: fmt.Sprintf(`[{"issue_field_id":10,"data_type":"single_select","single_select_option":{"id":2,"name":%q}}]`, status)},
			})
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceIssueField,
				Repository: "digitaldrywood/detent", ActiveStates: []string{"Todo"}})
			result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, nil, connector.IssueFilterHint{Authors: []string{"alice"}})
			if result.CandidateError != nil || result.StatusError != nil {
				t.Fatalf("refresh = %#v", result)
			}
			if len(result.Candidates)+len(result.Statuses) != 0 {
				t.Fatalf("diagnostic entered normal reads: %#v", result)
			}
			requests := server.requests()
			path := requests[1]["path"].(string)
			if !strings.Contains(path, "field.Status%3ATodo") || !strings.Contains(path, "author%3Aalice") {
				t.Fatalf("normal search lost state or authorization qualifier: %s", path)
			}
		})
	}
}

func TestLaneSignalIssueFieldStatusPages(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		body    string
		wantErr error
		next    bool
	}{
		{name: "later status page", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_1","issueFieldValues":{"nodes":[{}, {"name":"High","field":{"name":"Priority"}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}]}}`, next: true},
		{name: "empty field connection", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_1","issueFieldValues":{"nodes":[]}}]}}`},
		{name: "missing node", body: `{"data":{"nodes":[]}}`, wantErr: ErrInvalidResponse},
		{name: "null node", body: `{"data":{"nodes":[null]}}`, wantErr: ErrInvalidResponse},
		{name: "unavailable fields", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_1","issueFieldValues":null}]}}`, wantErr: ErrInvalidResponse},
		{name: "wrong issue", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_other","issueFieldValues":{"nodes":[]}}]}}`, wantErr: ErrInvalidResponse},
		{name: "missing cursor", body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_1","issueFieldValues":{"nodes":[],"pageInfo":{"hasNextPage":true}}}]}}`, wantErr: ErrInvalidResponse},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			responses := []graphqlTestResponse{{body: tt.body}}
			if tt.next {
				responses = append(responses, graphqlTestResponse{body: `{"data":{"nodes":[{"__typename":"Issue","id":"I_1","issueFieldValues":{"nodes":[{"name":"Ready","field":{"name":"Workflow"}}],"pageInfo":{"hasNextPage":false}}}]}}`})
			}
			server := newGraphQLTestServer(t, responses)
			c := newGitHubTestConnector(t, server, Config{GitHubStatusSource: GitHubStatusSourceIssueField, Repository: "digitaldrywood/detent", StatusField: "Workflow", StateMap: map[string]string{"Todo": "Ready"}})
			issues := []connector.Issue{{ID: "I_1", State: "Backlog"}}
			err := c.hydrateLaneSignalIssueFieldStatuses(t.Context(), issues)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.next {
				if issues[0].State != "Todo" || issues[0].Fields["Workflow"] != "Ready" || issues[0].PriorityName != "High" {
					t.Fatalf("diagnostic = %#v", issues[0])
				}
				requests := server.requests()
				variables := requests[1]["variables"].(map[string]any)
				if variables["after"] != "next" {
					t.Fatalf("page variables = %#v", variables)
				}
			} else if issues[0].State != "Backlog" {
				t.Fatalf("missing field changed state: %#v", issues[0])
			}
		})
	}
}
