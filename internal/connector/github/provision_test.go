package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/symphony/internal/connector"
)

func TestConnectorEnsureStateOptionsCreatesMissingStatusAndPriorityOptions(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"__typename":"ProjectV2","statusField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_status","options":[{"id":"OPT_todo","name":"Todo","color":"GREEN","description":"Existing todo"}]},"priorityField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_priority","options":[{"id":"OPT_none","name":"No priority","color":"GRAY","description":"Existing none"}]}}}}`,
		},
		{
			body: `{"data":{"updateProjectV2Field":{"projectV2Field":{"options":[{"id":"OPT_todo","name":"Todo","color":"GREEN","description":"Existing todo"},{"id":"OPT_rework","name":"Rework","color":"ORANGE","description":"Changes are requested before review can continue."}]}}}}`,
		},
		{
			body: `{"data":{"updateProjectV2Field":{"projectV2Field":{"options":[{"id":"OPT_none","name":"No priority","color":"GRAY","description":"Existing none"},{"id":"OPT_p0","name":"P0","color":"RED","description":"Symphony priority rank 1."}]}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:    "PVT_1",
		ActiveStates:   []string{"Todo", "Rework"},
		ObservedStates: []string{"Human Review", "Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
		StateMap: map[string]string{
			"Human Review": "Reviewing",
			"Cancelled":    "Done",
		},
		PriorityMap: map[string]*int{
			"P0":          intPtr(1),
			"No priority": nil,
		},
	})

	if err := c.EnsureStateOptions(context.Background()); err != nil {
		t.Fatalf("EnsureStateOptions() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	for _, index := range []int{1, 2} {
		query := requests[index]["query"].(string)
		if !strings.Contains(query, "updateProjectV2Field") {
			t.Fatalf("request %d query = %q, want updateProjectV2Field", index, query)
		}
	}

	statusInput := graphQLInput(t, requests[1])
	if statusInput["fieldId"] != "PVTSSF_status" {
		t.Fatalf("status fieldId = %v, want PVTSSF_status", statusInput["fieldId"])
	}
	statusOptions := graphQLOptions(t, statusInput)
	if got := optionNames(statusOptions); !reflect.DeepEqual(got, []string{"Todo", "Rework", "Reviewing", "Blocked", "Done"}) {
		t.Fatalf("status option names = %#v", got)
	}
	if statusOptions[0]["id"] != "OPT_todo" {
		t.Fatalf("existing status id = %v, want OPT_todo", statusOptions[0]["id"])
	}
	if statusOptions[0]["color"] != "GREEN" {
		t.Fatalf("existing status color = %v, want GREEN", statusOptions[0]["color"])
	}
	if _, ok := statusOptions[1]["id"]; ok {
		t.Fatalf("new status option has id = %v, want no id", statusOptions[1]["id"])
	}

	priorityInput := graphQLInput(t, requests[2])
	if priorityInput["fieldId"] != "PVTSSF_priority" {
		t.Fatalf("priority fieldId = %v, want PVTSSF_priority", priorityInput["fieldId"])
	}
	priorityOptions := graphQLOptions(t, priorityInput)
	if got := optionNames(priorityOptions); !reflect.DeepEqual(got, []string{"No priority", "P0"}) {
		t.Fatalf("priority option names = %#v", got)
	}
	if priorityOptions[0]["id"] != "OPT_none" {
		t.Fatalf("existing priority id = %v, want OPT_none", priorityOptions[0]["id"])
	}
	if priorityOptions[1]["color"] != "RED" {
		t.Fatalf("P0 color = %v, want RED", priorityOptions[1]["color"])
	}
}

func TestConnectorEnsureStateOptionsNoopsWhenOptionsPresent(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"node":{"__typename":"ProjectV2","statusField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_status","options":[{"id":"OPT_todo","name":"Todo","color":"GRAY","description":""},{"id":"OPT_done","name":"Done","color":"GREEN","description":""}]},"priorityField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_priority","options":[{"id":"OPT_high","name":"High","color":"ORANGE","description":""},{"id":"OPT_none","name":"No priority","color":"GRAY","description":""}]}}}}`,
	}})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:    "PVT_1",
		ActiveStates:   []string{"Todo"},
		TerminalStates: []string{"Done"},
		PriorityMap: map[string]*int{
			"High":        intPtr(2),
			"No priority": nil,
		},
	})

	if err := c.EnsureStateOptions(context.Background()); err != nil {
		t.Fatalf("EnsureStateOptions() error = %v", err)
	}

	if got := len(server.requests()); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestConnectorEnsureStateOptionsSkipsMissingPriorityField(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"__typename":"ProjectV2","statusField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_status","options":[]},"priorityField":null}}}`,
		},
		{
			body: `{"data":{"updateProjectV2Field":{"projectV2Field":{"options":[{"id":"OPT_todo","name":"Todo","color":"GRAY","description":"Ready for Symphony dispatch."}]}}}}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
	})

	if err := c.EnsureStateOptions(context.Background()); err != nil {
		t.Fatalf("EnsureStateOptions() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	input := graphQLInput(t, requests[1])
	if input["fieldId"] != "PVTSSF_status" {
		t.Fatalf("fieldId = %v, want PVTSSF_status", input["fieldId"])
	}
}

func TestConnectorEnsureStateOptionsReturnsGraphQLErrorOnWriteFailure(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"__typename":"ProjectV2","statusField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_status","options":[]},"priorityField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_priority","options":[{"id":"OPT_none","name":"No priority","color":"GRAY","description":""}]}}}}`,
		},
		{
			body: `{"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:  "PVT_1",
		ActiveStates: []string{"Todo"},
		PriorityMap: map[string]*int{
			"No priority": nil,
		},
	})

	err := c.EnsureStateOptions(context.Background())
	if err == nil {
		t.Fatal("EnsureStateOptions() error = nil, want error")
	}
	if !errors.Is(err, ErrGraphQLErrors) {
		t.Fatalf("EnsureStateOptions() error = %v, want ErrGraphQLErrors", err)
	}
	if got := len(server.requests()); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}

func TestConnectorImplementsProvisioner(t *testing.T) {
	t.Parallel()

	c, err := NewConnector(Config{APIKey: "token"})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	if _, ok := any(c).(connector.Provisioner); !ok {
		t.Fatalf("connector = %T, want connector.Provisioner", c)
	}
}

func graphQLInput(t *testing.T, request map[string]any) map[string]any {
	t.Helper()

	variables, ok := request["variables"].(map[string]any)
	if !ok {
		t.Fatalf("variables = %T, want map[string]any", request["variables"])
	}
	input, ok := variables["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %T, want map[string]any", variables["input"])
	}
	return input
}

func graphQLOptions(t *testing.T, input map[string]any) []map[string]any {
	t.Helper()

	raw, ok := input["singleSelectOptions"].([]any)
	if !ok {
		t.Fatalf("singleSelectOptions = %T, want []any", input["singleSelectOptions"])
	}
	options := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		option, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("singleSelectOptions item = %T, want map[string]any", item)
		}
		options = append(options, option)
	}
	return options
}

func optionNames(options []map[string]any) []string {
	names := make([]string, 0, len(options))
	for _, option := range options {
		name, _ := option["name"].(string)
		names = append(names, name)
	}
	return names
}
