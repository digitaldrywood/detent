package github

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorIssueFieldFetchIssuesByStates(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
			body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":1,"name":"Ready","color":"green"},{"id":3,"name":"Working","color":"yellow"}]},{"id":11,"node_id":"IFSS_priority","name":"Priority","data_type":"single_select","options":[{"id":2,"name":"High","color":"red"}]},{"id":12,"node_id":"IFT_owner","name":"Owner","data_type":"text"}]`,
		},
		{
			method: http.MethodGet,
			body:   `{"total_count":2,"items":[{"node_id":"I_1","number":1,"title":"Ready issue","body":"Depends on: #42","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1","user":{"login":"author-1"},"assignees":[{"node_id":"U_1","login":"worker-1"}],"labels":[{"name":"enhancement"}]},{"node_id":"I_2","number":2,"title":"Active issue","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/2","assignees":[],"labels":[]}]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values?per_page=100",
			body:   `[{"issue_field_id":10,"node_id":"IFV_1","data_type":"single_select","value":1,"single_select_option":{"id":1,"name":"Ready","color":"green"}},{"issue_field_id":11,"node_id":"IFV_2","data_type":"single_select","value":2,"single_select_option":{"id":2,"name":"High","color":"red"}},{"issue_field_id":12,"node_id":"IFV_3","data_type":"text","value":"worker-a"}]`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/2/issue-field-values?per_page=100",
			body:   `[{"issue_field_id":10,"node_id":"IFV_4","data_type":"single_select","value":3,"single_select_option":{"id":3,"name":"Working","color":"yellow"}}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
		StateMap: map[string]string{
			"Todo":        "Ready",
			"In Progress": "Working",
		},
		PriorityMap: map[string]*int{"High": new(1)},
	})

	got, err := c.FetchIssuesByStates(context.Background(), []string{"Todo", "In Progress"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if ids := githubIssueIDs(got); !reflect.DeepEqual(ids, []string{"I_1", "I_2"}) {
		t.Fatalf("FetchIssuesByStates() ids = %#v, want [I_1 I_2]", ids)
	}
	if got[0].State != "Todo" || got[1].State != "In Progress" {
		t.Fatalf("states = %q/%q, want Todo/In Progress", got[0].State, got[1].State)
	}
	if got[0].Priority == nil || *got[0].Priority != 1 {
		t.Fatalf("Priority = %v, want 1", got[0].Priority)
	}
	wantFields := map[string]string{"Owner": "worker-a", "Priority": "High", "Status": "Ready"}
	if !reflect.DeepEqual(got[0].Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", got[0].Fields, wantFields)
	}
	if got[0].AuthorID != "author-1" || got[0].AssigneeID != "worker-1" {
		t.Fatalf("identity fields = author %q assignee %q", got[0].AuthorID, got[0].AssigneeID)
	}
	if !reflect.DeepEqual(got[0].BlockedBy, []connector.BlockedRef{{Identifier: "digitaldrywood/detent#42"}}) {
		t.Fatalf("BlockedBy = %#v", got[0].BlockedBy)
	}

	requests := server.requests()
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want metadata, search, and two field reads", len(requests))
	}
	searchPath := requests[1]["path"].(string)
	for _, want := range []string{"repo%3Adigitaldrywood%2Fdetent", "is%3Aissue", "field.Status%3AReady%2CWorking"} {
		if !strings.Contains(searchPath, want) {
			t.Fatalf("search path = %q, missing %q", searchPath, want)
		}
	}
}

