package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestProjectsV2ParityGateMatchesElixirAdapterFlow(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"__typename":"ProjectV2","statusField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_status","options":[{"id":"OPT_backlog","name":"Backlog","color":"GRAY","description":"Backlog."},{"id":"OPT_ready","name":"Ready","color":"GREEN","description":"Ready."},{"id":"OPT_progress","name":"In Progress","color":"YELLOW","description":"Active."},{"id":"OPT_review","name":"In Review","color":"PURPLE","description":"Review."}]},"priorityField":{"__typename":"ProjectV2SingleSelectField","id":"PVTSSF_priority","options":[{"id":"OPT_medium","name":"Medium","color":"YELLOW","description":"Normal."}]}}}}`,
		},
		{
			body: `{"data":{"updateProjectV2Field":{"projectV2Field":{"options":[{"id":"OPT_backlog","name":"Backlog","color":"GRAY","description":"Backlog."},{"id":"OPT_ready","name":"Ready","color":"GREEN","description":"Ready."},{"id":"OPT_progress","name":"In Progress","color":"YELLOW","description":"Active."},{"name":"Blocked","color":"RED","description":"Cannot continue without human input."},{"id":"OPT_review","name":"In Review","color":"PURPLE","description":"Review."},{"name":"Rework","color":"ORANGE","description":"Changes are requested before review can continue."},{"name":"Merging","color":"PURPLE","description":"Approved work is being integrated."},{"name":"Done","color":"GREEN","description":"Work is complete."}]}}}}`,
		},
		{
			body: `{"data":{"updateProjectV2Field":{"projectV2Field":{"options":[{"name":"Urgent","color":"RED","description":"Needs immediate attention."},{"name":"High","color":"ORANGE","description":"Important work to prioritize soon."},{"id":"OPT_medium","name":"Medium","color":"YELLOW","description":"Normal."},{"name":"Low","color":"BLUE","description":"Can wait behind higher-priority work."},{"name":"No priority","color":"GRAY","description":"Priority has not been set."}]}}}}`,
		},
		{
			body: `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_28","content":{"__typename":"Issue","id":"I_kw28","number":28,"title":"Projects-v2 parity gate","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/28","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"Ready"},"priorityValue":{"name":"Urgent"}},{"id":"PVTI_29","content":{"__typename":"Issue","id":"I_kw29","number":29,"title":"Human review","state":"OPEN","url":"https://github.com/digitaldrywood/detent/issues/29","repository":{"nameWithOwner":"digitaldrywood/detent"}},"statusValue":{"name":"In Review"},"priorityValue":{"name":"No priority"}}]}}}}`,
		},
		{
			method: "GET",
			path:   "/repos/digitaldrywood/detent/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
			body:   `[]`,
		},
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_28","project":{"id":"PVT_throwaway"}}]}}}}`,
		},
		{
			body: `{"data":{"node":{"field":{"id":"PVTSSF_status","options":[{"id":"OPT_backlog","name":"Backlog"},{"id":"OPT_ready","name":"Ready"},{"id":"OPT_progress","name":"In Progress"},{"id":"OPT_review","name":"In Review"},{"id":"OPT_merging","name":"Merging"},{"id":"OPT_rework","name":"Rework"},{"id":"OPT_blocked","name":"Blocked"},{"id":"OPT_done","name":"Done"}]}}}}`,
		},
		{
			body: `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_28"}}}}`,
		},
		{
			method: "POST",
			path:   "/repos/digitaldrywood/detent/issues/28/comments",
			body:   `{"node_id":"IC_workpad"}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug:    "PVT_throwaway",
		ActiveStates:   []string{"Todo", "In Progress", "Merging", "Rework"},
		ObservedStates: []string{"Backlog", "Human Review", "Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
		StateMap: map[string]string{
			"Todo":         "Ready",
			"Human Review": "In Review",
			"Cancelled":    "Done",
		},
		PriorityMap: map[string]*int{
			"Urgent":      intPtr(1),
			"High":        intPtr(2),
			"Medium":      intPtr(3),
			"Low":         intPtr(4),
			"No priority": nil,
		},
	})

	ctx := context.Background()
	if err := c.Provision(ctx); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	issues, err := c.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("FetchCandidateIssues() len = %d, want 1", len(issues))
	}
	if issues[0].ID != "I_kw28" || issues[0].State != "Todo" || issues[0].Identifier != "digitaldrywood/detent#28" {
		t.Fatalf("candidate = %#v, want issue 28 in Todo", issues[0])
	}
	if issues[0].Priority == nil || *issues[0].Priority != 1 {
		t.Fatalf("candidate priority = %v, want rank 1", issues[0].Priority)
	}

	if err := c.UpdateIssueState(ctx, "I_kw28", "Human Review"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := c.CreateComment(ctx, "I_kw28", "## Codex Workpad\n\nReady for review."); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}

	requests := server.requests()
	if len(requests) != 9 {
		t.Fatalf("request count = %d, want 9", len(requests))
	}

	statusInput := graphQLInput(t, requests[1])
	if statusInput["fieldId"] != "PVTSSF_status" {
		t.Fatalf("status fieldId = %v, want PVTSSF_status", statusInput["fieldId"])
	}
	statusOptions := graphQLOptions(t, statusInput)
	if got := optionNames(statusOptions); !reflect.DeepEqual(got, []string{"Backlog", "Ready", "In Progress", "Blocked", "In Review", "Rework", "Merging", "Done"}) {
		t.Fatalf("status option names = %#v", got)
	}

	priorityInput := graphQLInput(t, requests[2])
	if priorityInput["fieldId"] != "PVTSSF_priority" {
		t.Fatalf("priority fieldId = %v, want PVTSSF_priority", priorityInput["fieldId"])
	}
	priorityOptions := graphQLOptions(t, priorityInput)
	if got := optionNames(priorityOptions); !reflect.DeepEqual(got, []string{"Urgent", "High", "Medium", "Low", "No priority"}) {
		t.Fatalf("priority option names = %#v", got)
	}

	fetchVariables := requestVariables(t, requests[3])
	if fetchVariables["projectId"] != "PVT_throwaway" {
		t.Fatalf("fetch projectId = %v, want PVT_throwaway", fetchVariables["projectId"])
	}
	if fetchVariables["query"] != `status:Ready,"In Progress",Merging,Rework` {
		t.Fatalf("fetch query = %v, want actionable statuses", fetchVariables["query"])
	}
	if !strings.Contains(requests[3]["query"].(string), "ProjectV2") {
		t.Fatalf("fetch query = %q, want ProjectV2", requests[3]["query"])
	}

	updateVariables := requestVariables(t, requests[7])
	if updateVariables["projectId"] != "PVT_throwaway" ||
		updateVariables["itemId"] != "PVTI_28" ||
		updateVariables["fieldId"] != "PVTSSF_status" ||
		updateVariables["optionId"] != "OPT_review" {
		t.Fatalf("update variables = %#v, want Status option OPT_review on PVTI_28", updateVariables)
	}
	if !strings.Contains(requests[7]["query"].(string), "updateProjectV2ItemFieldValue") {
		t.Fatalf("update query = %q, want updateProjectV2ItemFieldValue", requests[7]["query"])
	}

	if requests[8]["method"] != "POST" || requests[8]["path"] != "/repos/digitaldrywood/detent/issues/28/comments" {
		t.Fatalf("comment request = %#v, want REST issue comment", requests[8])
	}
	commentBody := requests[8]["body"].(map[string]any)
	if commentBody["body"] != "## Codex Workpad\n\nReady for review." {
		t.Fatalf("comment body = %q", commentBody["body"])
	}
}

func requestVariables(t *testing.T, request map[string]any) map[string]any {
	t.Helper()

	variables, ok := request["variables"].(map[string]any)
	if !ok {
		t.Fatalf("variables = %T, want map[string]any", request["variables"])
	}
	return variables
}
