package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestServicePersistsConversationAndProviderThreadPerSession(t *testing.T) {
	t.Parallel()
	provider := ProviderFunc(func(ctx context.Context, request TurnRequest) (TurnResponse, error) {
		if request.ThreadID != "" && request.ThreadID != "thread-1" {
			t.Fatalf("ThreadID = %q, want thread-1", request.ThreadID)
		}
		result, err := request.Handle(ctx, ToolCall{Name: "board_state", Arguments: json.RawMessage(`{"state":"Blocked"}`)})
		if err != nil {
			return TurnResponse{}, err
		}
		return TurnResponse{ThreadID: "thread-1", Content: "Live answer: " + result.Content}, nil
	})
	tools := &toolExecutorStub{result: ToolResult{Content: `{"blocked":2}`}}
	service := newTestService(provider, tools, &actionExecutorStub{})

	for _, prompt := range []string{"What is blocked?", "Why?"} {
		conversation, err := service.Send(context.Background(), "browser-session", prompt)
		if err != nil {
			t.Fatalf("Send(%q) error = %v", prompt, err)
		}
		if len(conversation.Messages) == 0 {
			t.Fatalf("Send(%q) returned no messages", prompt)
		}
	}

	conversation := service.Conversation("browser-session")
	if len(conversation.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(conversation.Messages))
	}
	if conversation.Messages[0].Content != "What is blocked?" || !strings.Contains(conversation.Messages[3].Content, `"blocked":2`) {
		t.Fatalf("conversation = %#v", conversation.Messages)
	}
	if tools.calls != 2 {
		t.Fatalf("tool calls = %d, want 2", tools.calls)
	}
	for index := range service.sessionLimit - 1 {
		if _, err := service.Send(t.Context(), fmt.Sprintf("other-%d", index), "hello"); err != nil {
			t.Fatal(err)
		}
	}
	for _, lookup := range []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "conversation", run: func(t *testing.T) { service.Conversation("unknown") }},
		{name: "action", run: func(t *testing.T) { service.Action("unknown", "missing") }},
		{name: "confirm", run: func(t *testing.T) {
			if _, err := service.Confirm(t.Context(), "unknown", "missing"); !errors.Is(err, ErrActionNotFound) {
				t.Fatalf("unknown Confirm() error = %v", err)
			}
		}},
		{name: "reject", run: func(t *testing.T) {
			if _, err := service.Reject(t.Context(), "unknown", "missing"); !errors.Is(err, ErrActionNotFound) {
				t.Fatalf("unknown Reject() error = %v", err)
			}
		}},
		{name: "restore", run: func(t *testing.T) {
			if err := service.RestoreConnection(t.Context(), "unknown"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stored restore", run: func(t *testing.T) {
			service.store = &connectionIdentityStore{}
			if err := service.RestoreConnection(t.Context(), "unknown"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			lookup.run(t)
			if len(service.sessions) != service.sessionLimit || service.sessions["unknown"] != nil {
				t.Fatalf("unknown lookup changed sessions: count=%d unknown=%v", len(service.sessions), service.sessions["unknown"])
			}
			if got := service.Conversation("browser-session"); len(got.Messages) != 4 {
				t.Fatalf("lookup evicted existing conversation: %+v", got)
			}
		})
	}
	t.Run("restored session retains recency and expiry", func(t *testing.T) {
		state := SessionState{ID: "restored", Identity: operatortool.Identity{PrincipalID: "operator", OrganizationID: "organization", CredentialID: "credential"}, Client: "restored-client", LastUsedAt: service.now().Add(-5 * time.Minute)}
		service.store = &connectionIdentityStore{state: state}
		if err := service.RestoreConnection(t.Context(), state.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Send(t.Context(), "next-session", "hello"); err != nil {
			t.Fatal(err)
		}
		got := service.Conversation(state.ID)
		if got.ConnectionID != state.ID || got.PrincipalID != state.Identity.PrincipalID || got.Client != state.Client {
			t.Fatalf("new session evicted restored connection: %+v", got)
		}
		service.now = func() time.Time { return state.LastUsedAt.Add(service.sessionTTL + time.Second) }
		if err := service.RestoreConnection(t.Context(), state.ID); err != nil {
			t.Fatal(err)
		}
		if got := service.Conversation(state.ID); got.ConnectionID != "" || service.sessions[state.ID] != nil {
			t.Fatalf("access extended restored lifetime: %+v", got)
		}
	})
}

func TestServiceRequiresConfirmationBeforeExecutingProposal(t *testing.T) {
	t.Parallel()
	provider := ProviderFunc(func(ctx context.Context, request TurnRequest) (TurnResponse, error) {
		result, err := request.Handle(ctx, ToolCall{Name: "propose_move_item", Arguments: json.RawMessage(`{"project_id":"detent"}`)})
		if err != nil {
			return TurnResponse{}, err
		}
		if !strings.Contains(result.Content, `"status":"pending"`) {
			t.Fatalf("proposal tool content = %q", result.Content)
		}
		return TurnResponse{ThreadID: "thread-move", Content: "Confirm the move below."}, nil
	})
	tools := &toolExecutorStub{result: ToolResult{Proposal: &Action{Kind: ActionMoveItem, ProjectID: "detent", IssueID: "issue-1362", Identifier: "digitaldrywood/detent#1362", CurrentState: "Backlog", TargetState: "Todo"}}}
	actions := &actionExecutorStub{result: "Moved digitaldrywood/detent#1362 to Todo via chat."}
	service := newTestService(provider, tools, actions)

	conversation, err := service.Send(context.Background(), "browser-session", "Move issue 1362 to Todo")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if actions.calls != 0 {
		t.Fatalf("action calls before confirmation = %d, want 0", actions.calls)
	}
	if len(conversation.Actions) != 1 || conversation.Actions[0].Status != ActionPending {
		t.Fatalf("actions = %#v, want one pending action", conversation.Actions)
	}
	stored, ok := service.Action("browser-session", conversation.Actions[0].ID)
	if !ok || stored.ProjectID != "detent" || stored.Status != ActionPending {
		t.Fatalf("Action() = (%#v, %t)", stored, ok)
	}
	if _, ok := service.Action("browser-session", "missing"); ok {
		t.Fatal("Action(missing) ok = true")
	}

	conversation, err = service.Confirm(context.Background(), "browser-session", conversation.Actions[0].ID)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if actions.calls != 1 {
		t.Fatalf("action calls after confirmation = %d, want 1", actions.calls)
	}
	if conversation.Actions[0].Status != ActionSucceeded || !strings.Contains(conversation.Messages[len(conversation.Messages)-1].Content, "via chat") {
		t.Fatalf("confirmed conversation = %#v", conversation)
	}
	if _, err := service.Confirm(context.Background(), "browser-session", conversation.Actions[0].ID); !errors.Is(err, ErrActionNotPending) {
		t.Fatalf("second Confirm() error = %v, want ErrActionNotPending", err)
	}
}

func TestServiceProviderFailureLeavesActionsEmpty(t *testing.T) {
	t.Parallel()
	service := newTestService(ProviderFunc(func(context.Context, TurnRequest) (TurnResponse, error) {
		return TurnResponse{}, errors.New("provider offline")
	}), &toolExecutorStub{}, &actionExecutorStub{})

	conversation, err := service.Send(context.Background(), "browser-session", "Move everything")
	if err == nil {
		t.Fatal("Send() error = nil, want provider failure")
	}
	if len(conversation.Actions) != 0 {
		t.Fatalf("actions = %#v, want none", conversation.Actions)
	}
	if len(conversation.Messages) != 2 || !conversation.Messages[1].Error || !strings.Contains(conversation.Messages[1].Content, "board was not changed") {
		t.Fatalf("messages = %#v", conversation.Messages)
	}
}

func TestServiceRejectsPendingProposalWithoutExecutingIt(t *testing.T) {
	t.Parallel()
	provider := ProviderFunc(func(ctx context.Context, request TurnRequest) (TurnResponse, error) {
		_, err := request.Handle(ctx, ToolCall{Name: "propose_file_issue", Arguments: json.RawMessage(`{}`)})
		return TurnResponse{Content: "Confirm the issue below."}, err
	})
	actions := &actionExecutorStub{}
	service := newTestService(provider, &toolExecutorStub{result: ToolResult{Proposal: &Action{Kind: ActionFileIssue, ProjectID: "detent", Title: "Follow-up"}}}, actions)

	conversation, err := service.Send(context.Background(), "browser-session", "File a follow-up")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	conversation, err = service.Reject(t.Context(), "browser-session", conversation.Actions[0].ID)
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if actions.calls != 0 || conversation.Actions[0].Status != ActionRejected || conversation.Actions[0].Result != "Cancelled by the operator." {
		t.Fatalf("rejected conversation = %#v; action calls = %d", conversation, actions.calls)
	}
	if _, err := service.Reject(t.Context(), "browser-session", conversation.Actions[0].ID); !errors.Is(err, ErrActionNotPending) {
		t.Fatalf("second Reject() error = %v, want ErrActionNotPending", err)
	}
	if _, err := service.Reject(t.Context(), "browser-session", "missing"); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("missing Reject() error = %v, want ErrActionNotFound", err)
	}
}

