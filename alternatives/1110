package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/intake"
)

func TestConnectorFindIntakeIssueSearchesDurableMarker(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ state, reason string }{{"open", ""}, {"closed", "not_planned"}, {"closed", "completed"}} {
		t.Run(tt.state+tt.reason, func(t *testing.T) {
			marker := "<!-- detent-intake:abc123 -->"
			server := newGraphQLTestServer(t, []graphqlTestResponse{{
				method: http.MethodGet,
				body:   fmt.Sprintf(`{"total_count":1,"items":[{"node_id":"I_42","number":42,"title":"Alert","body":%q,"state":%q,"state_reason":%q,"html_url":"https://github.com/example/repo/issues/42"}]}`, "Alert details\n\n"+marker, tt.state, tt.reason),
			}})
			connector := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})

			issue, found, err := connector.FindIntakeIssue(context.Background(), marker)
			if err != nil {
				t.Fatalf("FindIntakeIssue() error = %v", err)
			}
			if issue.Closed != (tt.state == "closed") {
				t.Fatalf("closed = %t", issue.Closed)
			}
			if !found || issue.ID != "I_42" || issue.Number != 42 {
				t.Fatalf("FindIntakeIssue() = %#v, %t", issue, found)
			}
			requests := server.requests()
			path, err := url.ParseRequestURI(requests[0]["path"].(string))
			if err != nil {
				t.Fatalf("ParseRequestURI() error = %v", err)
			}
			query := path.Query().Get("q")
			if strings.Contains(query, "is:open") || !strings.Contains(query, "repo:example/repo") || !strings.Contains(query, "detent-intake:abc123") {
				t.Fatalf("search query = %q", query)
			}
		})
	}
}

func TestConnectorUpdateIntakeIssuePatchesContentAndAddsLabels(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{method: http.MethodGet, body: `{"node_id":"I_42","number":42,"body":"original body","state":"open"}`},
		{
			method: http.MethodPatch,
			path:   "/repos/example/repo/issues/42",
			body:   `{"node_id":"I_42","number":42,"title":"Updated alert","body":"updated body","state":"open","html_url":"https://github.com/example/repo/issues/42"}`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/example/repo/issues/42/labels",
			body:   `[{"name":"bug"}]`,
		},
	})
	connector := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
	connector.projectCache.SetIssueRef("I_42", issueRef{Owner: "example", Name: "repo", Number: 42})

	issue, err := connector.UpdateIntakeIssue(context.Background(), "I_42", intake.IssueDraft{
		Title:  "Updated alert",
		Body:   "updated body",
		Labels: []string{"bug", "Bug"},
	})
	if err != nil {
		t.Fatalf("UpdateIntakeIssue() error = %v", err)
	}
	if issue.ID != "I_42" || issue.Number != 42 {
		t.Fatalf("issue = %#v", issue)
	}
	requests := server.requests()
	patchBody := requests[1]["body"].(map[string]any)
	if patchBody["title"] != "Updated alert" || patchBody["body"] != "updated body" {
		t.Fatalf("patch body = %#v", patchBody)
	}
	labels := requests[2]["body"].(map[string]any)["labels"].([]any)
	if len(labels) != 1 || labels[0] != "bug" {
		t.Fatalf("labels = %#v, want [bug]", labels)
	}
}

func TestConnectorUpdateIssueBodyPatchesOnlyBody(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{method: http.MethodGet, body: `{"node_id":"I_42","number":42,"body":"original body","state":"open"}`}, {
		method: http.MethodPatch,
		path:   "/repos/example/repo/issues/42",
		body:   `{"node_id":"I_42","number":42,"title":"Existing title","body":"updated body","state":"open","html_url":"https://github.com/example/repo/issues/42"}`,
	}})
	connector := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
	connector.projectCache.SetIssueRef("I_42", issueRef{Owner: "example", Name: "repo", Number: 42})

	if err := connector.UpdateIssueBody(context.Background(), "I_42", "updated body"); err != nil {
		t.Fatalf("UpdateIssueBody() error = %v", err)
	}
	body := server.requests()[1]["body"].(map[string]any)
	if len(body) != 1 || body["body"] != "updated body" {
		t.Fatalf("patch body = %#v, want only updated body", body)
	}
}

