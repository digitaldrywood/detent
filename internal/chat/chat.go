package chat

import (
	"context"
	"encoding/json"
	"time"
)

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"

	ActionMoveItem    ActionKind = "move_item"
	ActionSetPriority ActionKind = "set_priority"
	ActionStopRun     ActionKind = "stop_run"
	ActionFileIssue   ActionKind = "file_issue"

	ActionPending   ActionStatus = "pending"
	ActionSucceeded ActionStatus = "succeeded"
	ActionFailed    ActionStatus = "failed"
	ActionRejected  ActionStatus = "rejected"
)

type ActionKind string

type ActionStatus string

type Message struct {
	ID      string
	Role    string
	Content string
	At      time.Time
	Error   bool
}

type Action struct {
	ID                string
	Kind              ActionKind
	ProjectID         string
	IssueID           string
	Identifier        string
	CurrentState      string
	TargetState       string
	Priority          string
	PriorityRank      int
	Destination       string
	Reason            string
	Title             string
	Description       string
	State             string
	Labels            []string
	Attempt           int
	WorkAttemptID     int64
	DetentSessionID   int64
	ProviderSessionID string
	ScenarioID        string
	Status            ActionStatus
	Result            string
	CreatedAt         time.Time
	ResolvedAt        *time.Time
}

type Conversation struct {
	Messages    []Message
	Actions     []Action
	Unavailable bool
}

type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type ToolCall struct {
	Name      string
	Arguments json.RawMessage
}

type ToolResult struct {
	Content  string
	Proposal *Action
}

type ToolExecutor interface {
	ExecuteTool(context.Context, ToolCall) (ToolResult, error)
}

type ActionExecutor interface {
	ExecuteAction(context.Context, Action) (string, error)
}

type Provider interface {
	Reply(context.Context, TurnRequest) (TurnResponse, error)
}

type ProviderFunc func(context.Context, TurnRequest) (TurnResponse, error)

func (f ProviderFunc) Reply(ctx context.Context, request TurnRequest) (TurnResponse, error) {
	return f(ctx, request)
}

type TurnRequest struct {
	ThreadID string
	Prompt   string
	Tools    []Tool
	Handle   func(context.Context, ToolCall) (ToolResult, error)
}

type TurnResponse struct {
	ThreadID string
	Content  string
}
