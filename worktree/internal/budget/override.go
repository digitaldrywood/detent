package budget

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

const (
	DefaultOverrideMaxDuration   = 48 * time.Hour
	DefaultOverrideMaxMultiplier = 3.0
)

type OverrideStore interface {
	ActiveBudgetOverride(context.Context, string, time.Time) (store.BudgetOverride, error)
}

type OverrideWriter interface {
	OverrideStore
	SetBudgetOverride(context.Context, store.BudgetOverride) (store.BudgetOverride, error)
	ListActiveBudgetOverrides(context.Context, time.Time) ([]store.BudgetOverride, error)
	ClearBudgetOverride(context.Context, string) error
}

type OverrideLimits struct {
	MaxDuration   time.Duration
	MaxMultiplier float64
}

type OverrideRequest struct {
	ProjectID      string
	PerDayMaxUSD   *float64
	PerIssueMaxUSD *float64
	Duration       time.Duration
	Reason         string
	Now            time.Time
}

func EffectiveConfig(ctx context.Context, base Config, now time.Time) (Config, error) {
	if base.Overrides == nil || strings.TrimSpace(base.ProjectID) == "" {
		return base, nil
	}
	override, err := base.Overrides.ActiveBudgetOverride(ctx, base.ProjectID, now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return base, nil
		}
		return Config{}, fmt.Errorf("resolve budget override: %w", err)
	}
	if override.PerDayMaxUSD != nil {
		base.PerDayMaxUSD = *override.PerDayMaxUSD
	}
	if override.PerIssueMaxUSD != nil {
		base.PerIssueMaxUSD = *override.PerIssueMaxUSD
	}
	return base, nil
}

func SetOverride(ctx context.Context, writer OverrideWriter, base Config, limits OverrideLimits, req OverrideRequest) (store.BudgetOverride, error) {
	if writer == nil {
		return store.BudgetOverride{}, errors.New("budget override store is required")
	}
	projectID := strings.TrimSpace(req.ProjectID)
	if projectID == "" {
		return store.BudgetOverride{}, errors.New("project is required")
	}
	if !base.Enabled {
		return store.BudgetOverride{}, errors.New("budget overrides require budget enforcement to be enabled")
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return store.BudgetOverride{}, errors.New("reason is required")
	}
	if req.PerDayMaxUSD == nil && req.PerIssueMaxUSD == nil {
		return store.BudgetOverride{}, errors.New("at least one budget cap is required")
	}
	if req.Duration <= 0 {
		return store.BudgetOverride{}, errors.New("duration must be positive")
	}
	limits = normalizedOverrideLimits(limits)
	if req.Duration > limits.MaxDuration {
		return store.BudgetOverride{}, fmt.Errorf("duration %s exceeds maximum %s", req.Duration, limits.MaxDuration)
	}
	if err := validateOverrideCap("per-day-max-usd", req.PerDayMaxUSD, base.PerDayMaxUSD, limits.MaxMultiplier); err != nil {
		return store.BudgetOverride{}, err
	}
	if err := validateOverrideCap("per-issue-max-usd", req.PerIssueMaxUSD, base.PerIssueMaxUSD, limits.MaxMultiplier); err != nil {
		return store.BudgetOverride{}, err
	}
	now := req.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	createdAt := now
	if active, err := writer.ActiveBudgetOverride(ctx, projectID, now); err == nil {
		createdAt = active.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		if now.Add(req.Duration).After(createdAt.Add(limits.MaxDuration)) {
			return store.BudgetOverride{}, fmt.Errorf("override extension exceeds maximum lifetime %s from original creation", limits.MaxDuration)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.BudgetOverride{}, fmt.Errorf("read current budget override: %w", err)
	}
	return writer.SetBudgetOverride(ctx, store.BudgetOverride{
		ProjectID:      projectID,
		PerDayMaxUSD:   req.PerDayMaxUSD,
		PerIssueMaxUSD: req.PerIssueMaxUSD,
		CreatedAt:      createdAt,
		ExpiresAt:      now.Add(req.Duration),
		Reason:         reason,
	})
}

func normalizedOverrideLimits(limits OverrideLimits) OverrideLimits {
	if limits.MaxDuration <= 0 {
		limits.MaxDuration = DefaultOverrideMaxDuration
	}
	if limits.MaxMultiplier <= 0 {
		limits.MaxMultiplier = DefaultOverrideMaxMultiplier
	}
	return limits
}

func validateOverrideCap(name string, value *float64, base float64, multiplier float64) error {
	if value == nil {
		return nil
	}
	if *value <= 0 {
		return fmt.Errorf("%s must be positive", name)
	}
	if base <= 0 {
		return fmt.Errorf("%s cannot override a disabled base cap", name)
	}
	maximum := base * multiplier
	if *value > maximum {
		return fmt.Errorf("%s %.2f exceeds maximum %.2f (%.2fx base cap)", name, *value, maximum, multiplier)
	}
	return nil
}