func TestServiceUnavailableProviderAndFailedActionStaySafe(t *testing.T) {
	t.Parallel()
	unavailable := newTestService(nil, &toolExecutorStub{}, &actionExecutorStub{})
	conversation, err := unavailable.Send(context.Background(), "browser-session", "Move an item")
	if !errors.Is(err, ErrUnavailable) || !conversation.Unavailable || len(conversation.Messages) != 2 || !conversation.Messages[1].Error {
		t.Fatalf("unavailable Send() = (%#v, %v)", conversation, err)
	}

	provider := ProviderFunc(func(ctx context.Context, request TurnRequest) (TurnResponse, error) {
		_, err := request.Handle(ctx, ToolCall{Name: "propose_move_item", Arguments: json.RawMessage(`{}`)})
		return TurnResponse{Content: "Confirm the move."}, err
	})
	service := newTestService(provider, &toolExecutorStub{result: ToolResult{Proposal: &Action{Kind: ActionMoveItem}}}, &actionExecutorStub{result: "Tracker rejected the move.", err: errors.New("tracker offline")})
	conversation, err = service.Send(context.Background(), "browser-session", "Move an item")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	conversation, err = service.Confirm(context.Background(), "browser-session", conversation.Actions[0].ID)
	if err == nil || conversation.Actions[0].Status != ActionFailed || !conversation.Messages[len(conversation.Messages)-1].Error {
		t.Fatalf("failed Confirm() = (%#v, %v)", conversation, err)
	}
}

