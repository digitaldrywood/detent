package chat

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
	GitHubIssueURL    string                      `json:"github_issue_url,omitempty"`
	ConversationID    string                      `json:"-"`
	SignOut           *operatortool.SignOutResult `json:"sign_out,omitempty"`
	resultData        json.RawMessage             // Never serialized into browser, logs or audit.
	Work              *operatortool.WorkArguments `json:"work,omitempty"`
	Material          bool                        `json:"material,omitempty"`
	Revision          int64                       `json:"revision,omitempty"`
	CommentID         string                      `json:"comment_id,omitempty"`
	Mutation          mutation.Metadata           `json:"-"`
	ConnectionID      string                      `json:"connection_id"`
	OrganizationID    string                      `json:"organization_id"`
	Client            string                      `json:"client"`
	RequestID         string                      `json:"request_id"`
	Arguments         json.RawMessage             `json:"arguments"`
	Mode              ConnectionMode              `json:"mode"`
	ID                string                      `json:"id"`
	Kind              ActionKind                  `json:"kind"`
	ProjectID         string                      `json:"project_id"`
	IssueID           string                      `json:"issue_id"`
	Identifier        string                      `json:"identifier"`
	ResourceURL       string                      `json:"resource_url,omitempty"`
	CurrentState      string                      `json:"current_state"`
	MaterialChange    bool                        `json:"material_change,omitempty"`
	TargetState       string                      `json:"target_state"`
	Priority          string                      `json:"priority"`
	PriorityRank      int                         `json:"priority_rank"`
	Destination       string                      `json:"destination"`
	Reason            string                      `json:"reason"`
	Title             string                      `json:"title"`
	Description       string                      `json:"description"`
	State             string                      `json:"state"`
	Labels            []string                    `json:"labels,omitempty"`
	Attempt           int                         `json:"attempt"`
	WorkAttemptID     int64                       `json:"work_attempt_id"`
	DetentSessionID   int64                       `json:"detent_session_id"`
	ProviderSessionID string                      `json:"provider_session_id"`
	ScenarioID        string                      `json:"scenario_id"`
	Status            ActionStatus                `json:"status"`
	Result            string                      `json:"result,omitempty"`
	CreatedAt         time.Time                   `json:"created_at"`
	ResolvedAt        *time.Time                  `json:"resolved_at,omitempty"`
}

type Conversation struct {
	RequireConfirmation bool
	PrincipalID         string `json:"-"`
	ApprovalBaseURL     string
	ConnectionID        string
	OrganizationID      string
	Client              string
	Mode                ConnectionMode
	Messages            []Message
	Actions             []Action
	Unavailable         bool
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
	ExecuteAction(context.Context, Action) (ActionExecution, error)
}

// ActionExecution carries the application command's result, including the
// identity of a newly created resource, into the shared action receipt.
type ActionExecution struct {
	SignOut    *operatortool.SignOutResult
	Data       json.RawMessage // Deliberate tool result; retained only in this bound chat session.
	Revision   int64
	CommentID  string
	Message    string
	ResourceID string
	Identifier string
	URL        string
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
