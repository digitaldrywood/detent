package runner

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	FinalStateCompleted = "completed"
	FinalStateFailed    = "failed"
)

type Backend interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

type AgentBackend interface {
	RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error)
}

type AgentTurnRequest struct {
	Workspace         string
	Prompt            string
	ApprovalPolicy    any
	ThreadSandbox     string
	TurnSandboxPolicy any
	Model             string
	ModelProvider     string
	ServiceTier       string
}

type AgentTurnResult struct {
	ThreadID  string
	TurnID    string
	SessionID string
}

type AgentUpdateHandler func(AgentUpdate) error

type AgentUpdateType string

const (
	AgentUpdateProcessStarted AgentUpdateType = "process_started"
	AgentUpdateMessageDelta   AgentUpdateType = "agent_message_delta"
	AgentUpdateTokenUsage     AgentUpdateType = "token_usage"
	AgentUpdateRateLimits     AgentUpdateType = "rate_limits"
	AgentUpdateTurnStarted    AgentUpdateType = "turn_started"
	AgentUpdateTurnCompleted  AgentUpdateType = "turn_completed"
)

type AgentUpdate struct {
	Type            AgentUpdateType
	Method          string
	ProcessIdentity string
	ThreadID        string
	TurnID          string
	ItemID          string
	Delta           string
	Status          string
	Tokens          AgentTokenUsage
	RateLimits      *telemetry.RateLimits
}

type AgentTokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ModelContextWindow    *int64
}

type RunRequest struct {
	Issue           connector.Issue
	Attempt         int
	StartedAt       time.Time
	WorkerHost      string
	SelectorContext selector.Context
	OnUsageUpdate   UsageUpdateHandler
}

type RunResult struct {
	FinalState    string
	Tokens        CodexTotals
	DiffStats     DiffStats
	RateLimits    *telemetry.RateLimits
	BudgetRefusal *BudgetRefusal
}

type UsageUpdateHandler func(UsageUpdate) error

type UsageUpdate struct {
	SessionID       string
	ProcessIdentity string
	TurnCount       int
	LastEventAt     time.Time
	LastEvent       string
	LastMessage     string
	RecentEvents    []telemetry.ActivityEvent
	Tokens          CodexTotals
	DiffStats       DiffStats
	RateLimits      *telemetry.RateLimits
}

type BudgetRefusal struct {
	Issue            connector.Issue
	Code             string
	Message          string
	CurrentSpendUSD  float64
	ProjectedCostUSD float64
	MaxUSD           *float64
	ResetAt          *time.Time
	RefusedAt        time.Time
}

type DiffStats struct {
	FilesChanged int
	AddedLines   int
	RemovedLines int
	Status       string
}

type CodexTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	RuntimeSeconds float64
}

type FakeRunner struct{}

func (FakeRunner) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{FinalState: FinalStateCompleted}, nil
}
