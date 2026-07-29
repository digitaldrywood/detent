package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
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
		AuthorKind:        "User",
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

func TestConnectorUpdateIssueStateResolvesWorkflowState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		targetState string
		stateMap    map[string]string
		states      []map[string]any
		wantStateID string
	}{
		{
			name:        "happy path",
			targetState: "Started",
			states: []map[string]any{
				linearWorkflowStateFixture("state-started", "Started"),
			},
			wantStateID: "state-started",
		},
		{
			name:        "case insensitive match",
			targetState: "started",
			states: []map[string]any{
				linearWorkflowStateFixture("state-started", "Started"),
			},
			wantStateID: "state-started",
		},
		{
			name:        "state map translation",
			targetState: "IN PROGRESS",
			stateMap: map[string]string{
				"In Progress": "Started",
			},
			states: []map[string]any{
				linearWorkflowStateFixture("state-started", "Started"),
			},
			wantStateID: "state-started",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			requests := []linearGraphQLRequest{}
			server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
				mu.Lock()
				requests = append(requests, request)
				requestNumber := len(requests)
				mu.Unlock()

				switch requestNumber {
				case 1:
					if !strings.Contains(request.Query, "DetentLinearIssueWorkflowStates") {
						t.Fatalf("query = %q, want issue workflow states query", request.Query)
					}
					if request.Variables["issueId"] != "LIN-123" {
						t.Fatalf("issueId variable = %#v, want LIN-123", request.Variables["issueId"])
					}
					return linearWorkflowStatesResponse("team-1", tt.states)
				case 2:
					if !strings.Contains(request.Query, "DetentLinearIssueUpdateState") || !strings.Contains(request.Query, "issueUpdate") {
						t.Fatalf("query = %q, want issue update mutation", request.Query)
					}
					if request.Variables["issueId"] != "LIN-123" {
						t.Fatalf("issueId variable = %#v, want LIN-123", request.Variables["issueId"])
					}
					if request.Variables["stateId"] != tt.wantStateID {
						t.Fatalf("stateId variable = %#v, want %s", request.Variables["stateId"], tt.wantStateID)
					}
					return linearIssueUpdateResponse(true)
				default:
					t.Fatalf("request count = %d, want 2", requestNumber)
					return nil
				}
			})

			c := newLinearTestConnectorWithStateMap(t, server.URL, tt.stateMap)
			if err := c.UpdateIssueState(context.Background(), " LIN-123 ", tt.targetState); err != nil {
				t.Fatalf("UpdateIssueState() error = %v", err)
			}

			mu.Lock()
			gotRequests := len(requests)
			mu.Unlock()
			if gotRequests != 2 {
				t.Fatalf("request count = %d, want 2", gotRequests)
			}
		})
	}
}

func TestConnectorUpdateIssueStateErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		issueID      string
		respond      func(*testing.T, int, linearGraphQLRequest) any
		wantErr      error
		wantRequests int
	}{
		{
			name:    "blank issue id",
			issueID: " \t",
			respond: func(t *testing.T, _ int, _ linearGraphQLRequest) any {
				t.Fatalf("server received request for blank issue id")
				return nil
			},
			wantErr:      ErrMissingIssue,
			wantRequests: 0,
		},
		{
			name:    "null issue",
			issueID: "LIN-123",
			respond: func(t *testing.T, requestNumber int, request linearGraphQLRequest) any {
				if requestNumber != 1 {
					t.Fatalf("request count = %d, want 1", requestNumber)
				}
				if !strings.Contains(request.Query, "DetentLinearIssueWorkflowStates") {
					t.Fatalf("query = %q, want issue workflow states query", request.Query)
				}
				return map[string]any{
					"data": map[string]any{
						"issue": nil,
					},
				}
			},
			wantErr:      ErrIssueNotFound,
			wantRequests: 1,
		},
		{
			name:    "failed mutation",
			issueID: "LIN-123",
			respond: func(t *testing.T, requestNumber int, request linearGraphQLRequest) any {
				switch requestNumber {
				case 1:
					return linearWorkflowStatesResponse("team-1", []map[string]any{
						linearWorkflowStateFixture("state-started", "Started"),
					})
				case 2:
					if !strings.Contains(request.Query, "DetentLinearIssueUpdateState") {
						t.Fatalf("query = %q, want issue update mutation", request.Query)
					}
					return linearIssueUpdateResponse(false)
				default:
					t.Fatalf("request count = %d, want 2", requestNumber)
					return nil
				}
			},
			wantErr:      ErrIssueUpdateFailed,
			wantRequests: 2,
		},
		{
			name:    "graphql errors envelope",
			issueID: "LIN-123",
			respond: func(t *testing.T, requestNumber int, _ linearGraphQLRequest) any {
				if requestNumber != 1 {
					t.Fatalf("request count = %d, want 1", requestNumber)
				}
				return map[string]any{
					"errors": []map[string]any{{
						"message": "linear unavailable",
					}},
				}
			},
			wantErr:      ErrGraphQLErrors,
			wantRequests: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			requests := 0
			server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
				mu.Lock()
				requests++
				requestNumber := requests
				mu.Unlock()

				return tt.respond(t, requestNumber, request)
			})

			c := newLinearTestConnector(t, server.URL)
			err := c.UpdateIssueState(context.Background(), tt.issueID, "Started")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("UpdateIssueState() error = %v, want %v", err, tt.wantErr)
			}

			mu.Lock()
			gotRequests := requests
			mu.Unlock()
			if gotRequests != tt.wantRequests {
				t.Fatalf("request count = %d, want %d", gotRequests, tt.wantRequests)
			}
		})
	}
}

func TestConnectorUpdateIssueStateRefetchesBeforeStateNotFound(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	stateQueries := 0
	mutations := 0
	server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(request.Query, "DetentLinearIssueWorkflowStates") {
			stateQueries++
			return linearWorkflowStatesResponse("team-1", []map[string]any{
				linearWorkflowStateFixture("state-todo", "Todo"),
			})
		}
		if strings.Contains(request.Query, "DetentLinearIssueUpdateState") {
			mutations++
			return linearIssueUpdateResponse(true)
		}

		t.Fatalf("query = %q, want workflow states query or issue update mutation", request.Query)
		return nil
	})

	c := newLinearTestConnector(t, server.URL)
	if err := c.UpdateIssueState(context.Background(), "LIN-123", "Todo"); err != nil {
		t.Fatalf("UpdateIssueState() first error = %v", err)
	}
	err := c.UpdateIssueState(context.Background(), "LIN-123", "Done")
	if err == nil || !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("UpdateIssueState() second error = %v, want ErrStateNotFound", err)
	}
	if !strings.Contains(err.Error(), "Done") {
		t.Fatalf("UpdateIssueState() second error = %v, want state name", err)
	}

	mu.Lock()
	gotStateQueries := stateQueries
	gotMutations := mutations
	mu.Unlock()
	if gotStateQueries != 2 {
		t.Fatalf("state query count = %d, want 2", gotStateQueries)
	}
	if gotMutations != 1 {
		t.Fatalf("mutation count = %d, want 1", gotMutations)
	}
}

func TestConnectorUpdateIssueStateCachesWorkflowStatesByTeam(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	stateQueries := 0
	mutations := 0
	server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(request.Query, "DetentLinearIssueWorkflowStates") {
			stateQueries++
			return linearWorkflowStatesResponse("team-1", []map[string]any{
				linearWorkflowStateFixture("state-todo", "Todo"),
				linearWorkflowStateFixture("state-started", "Started"),
			})
		}
		if strings.Contains(request.Query, "DetentLinearIssueUpdateState") {
			mutations++
			return linearIssueUpdateResponse(true)
		}

		t.Fatalf("query = %q, want workflow states query or issue update mutation", request.Query)
		return nil
	})

	c := newLinearTestConnector(t, server.URL)
	if err := c.UpdateIssueState(context.Background(), "LIN-123", "Todo"); err != nil {
		t.Fatalf("UpdateIssueState() first error = %v", err)
	}
	if err := c.UpdateIssueState(context.Background(), "LIN-123", "Started"); err != nil {
		t.Fatalf("UpdateIssueState() second error = %v", err)
	}

	mu.Lock()
	gotStateQueries := stateQueries
	gotMutations := mutations
	mu.Unlock()
	if gotStateQueries != 1 {
		t.Fatalf("state query count = %d, want 1", gotStateQueries)
	}
	if gotMutations != 2 {
		t.Fatalf("mutation count = %d, want 2", gotMutations)
	}
}

