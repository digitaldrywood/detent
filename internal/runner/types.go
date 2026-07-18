package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	FinalStateCompleted             = "completed"
	FinalStateFailed                = "failed"
	FinalStateTokenCeilingExceeded  = "token_ceiling_exceeded"
	FinalStateOperatorStopped       = "operator_stopped"
	TokenCeilingSourceAbsolute      = "max_session_tokens"
	TokenCeilingSourceContextWindow = "max_session_context_multiplier"

	RunModeImplement = "implement"
	RunModePlan      = "plan"
	RunModeMerge     = "merge"
	RunModeRoutine   = "routine"

	RunOutputMergeFastPathClean       = "merge_fast_path_clean"
	RunOutputMergeFastPathCheckedHead = "merge_fast_path_checked_head"
)

var (
	ErrSessionTokenCeilingExceeded = errors.New("session token ceiling exceeded")
	ErrOperatorStopped             = errors.New("operator stopped run")
	ErrAgentTurnCleanup            = errors.New("agent turn cleanup failed")
	ErrAgentResumeUnsupported      = errors.New("agent backend does not support resume verification")
)

type Backend interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

type Validator interface {
	Validate(context.Context, ValidatorRequest) (gate.ValidatorResult, error)
}

type WorkspaceReaper interface {
	ReapWorkspace(context.Context, connector.Issue) (WorkspaceReapResult, error)
}

type DailyBudgetStatusProvider interface {
	DailyBudgetStatus(context.Context, time.Time) (DailyBudgetStatus, bool, error)
}

type IssueBudgetStatusProvider interface {
	IssueBudgetStatus(context.Context, connector.Issue) (IssueBudgetStatus, bool, error)
}

type DailyBudgetStatus struct {
	Active          bool
	CurrentSpendUSD float64
	MaxUSD          float64
}

type IssueBudgetStatus struct {
	Active          bool
	CurrentSpendUSD float64
	MaxUSD          float64
}

type WorkspaceReapResult struct {
	Worktrees int
	Branches  int
	Processes int
}

type AgentBackend interface {
	RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error)
}

type AgentToolBackend interface {
	RunTurnWithTools(context.Context, AgentTurnRequest, []AgentTool, AgentToolHandler, AgentUpdateHandler) (AgentTurnResult, error)
}

type AgentTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type AgentToolCall struct {
	Name      string
	Arguments json.RawMessage
}

type AgentToolResult struct {
	Content string
	Success bool
}

type AgentToolHandler func(context.Context, AgentToolCall) (AgentToolResult, error)

type AgentResumeVerifier interface {
	VerifyResume(context.Context, AgentResume) error
}

type AgentModelCatalogProvider interface {
	ListModels(context.Context) ([]AgentModel, error)
}

type AgentDefaultModelProvider interface {
	DefaultModel(context.Context, string) (string, error)
}

type AgentModel struct {
	ID                        string
	Model                     string
	Default                   bool
	Upgrade                   string
	SupportedReasoningEfforts []string
}

type AgentTurnRequest struct {
	Workspace          string
	TempDir            string
	Prompt             string
	ToolInstructions   string
	ReadOnly           bool
	Model              string
	ModelProvider      string
	ServiceTier        string
	ReasoningEffort    string
	Resume             AgentResume
	TurnTimeout        time.Duration
	ExtraWritableRoots []string
	Environment        procgroup.Environment
	cacheStrategy      string
	projectID          string
}

type AgentResume struct {
	ThreadID  string
	SessionID string
}

type AgentTurnResult struct {
	ThreadID  string
	TurnID    string
	SessionID string
}

type AgentTurnCleanupError struct {
	Err error
}

func NewAgentTurnCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return &AgentTurnCleanupError{Err: err}
}

func (e *AgentTurnCleanupError) Error() string {
	if e == nil || e.Err == nil {
		return ErrAgentTurnCleanup.Error()
	}
	return fmt.Sprintf("%s: %v", ErrAgentTurnCleanup, e.Err)
}

func (e *AgentTurnCleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *AgentTurnCleanupError) Is(target error) bool {
	return target == ErrAgentTurnCleanup
}

type AgentUpdateHandler func(AgentUpdate) error

type AgentUpdateType string

const (
	AgentUpdateProcessStarted   AgentUpdateType = "process_started"
	AgentUpdateProviderIdentity AgentUpdateType = "provider_identity"
	AgentUpdateMessageDelta     AgentUpdateType = "agent_message_delta"
	AgentUpdateTokenUsage       AgentUpdateType = "token_usage"
	AgentUpdateRateLimits       AgentUpdateType = "rate_limits"
	AgentUpdateTurnStarted      AgentUpdateType = "turn_started"
	AgentUpdateTurnCompleted    AgentUpdateType = "turn_completed"
	AgentUpdateModelUpdated     AgentUpdateType = "model_updated"
	AgentUpdateRuntimeIdentity  AgentUpdateType = "runtime_identity"
	AgentUpdateToolStarted      AgentUpdateType = "tool_started"
	AgentUpdateToolOutput       AgentUpdateType = "tool_output"
	AgentUpdateToolCompleted    AgentUpdateType = "tool_completed"
)

