package orchestrator

import (
	"context"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/telemetry"
)

const FinalStateCompleted = "completed"

type Runner interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

type RunRequest struct {
	Issue      connector.Issue
	Attempt    int
	StartedAt  time.Time
	WorkerHost string
}

type RunResult struct {
	FinalState    string
	Tokens        CodexTotals
	DiffStats     DiffStats
	RateLimits    *telemetry.RateLimits
	BudgetRefusal *BudgetRefusal
}

type FakeRunner struct{}

func (FakeRunner) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{FinalState: FinalStateCompleted}, nil
}