func TestConnectorUpdateIssueStateRefreshesStaleCachedTeam(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	stateQueries := 0
	mutationStateIDs := []string{}
	server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(request.Query, "DetentLinearIssueWorkflowStates") {
			stateQueries++
			switch stateQueries {
			case 1:
				return linearWorkflowStatesResponse("team-1", []map[string]any{
					linearWorkflowStateFixture("state-todo-old", "Todo"),
				})
			case 2:
				return linearWorkflowStatesResponse("team-2", []map[string]any{
					linearWorkflowStateFixture("state-todo-new", "Todo"),
				})
			default:
				t.Fatalf("state query count = %d, want 2", stateQueries)
				return nil
			}
		}
		if strings.Contains(request.Query, "DetentLinearIssueUpdateState") {
			stateID, _ := request.Variables["stateId"].(string)
			mutationStateIDs = append(mutationStateIDs, stateID)
			return linearIssueUpdateResponse(len(mutationStateIDs) != 2)
		}

		t.Fatalf("query = %q, want workflow states query or issue update mutation", request.Query)
		return nil
	})

	c := newLinearTestConnector(t, server.URL)
	if err := c.UpdateIssueState(context.Background(), "LIN-123", "Todo"); err != nil {
		t.Fatalf("UpdateIssueState() first error = %v", err)
	}
	if err := c.UpdateIssueState(context.Background(), "LIN-123", "Todo"); err != nil {
		t.Fatalf("UpdateIssueState() second error = %v", err)
	}

	mu.Lock()
	gotStateQueries := stateQueries
	gotMutationStateIDs := append([]string(nil), mutationStateIDs...)
	mu.Unlock()
	if gotStateQueries != 2 {
		t.Fatalf("state query count = %d, want 2", gotStateQueries)
	}
	wantMutationStateIDs := []string{"state-todo-old", "state-todo-old", "state-todo-new"}
	if !reflect.DeepEqual(gotMutationStateIDs, wantMutationStateIDs) {
		t.Fatalf("mutation state IDs = %#v, want %#v", gotMutationStateIDs, wantMutationStateIDs)
	}
}

func TestConnectorSetAssigneeResolvesUserAndUpdatesIssue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		login          string
		wantLookups    []string
		userResponses  map[string][]map[string]any
		wantAssigneeID string
	}{
		{
			name:        "email match",
			login:       "worker@example.com",
			wantLookups: []string{"email"},
			userResponses: map[string][]map[string]any{
				"email": {
					linearUserFixture("user-email", "Worker Email", "worker", "worker@example.com"),
				},
			},
			wantAssigneeID: "user-email",
		},
		{
			name:        "display name case insensitive match",
			login:       "WORKER ONE",
			wantLookups: []string{"email", "displayName"},
			userResponses: map[string][]map[string]any{
				"email": nil,
				"displayName": {
					linearUserFixture("user-display", "Worker Name", "worker one", "worker-name@example.com"),
				},
			},
			wantAssigneeID: "user-display",
		},
		{
			name:        "name case insensitive match",
			login:       "WORKER NAME",
			wantLookups: []string{"email", "displayName", "name"},
			userResponses: map[string][]map[string]any{
				"email":       nil,
				"displayName": nil,
				"name": {
					linearUserFixture("user-name", "worker name", "worker", "worker-name@example.com"),
				},
			},
			wantAssigneeID: "user-name",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			lookups := []string{}
			mutationAssigneeID := ""
			server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
				if strings.Contains(request.Query, "DetentLinearUserByLogin") {
					lookup := linearUserLookupFilterKind(t, request.Variables["filter"], tt.login)
					mu.Lock()
					lookups = append(lookups, lookup)
					mu.Unlock()
					return linearUsersResponse(tt.userResponses[lookup])
				}
				if strings.Contains(request.Query, "DetentLinearIssueUpdateAssignee") {
					if request.Variables["issueId"] != "LIN-123" {
						t.Fatalf("issueId variable = %#v, want LIN-123", request.Variables["issueId"])
					}
					assigneeID, _ := request.Variables["assigneeId"].(string)
					mu.Lock()
					mutationAssigneeID = assigneeID
					mu.Unlock()
					return linearIssueUpdateResponse(true)
				}

				t.Fatalf("query = %q, want user lookup query or issue update assignee mutation", request.Query)
				return nil
			})

			c := newLinearTestConnector(t, server.URL)
			if err := c.SetAssignee(context.Background(), " LIN-123 ", " "+tt.login+" "); err != nil {
				t.Fatalf("SetAssignee() error = %v", err)
			}

			mu.Lock()
			gotLookups := append([]string(nil), lookups...)
			gotMutationAssigneeID := mutationAssigneeID
			mu.Unlock()
			if !reflect.DeepEqual(gotLookups, tt.wantLookups) {
				t.Fatalf("user lookups = %#v, want %#v", gotLookups, tt.wantLookups)
			}
			if gotMutationAssigneeID != tt.wantAssigneeID {
				t.Fatalf("mutation assignee ID = %q, want %q", gotMutationAssigneeID, tt.wantAssigneeID)
			}
		})
	}
}

