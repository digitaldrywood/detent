package orchestrator

import (
	"github.com/digitaldrywood/symphony-go/internal/runner"
)

const FinalStateCompleted = runner.FinalStateCompleted

type Runner = runner.Backend

type RunRequest = runner.RunRequest

type RunResult = runner.RunResult

type BudgetRefusal = runner.BudgetRefusal

type DiffStats = runner.DiffStats

type CodexTotals = runner.CodexTotals

type FakeRunner = runner.FakeRunner
