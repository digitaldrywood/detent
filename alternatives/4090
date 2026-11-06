package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
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

func (s *sqliteStore) SetBudgetOverride(ctx context.Context, override BudgetOverride) (BudgetOverride, error) {
	projectID := strings.TrimSpace(override.ProjectID)
	if projectID == "" {
		return BudgetOverride{}, errors.New("project_id is required")
	}
	expiresAt, err := requiredTimestamp("expires_at", override.ExpiresAt)
	if err != nil {
		return BudgetOverride{}, err
	}
	createdAt, err := requiredTimestamp("created_at", override.CreatedAt)
	if err != nil {
		return BudgetOverride{}, err
	}
	row, err := s.queries.UpsertBudgetOverride(ctx, sqlc.UpsertBudgetOverrideParams{
		ProjectID:      projectID,
		PerDayMaxUsd:   nullableFloat(override.PerDayMaxUSD),
		PerIssueMaxUsd: nullableFloat(override.PerIssueMaxUSD),
		ExpiresAt:      expiresAt,
		CreatedAt:      createdAt,
		Reason:         strings.TrimSpace(override.Reason),
	})
	if err != nil {
		return BudgetOverride{}, fmt.Errorf("setting budget override: %w", err)
	}
	return budgetOverrideFromRow(row)
}

func (s *sqliteStore) ActiveBudgetOverride(ctx context.Context, projectID string, now time.Time) (BudgetOverride, error) {
	nowText, err := requiredTimestamp("now", now)
	if err != nil {
		return BudgetOverride{}, err
	}
	row, err := s.queries.ActiveBudgetOverride(ctx, sqlc.ActiveBudgetOverrideParams{
		ProjectID: strings.TrimSpace(projectID),
		Now:       nowText,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BudgetOverride{}, ErrNotFound
		}
		return BudgetOverride{}, fmt.Errorf("reading active budget override: %w", err)
	}
	return budgetOverrideFromRow(row)
}

func (s *sqliteStore) ListActiveBudgetOverrides(ctx context.Context, now time.Time) ([]BudgetOverride, error) {
	nowText, err := requiredTimestamp("now", now)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries.ListActiveBudgetOverrides(ctx, nowText)
	if err != nil {
		return nil, fmt.Errorf("listing active budget overrides: %w", err)
	}
	overrides := make([]BudgetOverride, 0, len(rows))
	for _, row := range rows {
		override, err := budgetOverrideFromRow(row)
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, override)
	}
	return overrides, nil
}

func (s *sqliteStore) ClearBudgetOverride(ctx context.Context, projectID string) error {
	rows, err := s.queries.DeleteBudgetOverride(ctx, strings.TrimSpace(projectID))
	if err != nil {
		return fmt.Errorf("clearing budget override: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func budgetOverrideFromRow(row sqlc.BudgetOverride) (BudgetOverride, error) {
	expiresAt, err := parseTimestamp("expires_at", row.ExpiresAt)
	if err != nil {
		return BudgetOverride{}, err
	}
	createdAt, err := parseTimestamp("created_at", row.CreatedAt)
	if err != nil {
		return BudgetOverride{}, err
	}
	return BudgetOverride{
		ProjectID:      row.ProjectID,
		PerDayMaxUSD:   floatPointer(row.PerDayMaxUsd),
		PerIssueMaxUSD: floatPointer(row.PerIssueMaxUsd),
		ExpiresAt:      expiresAt,
		CreatedAt:      createdAt,
		Reason:         row.Reason,
	}, nil
}

func nullableFloat(value *float64) sql.NullFloat64 {
	if value == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *value, Valid: true}
}

func floatPointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	result := value.Float64
	return &result
}
