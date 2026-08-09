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
	FinalStateMergeRevoked          = "merge_revoked"
	FinalStateCIUnavailable         = "ci_unavailable"
	FinalStateMergeDurationExceeded = "merge_duration_exceeded"
	TokenCeilingSourceAbsolute      = "max_session_tokens"
	TokenCeilingSourceContextWindow = "max_session_context_multiplier"

	RunModeImplement = "implement"
	RunModePlan      = "plan"
	RunModeMerge     = "merge"
	RunModeRoutine   = "routine"

	RunOutputMergeFastPathClean       = "merge_fast_path_clean"
	RunOutputMergeFastPathCheckedHead = "merge_fast_path_checked_head"
	RunOutputMergeFallbackDeferred    = "merge_fallback_deferred"
)

var (
	ErrSessionTokenCeilingExceeded = errors.New("session token ceiling exceeded")
	ErrTurnDurationExceeded        = errors.New("agent turn duration exceeded")
	ErrSessionDurationExceeded     = errors.New("agent session duration exceeded")
	ErrOperatorStopped             = errors.New("operator stopped run")
	ErrMergeRevoked                = errors.New("merge eligibility revoked")
	ErrCIUnavailable               = errors.New("CI unavailable")
	ErrMergeWorkerStartupTimeout   = errors.New("merge worker startup timed out")
	ErrMergeWorkerDurationExceeded = errors.New("merge worker duration exceeded")
	ErrModelPermitUnavailable      = errors.New("provider model permit unavailable")
	ErrAgentTurnCleanup            = errors.New("agent turn cleanup failed")
	ErrAgentResumeUnsupported      = errors.New("agent backend does not support resume verification")
)

type Backend interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

type BlockedRecoveryInspector interface {
	BlockedRecoverySnapshot(context.Context, RunRequest) BlockedRecoverySnapshot
}

type BlockedRecoverySnapshot struct {
	ConfigFingerprint    string
	ToolingFingerprint   string
	BaseFingerprint      string
	WorkspaceFingerprint string
	WorkspaceStatus      string
	WorkspacePresent     bool
	WorkspaceFiles       int
	UnpushedCommits      int
	Health               string
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
	MaxTurns           int
	TurnTimeout        time.Duration
	MaxDuration        time.Duration
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
	ThreadTotal           *AgentTokenCounts
	Last                  *AgentTokenCounts
	ModelContextWindow    *int64
}

type AgentTokenCounts struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
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

type agentDurationLimitError struct {
	limit    error
	duration time.Duration
}

func (e *agentDurationLimitError) Error() string {
	return fmt.Sprintf("%s after %s", e.limit, e.duration)
}

func (e *agentDurationLimitError) Is(target error) bool {
	return target == e.limit || target == context.DeadlineExceeded
}

type RunRequest struct {
	ProjectID           string
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
	ProgressProbe       SessionProgressProbe
	Routine             *RoutineRequest
	Admission           *AdmissionRequest
	AgentTools          []AgentTool
	AgentToolHandler    AgentToolHandler
	AcquireModelPermit  ModelPermitAcquirer
	MergePrecheck       *MergePrecheck
	sessionBrake        *sessionBrakeController
}

type SessionProgressProbe func(context.Context) (string, error)

type ModelPermitAcquirer func(context.Context) error

type MergePrecheck struct {
	Status      string
	Message     string
	DiffStats   DiffStats
	HeadChanged bool
}

type RoutineRequest struct {
	Name     string
	Schedule string
	Prompt   string
}

type AdmissionRequest struct {
	Schedule        string
	TargetState     string
	CriteriaSection string
	CriteriaText    string
	Dimensions      []AdmissionDimension
	EffortSection   string
	EffortText      string
	AllowedEfforts  []string
	Candidates      []AdmissionCandidate
}

type AdmissionDimension struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type AdmissionCandidate struct {
	ID          string   `json:"id"`
	Identifier  string   `json:"identifier"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	AuthorID    string   `json:"author_id,omitempty"`
	Labels      []string `json:"labels,omitempty"`
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
	MergePrecheck           *MergePrecheck
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
	WorkProductPushed     bool
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
	Last                  *AgentTokenCounts
	ModelContextWindow    *int64
	RuntimeSeconds        float64
}

type PriorAttempt struct {
	Source                  string
	Reason                  string
	ExplainBeforeRetry      bool
	MissingSignal           string
	ObservedTokens          int64
	NoProgressTokenLimit    int64
	ObservedSpendUSD        float64
	NoProgressSpendLimitUSD float64
	Validator               gate.ValidatorResult
}

type FakeRunner struct{}

func (FakeRunner) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{FinalState: FinalStateCompleted}, nil
}