func TestConnectorSetAssigneeFindsExactMatchesBeforeLowerPriorityNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		login          string
		wantLookups    []string
		userResponses  map[string][]map[string]any
		wantAssigneeID string
	}{
		{
			name:        "email match",
			login:       "worker@example.com",
			wantLookups: []string{"email"},
			userResponses: map[string][]map[string]any{
				"email": {
					linearUserFixture("user-email", "Worker Email", "worker", "worker@example.com"),
				},
				"or": {
					linearUserFixture("user-name-1", "worker@example.com", "Worker One", "worker1@example.com"),
					linearUserFixture("user-name-2", "WORKER@example.com", "Worker Two", "worker2@example.com"),
				},
			},
			wantAssigneeID: "user-email",
		},
		{
			name:        "display name match",
			login:       "Worker",
			wantLookups: []string{"email", "displayName"},
			userResponses: map[string][]map[string]any{
				"email": nil,
				"displayName": {
					linearUserFixture("user-display", "worker-name", "worker", "worker-name@example.com"),
				},
				"or": {
					linearUserFixture("user-name-1", "worker", "Worker One", "worker1@example.com"),
					linearUserFixture("user-name-2", "WORKER", "Worker Two", "worker2@example.com"),
				},
			},
			wantAssigneeID: "user-display",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			lookups := []string{}
			mutationAssigneeID := ""
			server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
				if strings.Contains(request.Query, "DetentLinearUserByLogin") {
					lookup := linearUserLookupFilterKind(t, request.Variables["filter"], tt.login)
					mu.Lock()
					lookups = append(lookups, lookup)
					mu.Unlock()
					return linearUsersResponse(tt.userResponses[lookup])
				}
				if strings.Contains(request.Query, "DetentLinearIssueUpdateAssignee") {
					assigneeID, _ := request.Variables["assigneeId"].(string)
					mu.Lock()
					mutationAssigneeID = assigneeID
					mu.Unlock()
					return linearIssueUpdateResponse(true)
				}

				t.Fatalf("query = %q, want user lookup query or issue update assignee mutation", request.Query)
				return nil
			})

			c := newLinearTestConnector(t, server.URL)
			if err := c.SetAssignee(context.Background(), "LIN-123", tt.login); err != nil {
				t.Fatalf("SetAssignee() error = %v", err)
			}

			mu.Lock()
			gotLookups := append([]string(nil), lookups...)
			gotMutationAssigneeID := mutationAssigneeID
			mu.Unlock()
			if !reflect.DeepEqual(gotLookups, tt.wantLookups) {
				t.Fatalf("user lookups = %#v, want %#v", gotLookups, tt.wantLookups)
			}
			if gotMutationAssigneeID != tt.wantAssigneeID {
				t.Fatalf("mutation assignee ID = %q, want %q", gotMutationAssigneeID, tt.wantAssigneeID)
			}
		})
	}
}

func TestConnectorSetAssigneeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		issueID       string
		login         string
		respond       func(*testing.T, int, linearGraphQLRequest) any
		wantErr       error
		wantRequests  int
		wantMutations int
	}{
		{
			name:    "blank issue id",
			issueID: " \t",
			login:   "worker",
			respond: func(t *testing.T, _ int, _ linearGraphQLRequest) any {
				t.Fatalf("server received request for blank issue id")
				return nil
			},
			wantErr: ErrMissingIssue,
		},
		{
			name:    "blank assignee",
			issueID: "LIN-123",
			login:   " \t",
			respond: func(t *testing.T, _ int, _ linearGraphQLRequest) any {
				t.Fatalf("server received request for blank assignee")
				return nil
			},
			wantErr: ErrMissingUser,
		},
		{
			name:    "user not found",
			issueID: "LIN-123",
			login:   "unknown",
			respond: func(t *testing.T, requestNumber int, request linearGraphQLRequest) any {
				if requestNumber > 3 {
					t.Fatalf("request count = %d, want at most 3", requestNumber)
				}
				if !strings.Contains(request.Query, "DetentLinearUserByLogin") {
					t.Fatalf("query = %q, want user lookup query", request.Query)
				}
				return linearUsersResponse(nil)
			},
			wantErr:      ErrUserNotFound,
			wantRequests: 3,
		},
		{
			name:    "ambiguous user",
			issueID: "LIN-123",
			login:   "worker",
			respond: func(t *testing.T, requestNumber int, request linearGraphQLRequest) any {
				if !strings.Contains(request.Query, "DetentLinearUserByLogin") {
					t.Fatalf("query = %q, want user lookup query", request.Query)
				}
				lookup := linearUserLookupFilterKind(t, request.Variables["filter"], "worker")
				switch lookup {
				case "email":
					return linearUsersResponse(nil)
				case "displayName":
					return linearUsersResponse([]map[string]any{
						linearUserFixture("user-1", "Worker One", "worker", "worker1@example.com"),
						linearUserFixture("user-2", "Worker Two", "WORKER", "worker2@example.com"),
					})
				default:
					t.Fatalf("request count = %d, lookup = %q, want email then displayName", requestNumber, lookup)
					return nil
				}
			},
			wantErr:      ErrUserAmbiguous,
			wantRequests: 2,
		},
		{
			name:    "failed mutation",
			issueID: "LIN-123",
			login:   "worker",
			respond: func(t *testing.T, requestNumber int, request linearGraphQLRequest) any {
				switch requestNumber {
				case 1:
					if !strings.Contains(request.Query, "DetentLinearUserByLogin") {
						t.Fatalf("query = %q, want user lookup query", request.Query)
					}
					lookup := linearUserLookupFilterKind(t, request.Variables["filter"], "worker")
					if lookup != "email" {
						t.Fatalf("lookup = %q, want email", lookup)
					}
					return linearUsersResponse(nil)
				case 2:
					if !strings.Contains(request.Query, "DetentLinearUserByLogin") {
						t.Fatalf("query = %q, want user lookup query", request.Query)
					}
					lookup := linearUserLookupFilterKind(t, request.Variables["filter"], "worker")
					if lookup != "displayName" {
						t.Fatalf("lookup = %q, want displayName", lookup)
					}
					return linearUsersResponse([]map[string]any{
						linearUserFixture("user-1", "Worker One", "worker", "worker@example.com"),
					})
				case 3:
					if !strings.Contains(request.Query, "DetentLinearIssueUpdateAssignee") {
						t.Fatalf("query = %q, want issue update assignee mutation", request.Query)
					}
					return linearIssueUpdateResponse(false)
				default:
					t.Fatalf("request count = %d, want 3", requestNumber)
					return nil
				}
			},
			wantErr:       ErrIssueUpdateFailed,
			wantRequests:  3,
			wantMutations: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			requests := 0
			mutations := 0
			server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
				mu.Lock()
				requests++
				requestNumber := requests
				if strings.Contains(request.Query, "DetentLinearIssueUpdateAssignee") {
					mutations++
				}
				mu.Unlock()

				return tt.respond(t, requestNumber, request)
			})

			c := newLinearTestConnector(t, server.URL)
			err := c.SetAssignee(context.Background(), tt.issueID, tt.login)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SetAssignee() error = %v, want %v", err, tt.wantErr)
			}

			mu.Lock()
			gotRequests := requests
			gotMutations := mutations
			mu.Unlock()
			if gotRequests != tt.wantRequests {
				t.Fatalf("request count = %d, want %d", gotRequests, tt.wantRequests)
			}
			if gotMutations != tt.wantMutations {
				t.Fatalf("mutation count = %d, want %d", gotMutations, tt.wantMutations)
			}
		})
	}
}

