package store

import (
	"context"
	"time"
)

type BudgetOverride struct {
	ProjectID      string
	PerDayMaxUSD   *float64
	PerIssueMaxUSD *float64
	ExpiresAt      time.Time
	CreatedAt      time.Time
	Reason         string
}

type BudgetOverrideStore interface {
	SetBudgetOverride(context.Context, BudgetOverride) (BudgetOverride, error)
	ActiveBudgetOverride(context.Context, string, time.Time) (BudgetOverride, error)
	ListActiveBudgetOverrides(context.Context, time.Time) ([]BudgetOverride, error)
	ClearBudgetOverride(context.Context, string) error
}