func TestConnectorCreateIntakeIssueAddsProjectV2Item(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodPost,
			path:   "/repos/example/repo/issues",
			body:   `{"node_id":"I_2","number":2,"title":"Alert","body":"body","state":"open","html_url":"https://github.com/example/repo/issues/2"}`,
		},
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"addProjectV2ItemById":{"item":{"id":"PVTI_2"}}}}`,
		},
	})
	connector := newGitHubTestConnector(t, server, Config{
		Repository:  "example/repo",
		ProjectSlug: "PVT_1",
	})

	issue, err := connector.CreateIntakeIssue(context.Background(), intake.IssueDraft{Title: "Alert", Body: "body"})
	if err != nil {
		t.Fatalf("CreateIntakeIssue() error = %v", err)
	}
	if issue.ID != "I_2" || issue.Number != 2 {
		t.Fatalf("issue = %#v", issue)
	}
	if itemID, ok := connector.projectCache.GetItemID("PVT_1", "I_2"); !ok || itemID != "PVTI_2" {
		t.Fatalf("project item cache = %q, %t", itemID, ok)
	}
	requests := server.requests()
	if !strings.Contains(requests[1]["query"].(string), "addProjectV2ItemById") {
		t.Fatalf("project mutation = %q", requests[1]["query"])
	}
	variables := requests[1]["variables"].(map[string]any)
	if variables["projectId"] != "PVT_1" || variables["contentId"] != "I_2" {
		t.Fatalf("variables = %#v", variables)
	}
}

func TestConnectorCreateIntakeIssueReturnsPartialIssueWhenProjectAddFails(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodPost,
			path:   "/repos/example/repo/issues",
			body:   `{"node_id":"I_2","number":2,"title":"Alert","body":"body","state":"open","html_url":"https://github.com/example/repo/issues/2"}`,
		},
		{
			method: http.MethodPost,
			path:   "/",
			status: http.StatusInternalServerError,
			body:   `{"message":"project unavailable"}`,
		},
	})
	connector := newGitHubTestConnector(t, server, Config{Repository: "example/repo", ProjectSlug: "PVT_1"})

	issue, err := connector.CreateIntakeIssue(context.Background(), intake.IssueDraft{Title: "Alert", Body: "body"})
	if err == nil {
		t.Fatal("CreateIntakeIssue() error = nil")
	}
	if issue.ID != "I_2" || issue.Number != 2 {
		t.Fatalf("CreateIntakeIssue() issue = %#v", issue)
	}
}

func TestConnectorSetIntakeIssueStateResolvesUncachedProjectItem(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_2","project":{"id":"PVT_1"},"statusValue":{"name":"Todo"}}]}}}}`,
		},
		{
			method: http.MethodPost,
			path:   "/",
			body:   `{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_2"}}}}`,
		},
	})
	connector := newGitHubTestConnector(t, server, Config{
		Repository:  "example/repo",
		ProjectSlug: "PVT_1",
	})
	connector.statusCache.Set("PVT_1", statusMetadata{
		FieldID:         "PVTSSF_1",
		OptionIDsByName: map[string]string{"Backlog": "backlog-option"},
	})

	if err := connector.SetIntakeIssueState(context.Background(), "I_2", "Backlog"); err != nil {
		t.Fatalf("SetIntakeIssueState() error = %v", err)
	}
	requests := server.requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if !strings.Contains(requests[0]["query"].(string), "projectItems") {
		t.Fatalf("project item query = %q", requests[0]["query"])
	}
	resolveVariables := requests[0]["variables"].(map[string]any)
	if resolveVariables["issueId"] != "I_2" {
		t.Fatalf("resolve variables = %#v", resolveVariables)
	}
	updateVariables := requests[1]["variables"].(map[string]any)
	if updateVariables["itemId"] != "PVTI_2" || updateVariables["fieldId"] != "PVTSSF_1" || updateVariables["optionId"] != "backlog-option" {
		t.Fatalf("update variables = %#v", updateVariables)
	}
}

func TestConnectorFindIntakeIssuePrefersOpenDuplicate(t *testing.T) {
	marker := "<!-- detent-intake:duplicate -->"
	item := func(number int, state string) string {
		return fmt.Sprintf(`{"node_id":"I_%d","number":%d,"body":%q,"state":%q}`, number, number, marker, state)
	}
	for _, tt := range []struct {
		name  string
		pages [][]string
		want  int
	}{
		{"closed before open", [][]string{{item(43, "closed"), item(42, "open")}}, 42},
		{"open before closed", [][]string{{item(42, "open"), item(43, "closed")}}, 42},
		{"newest closed last", [][]string{{item(42, "closed"), item(43, "closed")}}, 43},
		{"newest closed first", [][]string{{item(43, "closed"), item(42, "closed")}}, 43},
		{"open on later page", [][]string{{item(43, "closed")}, {item(42, "open")}}, 42},
		{"newest closed on later page", [][]string{{item(42, "closed")}, {item(43, "closed")}}, 43},
		{"no match", [][]string{{`{"node_id":"I_99","number":99,"body":"unrelated","state":"open"}`}}, 0},
		{"invalid open matches", [][]string{{item(42, "closed"), `{"number":99,"body":"<!-- detent-intake:duplicate -->","state":"open"}`, `{"node_id":"PR_99","number":99,"body":"<!-- detent-intake:duplicate -->","state":"open","pull_request":{}}`}}, 42},
	} {
		t.Run(tt.name, func(t *testing.T) {
			responses := make([]graphqlTestResponse, len(tt.pages))
			for i, items := range tt.pages {
				responses[i] = graphqlTestResponse{method: http.MethodGet, body: fmt.Sprintf(`{"total_count":%d,"items":[%s]}`, (len(tt.pages)-1)*intakeIssueSearchPageSize+len(items), strings.Join(items, ","))}
			}
			server := newGraphQLTestServer(t, responses)
			connector := newGitHubTestConnector(t, server, Config{Repository: "example/repo", GitHubStatusSource: GitHubStatusSourceLabel})
			issue, found, err := connector.FindIntakeIssue(t.Context(), marker)
			if err != nil || found != (tt.want != 0) || issue.Number != tt.want {
				t.Fatalf("FindIntakeIssue() = %#v, %t, %v; want number %d", issue, found, err, tt.want)
			}
		})
	}
}