func TestServiceValidatesMessages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		message string
		wantErr error
	}{
		{name: "empty", message: "  ", wantErr: ErrEmptyMessage},
		{name: "too long", message: strings.Repeat("x", maxMessageLength+1), wantErr: ErrMessageTooLong},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(ProviderFunc(func(context.Context, TurnRequest) (TurnResponse, error) {
				return TurnResponse{ThreadID: "thread", Content: "ok"}, nil
			}), &toolExecutorStub{}, &actionExecutorStub{})
			if _, err := service.Send(context.Background(), "session", test.message); !errors.Is(err, test.wantErr) {
				t.Fatalf("Send() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func newTestService(provider Provider, tools ToolExecutor, actions ActionExecutor) *Service {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	sequence := 0
	return NewService(provider, tools, actions,
		WithClock(func() time.Time {
			sequence++
			return now.Add(time.Duration(sequence) * time.Second)
		}),
		WithIDGenerator(func() (string, error) {
			sequence++
			return fmt.Sprintf("id-%d", sequence), nil
		}),
	)
}

type toolExecutorStub struct {
	result ToolResult
	err    error
	calls  int
}

func (s *toolExecutorStub) ExecuteTool(context.Context, ToolCall) (ToolResult, error) {
	s.calls++
	return s.result, s.err
}

type actionExecutorStub struct {
	result    string
	execution *ActionExecution
	err       error
	calls     int
	action    Action
}

func (s *actionExecutorStub) ExecuteAction(_ context.Context, action Action) (ActionExecution, error) {
	s.calls++
	s.action = action
	if s.execution != nil {
		return *s.execution, s.err
	}
	return ActionExecution{Message: s.result}, s.err
}
