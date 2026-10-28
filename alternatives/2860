package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBudgetOverridesLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	overrides := backend.(BudgetOverrideStore)
	now := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	dayCap := 150.0

	created, err := overrides.SetBudgetOverride(ctx, BudgetOverride{
		ProjectID:    "detent",
		PerDayMaxUSD: &dayCap,
		CreatedAt:    now,
		ExpiresAt:    now.Add(2 * time.Hour),
		Reason:       "release work",
	})
	if err != nil {
		t.Fatalf("SetBudgetOverride() error = %v", err)
	}
	if created.PerDayMaxUSD == nil || *created.PerDayMaxUSD != dayCap || created.Reason != "release work" {
		t.Fatalf("SetBudgetOverride() = %#v", created)
	}

	active, err := overrides.ActiveBudgetOverride(ctx, "detent", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ActiveBudgetOverride() error = %v", err)
	}
	if active.ProjectID != "detent" || active.ExpiresAt != now.Add(2*time.Hour) {
		t.Fatalf("ActiveBudgetOverride() = %#v", active)
	}

	listed, err := overrides.ListActiveBudgetOverrides(ctx, now)
	if err != nil {
		t.Fatalf("ListActiveBudgetOverrides() error = %v", err)
	}
	if len(listed) != 1 || listed[0].ProjectID != "detent" {
		t.Fatalf("ListActiveBudgetOverrides() = %#v", listed)
	}

	if _, err := overrides.ActiveBudgetOverride(ctx, "detent", now.Add(3*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired ActiveBudgetOverride() error = %v, want ErrNotFound", err)
	}
	if err := overrides.ClearBudgetOverride(ctx, "detent"); err != nil {
		t.Fatalf("ClearBudgetOverride() error = %v", err)
	}
	if _, err := overrides.ActiveBudgetOverride(ctx, "detent", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared ActiveBudgetOverride() error = %v, want ErrNotFound", err)
	}
}
