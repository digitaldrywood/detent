package runner

import (
	"context"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/telemetry"
)

const (
	FinalStateCompleted = "completed"
	FinalStateFailed    = "failed"
)

type Backend interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

type RunRequest struct {
	Issue         connector.Issue
	Attempt       int
	StartedAt     time.Time
	WorkerHost    string
	OnUsageUpdate UsageUpdateHandler
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
	Tokens     CodexTotals
	TurnCount  int
	RateLimits *telemetry.RateLimits
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
