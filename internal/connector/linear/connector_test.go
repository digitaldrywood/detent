package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorFetchIssueCommentsMapsLinearMetadata(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 7, 6, 12, 15, 0, 0, time.UTC)
	server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
		if !strings.Contains(request.Query, "DetentLinearIssueComments") {
			t.Fatalf("query = %q, want issue comments query", request.Query)
		}
		if request.Variables["issueId"] != "LIN-123" {
			t.Fatalf("issueId variable = %#v, want LIN-123", request.Variables["issueId"])
		}
		if request.Variables["first"] != float64(issueCommentsPageSize) {
			t.Fatalf("first variable = %#v, want %d", request.Variables["first"], issueCommentsPageSize)
		}
		return map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"comments": map[string]any{
						"nodes": []map[string]any{{
							"id":        "comment-1",
							"body":      "Root cause found.",
							"url":       "https://linear.app/acme/issue/LIN-123#comment-1",
							"createdAt": createdAt.Format(time.RFC3339),
							"updatedAt": updatedAt.Format(time.RFC3339),
							"user": map[string]any{
								"id":          "user-1",
								"name":        "Ada Lovelace",
								"displayName": "ada",
							},
						}},
						"pageInfo": map[string]any{
							"hasNextPage": false,
						},
					},
				},
			},
		}
	})

	c := newLinearTestConnector(t, server.URL)
	got, err := c.FetchIssueComments(context.Background(), connector.Issue{Identifier: "LIN-123"})
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}

	want := []connector.IssueComment{{
		ID:                "comment-1",
		Backend:           connector.BackendLinear.String(),
		Body:              "Root cause found.",
		URL:               "https://linear.app/acme/issue/LIN-123#comment-1",
		AuthorLogin:       "ada",
		AuthorDisplayName: "Ada Lovelace",
		CreatedAt:         &createdAt,
		UpdatedAt:         &updatedAt,
		TargetType:        connector.IssueCommentTargetIssue,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueComments() = %#v, want %#v", got, want)
	}
}

func TestConnectorFetchIssueCommentsPaginates(t *testing.T) {
	t.Parallel()

	var cursors []string
	server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
		after, _ := request.Variables["after"].(string)
		cursors = append(cursors, after)
		switch after {
		case "":
			return linearCommentsResponse([]map[string]any{linearCommentFixture("comment-1", "first")}, true, "cursor-1")
		case "cursor-1":
			return linearCommentsResponse([]map[string]any{linearCommentFixture("comment-2", "second")}, false, "")
		default:
			t.Fatalf("after variable = %q, want empty or cursor-1", after)
			return nil
		}
	})

	c := newLinearTestConnector(t, server.URL)
	got, err := c.FetchIssueComments(context.Background(), connector.Issue{ID: "issue-uuid"})
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}
	if bodies := []string{got[0].Body, got[1].Body}; !reflect.DeepEqual(bodies, []string{"first", "second"}) {
		t.Fatalf("comment bodies = %#v, want first and second", bodies)
	}
	if !reflect.DeepEqual(cursors, []string{"", "cursor-1"}) {
		t.Fatalf("pagination cursors = %#v, want empty then cursor-1", cursors)
	}
}

func TestConnectorCreateCommentCallsLinearMutation(t *testing.T) {
	t.Parallel()

	server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
		if !strings.Contains(request.Query, "DetentLinearCreateComment") || !strings.Contains(request.Query, "commentCreate") {
			t.Fatalf("query = %q, want create comment mutation", request.Query)
		}
		if request.Variables["issueId"] != "LIN-123" {
			t.Fatalf("issueId variable = %#v, want LIN-123", request.Variables["issueId"])
		}
		if request.Variables["body"] != "Please verify the Linear path." {
			t.Fatalf("body variable = %#v, want comment body", request.Variables["body"])
		}
		return map[string]any{
			"data": map[string]any{
				"commentCreate": map[string]any{
					"success": true,
					"comment": map[string]any{"id": "comment-created"},
				},
			},
		}
	})

	c := newLinearTestConnector(t, server.URL)
	if err := c.CreateComment(context.Background(), "LIN-123", "Please verify the Linear path."); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
}

func TestConnectorCreateCommentReportsFailedMutation(t *testing.T) {
	t.Parallel()

	server := linearTestServer(t, func(t *testing.T, _ linearGraphQLRequest) any {
		return map[string]any{
			"data": map[string]any{
				"commentCreate": map[string]any{
					"success": false,
					"comment": nil,
				},
			},
		}
	})

	c := newLinearTestConnector(t, server.URL)
	if err := c.CreateComment(context.Background(), "LIN-123", "body"); !errors.Is(err, ErrCommentCreateFailed) {
		t.Fatalf("CreateComment() error = %v, want ErrCommentCreateFailed", err)
	}
}

func TestConnectorDoesNotExposePullRequestCommenter(t *testing.T) {
	t.Parallel()

	c := newLinearTestConnector(t, "https://api.linear.app/graphql")
	if _, ok := any(c).(connector.PullRequestCommenter); ok {
		t.Fatalf("connector = %T, want no connector.PullRequestCommenter implementation", c)
	}
}

func TestClientGraphQLReadsLargeSuccessfulResponse(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("large response ", maxErrorBodyBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "lin_api_test" {
			t.Fatalf("Authorization = %q, want lin_api_test", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"payload": map[string]any{
					"body": body,
				},
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{
		Endpoint: server.URL,
		APIKey:   "lin_api_test",
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	var got struct {
		Payload struct {
			Body string `json:"body"`
		} `json:"payload"`
	}
	if err := client.GraphQL(context.Background(), "query Test { payload { body } }", nil, &got); err != nil {
		t.Fatalf("GraphQL() error = %v", err)
	}
	if got.Payload.Body != body {
		t.Fatalf("body length = %d, want %d", len(got.Payload.Body), len(body))
	}
}

type linearGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func linearTestServer(t *testing.T, handler func(*testing.T, linearGraphQLRequest) any) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "lin_api_test" {
			t.Fatalf("Authorization = %q, want lin_api_test", got)
		}

		var request linearGraphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(handler(t, request)); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newLinearTestConnector(t *testing.T, endpoint string) *Connector {
	t.Helper()

	c, err := NewConnector(Config{
		Endpoint: endpoint,
		APIKey:   "lin_api_test",
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	return c
}

func linearCommentsResponse(nodes []map[string]any, hasNextPage bool, endCursor string) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"issue": map[string]any{
				"comments": map[string]any{
					"nodes": nodes,
					"pageInfo": map[string]any{
						"hasNextPage": hasNextPage,
						"endCursor":   endCursor,
					},
				},
			},
		},
	}
}

func linearCommentFixture(id string, body string) map[string]any {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	return map[string]any{
		"id":        id,
		"body":      body,
		"url":       "https://linear.app/acme/issue/LIN-123#" + id,
		"createdAt": now.Format(time.RFC3339),
		"updatedAt": now.Format(time.RFC3339),
		"botActor": map[string]any{
			"id":              "bot-1",
			"name":            "Detent",
			"userDisplayName": "Detent Bot",
		},
	}
}