func TestConnectorSetAssigneeCachesResolvedUser(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	lookups := []string{}
	mutationAssigneeIDs := []string{}
	server := linearTestServer(t, func(t *testing.T, request linearGraphQLRequest) any {
		mu.Lock()
		defer mu.Unlock()

		if strings.Contains(request.Query, "DetentLinearUserByLogin") {
			lookup := linearUserLookupFilterKind(t, request.Variables["filter"], "Worker")
			lookups = append(lookups, lookup)
			switch lookup {
			case "email":
				return linearUsersResponse(nil)
			case "displayName":
				return linearUsersResponse([]map[string]any{
					linearUserFixture("user-worker", "Worker Name", "Worker", "worker@example.com"),
				})
			default:
				t.Fatalf("lookup = %q, want email then displayName", lookup)
				return nil
			}
		}
		if strings.Contains(request.Query, "DetentLinearIssueUpdateAssignee") {
			assigneeID, _ := request.Variables["assigneeId"].(string)
			mutationAssigneeIDs = append(mutationAssigneeIDs, assigneeID)
			return linearIssueUpdateResponse(true)
		}

		t.Fatalf("query = %q, want user lookup query or issue update assignee mutation", request.Query)
		return nil
	})

	c := newLinearTestConnector(t, server.URL)
	if err := c.SetAssignee(context.Background(), "LIN-123", "Worker"); err != nil {
		t.Fatalf("SetAssignee() first error = %v", err)
	}
	if err := c.SetAssignee(context.Background(), "LIN-124", "worker"); err != nil {
		t.Fatalf("SetAssignee() second error = %v", err)
	}

	mu.Lock()
	gotLookups := append([]string(nil), lookups...)
	gotMutationAssigneeIDs := append([]string(nil), mutationAssigneeIDs...)
	mu.Unlock()
	wantLookups := []string{"email", "displayName"}
	if !reflect.DeepEqual(gotLookups, wantLookups) {
		t.Fatalf("user lookups = %#v, want %#v", gotLookups, wantLookups)
	}
	wantMutationAssigneeIDs := []string{"user-worker", "user-worker"}
	if !reflect.DeepEqual(gotMutationAssigneeIDs, wantMutationAssigneeIDs) {
		t.Fatalf("mutation assignee IDs = %#v, want %#v", gotMutationAssigneeIDs, wantMutationAssigneeIDs)
	}
}

func TestConnectorDoesNotExposePullRequestCommenter(t *testing.T) {
	t.Parallel()

	c := newLinearTestConnector(t, "https://api.linear.app/graphql")
	if _, ok := any(c).(connector.PullRequestCommenter); ok {
		t.Fatalf("connector = %T, want no connector.PullRequestCommenter implementation", c)
	}
}