func TestConnectorIssueFieldUpdateIssueState(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values?per_page=100",
			body:   `[{"issue_field_id":10,"node_id":"IFV_1","data_type":"single_select","single_select_option":{"id":1,"name":"Ready","color":"green"}}]`,
		},
		{
			method: http.MethodGet,
			path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
			body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":1,"name":"Ready","color":"green"},{"id":2,"name":"Working","color":"yellow"}]}]`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values",
			body:   `[{"issue_field_id":10,"node_id":"IFV_1","data_type":"single_select","single_select_option":{"id":2,"name":"Working","color":"yellow"}}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
		StateMap: map[string]string{
			"Todo":        "Ready",
			"In Progress": "Working",
		},
	})
	c.projectCache.SetIssueRef("I_1", issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1})

	if err := c.UpdateIssueState(context.Background(), "I_1", "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want field read, metadata read, write", len(requests))
	}
	body := requests[2]["body"].(map[string]any)
	values, ok := body["issue_field_values"].([]any)
	if !ok || len(values) != 1 {
		t.Fatalf("issue_field_values = %#v, want one value", body["issue_field_values"])
	}
	value := values[0].(map[string]any)
	if value["field_id"] != float64(10) || value["value"] != "Working" {
		t.Fatalf("write body value = %#v, want Status field set to Working", value)
	}
}

func TestConnectorIssueFieldRemoveIssueFromProjectClearsStatusField(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
			body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":1,"name":"Ready","color":"green"},{"id":2,"name":"Working","color":"yellow"}]}]`,
		},
		{
			method: http.MethodDelete,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values/10",
			status: http.StatusNoContent,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
	})
	c.projectCache.SetIssueRef("I_1", issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1})

	if err := c.RemoveIssueFromProject(context.Background(), "I_1"); err != nil {
		t.Fatalf("RemoveIssueFromProject() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want metadata read and delete", len(requests))
	}
	if requests[1]["method"] != http.MethodDelete {
		t.Fatalf("remove method = %v, want DELETE", requests[1]["method"])
	}
}

func TestConnectorIssueFieldFetchIssueStatesByIDsCapturesClosedIssue(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/1",
			body:   `{"node_id":"I_1","number":1,"title":"Closed but active","body":"","state":"closed","state_reason":"completed","html_url":"https://github.com/digitaldrywood/detent/issues/1","assignees":[],"labels":[]}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values?per_page=100",
			body:   `[{"issue_field_id":10,"node_id":"IFV_1","data_type":"single_select","single_select_option":{"id":2,"name":"Working","color":"yellow"}}]`,
		},
		{
			method: http.MethodGet,
			path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
			body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":2,"name":"Working","color":"yellow"}]}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
		StateMap:           map[string]string{"In Progress": "Working"},
	})
	c.projectCache.SetIssueRef("I_1", issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1})

	got, err := c.FetchIssueStatesByIDs(context.Background(), []string{"I_1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchIssueStatesByIDs() len = %d, want 1", len(got))
	}
	if !got[0].Closed || got[0].ClosedReason != "completed" || got[0].State != "In Progress" {
		t.Fatalf("issue = %#v, want closed issue with non-terminal Status field state", got[0])
	}
}

func TestConnectorIssueFieldMissingStatusField(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodGet,
		path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
		body:   `[{"id":11,"node_id":"IFSS_priority","name":"Priority","data_type":"single_select","options":[{"id":1,"name":"High","color":"red"}]}]`,
	}})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
	})

	_, err := c.FetchIssuesByStates(context.Background(), []string{"Todo"})
	if !errors.Is(err, ErrStatusFieldNotFound) {
		t.Fatalf("FetchIssuesByStates() error = %v, want ErrStatusFieldNotFound", err)
	}
}

func TestConnectorIssueFieldMissingStatusOption(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodGet,
		path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
		body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":1,"name":"Ready","color":"green"}]}]`,
	}})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
		StateMap:           map[string]string{"Todo": "Ready", "Rework": "Needs changes"},
	})

	if err := c.VerifyStatusOptions(context.Background(), []string{"Rework"}); !errors.Is(err, ErrStatusOptionNotFound) {
		t.Fatalf("VerifyStatusOptions() error = %v, want ErrStatusOptionNotFound", err)
	}
}

func TestConnectorIssueFieldRateLimit(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method:  http.MethodGet,
		path:    "/orgs/digitaldrywood/issue-fields?per_page=100",
		status:  http.StatusForbidden,
		headers: map[string]string{"Retry-After": "1"},
		body:    `{"message":"secondary rate limit"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
	})

	if err := c.VerifyStatusOptions(context.Background(), []string{"Todo"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("VerifyStatusOptions() error = %v, want ErrRateLimited", err)
	}
}
