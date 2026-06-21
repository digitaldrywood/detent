package orchestrator

import (
	"github.com/digitaldrywood/detent/internal/runner"
)

const FinalStateCompleted = runner.FinalStateCompleted

type Runner = runner.Backend

type Validator = runner.Validator

type RunRequest = runner.RunRequest

type ValidatorRequest = runner.ValidatorRequest

type RunResult = runner.RunResult

type BudgetRefusal = runner.BudgetRefusal

type DiffStats = runner.DiffStats

type CodexTotals = runner.CodexTotals

type UsageUpdate = runner.UsageUpdate

type FakeRunner = runner.FakeRunner