type AgentUpdate struct {
	Type                AgentUpdateType
	Method              string
	ProcessIdentity     string
	WorkerProcess       procgroup.Identity
	ThreadID            string
	TurnID              string
	ProviderSessionID   string
	ItemID              string
	Tool                string
	Delta               string
	Status              string
	Model               string
	RuntimeIdentity     agentidentity.Identity
	BackendErrorBody    string
	BackendErrorMessage string
	Tokens              AgentTokenUsage
	RateLimits          *telemetry.RateLimits
}

type AgentTokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    *int64
}

type SessionTokenCeilingError struct {
	TotalTokens        int64
	CeilingTokens      int64
	Source             string
	ModelContextWindow int64
	ContextMultiplier  float64
}

func (e *SessionTokenCeilingError) Error() string {
	source := e.Source
	if source == "" {
		source = "unknown"
	}
	message := fmt.Sprintf("%s: total_tokens=%d ceiling_tokens=%d source=%s", ErrSessionTokenCeilingExceeded, e.TotalTokens, e.CeilingTokens, source)
	if e.ModelContextWindow > 0 {
		message += fmt.Sprintf(" model_context_window=%d", e.ModelContextWindow)
	}
	if e.ContextMultiplier > 0 {
		message += fmt.Sprintf(" context_multiplier=%g", e.ContextMultiplier)
	}
	return message
}

func (e *SessionTokenCeilingError) Unwrap() error {
	return ErrSessionTokenCeilingExceeded
}

type RunRequest struct {
	Issue               connector.Issue
	Attempt             int
	WorkAttemptID       int64
	Mode                string
	DispatchSourceState string
	DispatchTargetState string
	PriorAttempt        PriorAttempt
	StartedAt           time.Time
	WorkerHost          string
	RetryMode           RetryMode
	ResumeState         store.AgentResumeState
	SelectorContext     selector.Context
	OnUsageUpdate       UsageUpdateHandler
	OnActivityUpdate    AgentActivityUpdateHandler
	OnOverrideRejected  AgentOverrideRejectionHandler
	Routine             *RoutineRequest
	AgentTools          []AgentTool
	AgentToolHandler    AgentToolHandler
}

type RoutineRequest struct {
	Name     string
	Schedule string
	Prompt   string
}

type AgentOverrideRejectionHandler func([]AgentOverrideRejection) error

type AgentOverrideRejection struct {
	Field  string
	Value  string
	Reason string
}

type RetryMode string

const (
	RetryModeFresh  RetryMode = "fresh"
	RetryModeResume RetryMode = "resume"
)

type ValidatorRequest struct {
	Issue            connector.Issue
	StartedAt        time.Time
	SelectorContext  selector.Context
	OnUsageUpdate    UsageUpdateHandler
	OnActivityUpdate AgentActivityUpdateHandler
}

type RunResult struct {
	FinalState              string
	Output                  string
	Model                   string
	TurnStarted             bool
	RuntimeIdentity         agentidentity.Identity
	Tokens                  TokenTotals
	DiffStats               DiffStats
	RateLimits              *telemetry.RateLimits
	BudgetRefusal           *BudgetRefusal
	SkillDraftProposed      bool
	PullRequestUpdated      bool
	PullRequestHeadPushed   bool
	CITriggerLabelReapplied bool
}

type UsageUpdateHandler func(UsageUpdate) error

type AgentActivityUpdateHandler func(AgentActivityUpdate) error

type AgentActivityUpdate struct {
	At                time.Time
	DetentSessionID   int64
	ProviderSessionID string
	TurnID            string
	ItemID            string
	Type              AgentUpdateType
	Tool              string
	Content           string
	Status            string
	Model             string
	TotalTokens       int64
}

type UsageUpdate struct {
	DetentSessionID       int64
	SessionID             string
	ProcessIdentity       string
	WorkerProcess         procgroup.Identity
	WorkspacePath         string
	TurnCount             int
	LastEventAt           time.Time
	LastEvent             string
	LastMessage           string
	LastMessageTruncation *runtimeoutput.Truncation
	RecentEvents          []telemetry.ActivityEvent
	RuntimeIdentity       agentidentity.Identity
	Tokens                TokenTotals
	DiffStats             DiffStats
	RateLimits            *telemetry.RateLimits
}

type BudgetRefusal struct {
	Issue            connector.Issue
	Code             string
	Message          string
	Comment          string
	CurrentSpendUSD  float64
	ProjectedCostUSD float64
	MaxUSD           *float64
	ResetAt          *time.Time
	RefusedAt        time.Time
}

type DiffStats struct {
	FilesChanged    int
	AddedLines      int
	RemovedLines    int
	UnpushedCommits int
	Fingerprint     string
	Status          string
}

type TokenTotals struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    *int64
	RuntimeSeconds        float64
}

type PriorAttempt struct {
	Source                  string
	Reason                  string
	ExplainBeforeRetry      bool
	MissingSignal           string
	ObservedSpendUSD        float64
	NoProgressSpendLimitUSD float64
	Validator               gate.ValidatorResult
}

type FakeRunner struct{}

func (FakeRunner) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{FinalState: FinalStateCompleted}, nil
}
