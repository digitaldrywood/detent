package orchestrator

import (
	"github.com/digitaldrywood/detent/internal/runner"
)

const FinalStateCompleted = runner.FinalStateCompleted

var ErrWorkspacePreparation = runner.ErrWorkspacePreparation

const RunModeImplement = runner.RunModeImplement

const RunModePlan = runner.RunModePlan

const RunModeMerge = runner.RunModeMerge

type Runner = runner.Backend

type Validator = runner.Validator

type RunRequest = runner.RunRequest

type ValidatorRequest = runner.ValidatorRequest

type RunResult = runner.RunResult

type BudgetRefusal = runner.BudgetRefusal

type DailyBudgetStatus = runner.DailyBudgetStatus

type DailyBudgetStatusProvider = runner.DailyBudgetStatusProvider

type IssueBudgetStatus = runner.IssueBudgetStatus

type IssueBudgetStatusProvider = runner.IssueBudgetStatusProvider

type DiffStats = runner.DiffStats

type TokenTotals = runner.TokenTotals

type UsageUpdate = runner.UsageUpdate

type FakeRunner = runner.FakeRunner