func TestConnectorCapabilities(t *testing.T) {
	t.Parallel()

	c := newLinearTestConnector(t, "https://api.linear.app/graphql")
	want := connector.Capabilities{UpdateIssueState: true, SetAssignee: true, CreateComment: true}

	t.Run("reported capabilities are authoritative", func(t *testing.T) {
		t.Parallel()

		got := connector.DetectCapabilities(c)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("DetectCapabilities() = %#v, want %#v", got, want)
		}
		if reported := c.Capabilities(); !reflect.DeepEqual(reported, want) {
			t.Fatalf("Capabilities() = %#v, want %#v", reported, want)
		}
	})

	t.Run("optional interface probes match reported fields", func(t *testing.T) {
		t.Parallel()

		reported := c.Capabilities()
		_, canCreateWorkItems := any(c).(connector.IssueUpserter)
		_, canCloseIssues := any(c).(connector.IssueCloser)
		_, canRemoveFromProject := any(c).(connector.ProjectRemover)
		_, canSetIssueFields := any(c).(connector.IssueFieldSetter)
		_, canClearIssueFields := any(c).(connector.IssueFieldClearer)
		_, canCommentOnPullRequests := any(c).(connector.PullRequestCommenter)
		_, canUpdateComments := any(c).(connector.IssueCommentUpdater)
		_, canDeleteComments := any(c).(connector.IssueCommentDeleter)
		probed := connector.Capabilities{
			CreateWorkItems:       canCreateWorkItems,
			CloseIssues:           canCloseIssues,
			RemoveFromProject:     canRemoveFromProject,
			SetIssueFields:        canSetIssueFields,
			ClearIssueFields:      canClearIssueFields,
			CommentOnPullRequests: canCommentOnPullRequests,
			UpdateComments:        canUpdateComments,
			DeleteComments:        canDeleteComments,
		}
		want := connector.Capabilities{
			CreateWorkItems:       reported.CreateWorkItems,
			CloseIssues:           reported.CloseIssues,
			RemoveFromProject:     reported.RemoveFromProject,
			SetIssueFields:        reported.SetIssueFields,
			ClearIssueFields:      reported.ClearIssueFields,
			CommentOnPullRequests: reported.CommentOnPullRequests,
			UpdateComments:        reported.UpdateComments,
			DeleteComments:        reported.DeleteComments,
		}
		if !reflect.DeepEqual(probed, want) {
			t.Fatalf("optional interface probes = %#v, want %#v", probed, want)
		}
	})
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

	return newLinearTestConnectorWithStateMap(t, endpoint, nil)
}

func newLinearTestConnectorWithStateMap(t *testing.T, endpoint string, stateMap map[string]string) *Connector {
	t.Helper()

	c, err := NewConnector(Config{
		Endpoint: endpoint,
		APIKey:   "lin_api_test",
		StateMap: stateMap,
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

func linearWorkflowStatesResponse(teamID string, states []map[string]any) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"issue": map[string]any{
				"id": "LIN-123",
				"team": map[string]any{
					"id": teamID,
					"states": map[string]any{
						"nodes": states,
					},
				},
			},
		},
	}
}

func linearWorkflowStateFixture(id string, name string) map[string]any {
	return map[string]any{
		"id":   id,
		"name": name,
		"type": "started",
	}
}

func linearUsersResponse(users []map[string]any) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"users": map[string]any{
				"nodes": users,
			},
		},
	}
}

func linearUserFixture(id string, name string, displayName string, email string) map[string]any {
	return map[string]any{
		"id":          id,
		"name":        name,
		"displayName": displayName,
		"email":       email,
	}
}

func linearUserLookupFilterKind(t *testing.T, got any, login string) string {
	t.Helper()

	filters := map[string]any{
		"email":       map[string]any{"email": map[string]any{"eq": login}},
		"displayName": map[string]any{"displayName": map[string]any{"eqIgnoreCase": login}},
		"name":        map[string]any{"name": map[string]any{"eqIgnoreCase": login}},
		"or": map[string]any{
			"or": []any{
				map[string]any{"email": map[string]any{"eq": login}},
				map[string]any{"displayName": map[string]any{"eqIgnoreCase": login}},
				map[string]any{"name": map[string]any{"eqIgnoreCase": login}},
			},
		},
	}
	for name, filter := range filters {
		if reflect.DeepEqual(got, filter) {
			return name
		}
	}
	t.Fatalf("filter variable = %#v, want one of %#v", got, filters)
	return ""
}

func linearIssueUpdateResponse(success bool) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"issueUpdate": map[string]any{
				"success": success,
			},
		},
	}
}
