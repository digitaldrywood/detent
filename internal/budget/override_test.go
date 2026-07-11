package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestEffectiveConfig(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	overrideCap := 200.0
	tests := []struct {
		name string
		row  *store.BudgetOverride
		want float64
	}{
		{name: "no override uses base", want: 100},
		{name: "active override applies", row: &store.BudgetOverride{ProjectID: "detent", PerDayMaxUSD: &overrideCap, ExpiresAt: now.Add(time.Hour)}, want: 200},
		{name: "expired override uses base", row: &store.BudgetOverride{ProjectID: "detent", PerDayMaxUSD: &overrideCap, ExpiresAt: now.Add(-time.Second)}, want: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeOverrideStore{row: tt.row}
			got, err := EffectiveConfig(context.Background(), Config{ProjectID: "detent", PerDayMaxUSD: 100, Overrides: fake}, now)
			if err != nil {
				t.Fatalf("EffectiveConfig() error = %v", err)
			}
			if got.PerDayMaxUSD != tt.want {
				t.Fatalf("PerDayMaxUSD = %.2f, want %.2f", got.PerDayMaxUSD, tt.want)
			}
		})
	}
}

func TestCheckerUsesActiveBudgetOverride(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	overrideCap := 2.0
	pricing := PricingTable{"gpt-test": {USDPerInputToken: 0.01}}
	tests := []struct {
		name        string
		override    *store.BudgetOverride
		wantAllowed bool
	}{
		{name: "no override uses base", wantAllowed: false},
		{name: "active override raises cap", override: &store.BudgetOverride{ProjectID: "detent", PerDayMaxUSD: &overrideCap, ExpiresAt: now.Add(time.Hour)}, wantAllowed: true},
		{name: "expired override returns to base", override: &store.BudgetOverride{ProjectID: "detent", PerDayMaxUSD: &overrideCap, ExpiresAt: now.Add(-time.Second)}, wantAllowed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checker := NewChecker(Config{Enabled: true, ProjectID: "detent", PerDayMaxUSD: 1, Overrides: &fakeOverrideStore{row: tt.override}}, &fakeSpendStore{
				daily: store.TokenSpend{ByModel: []store.ModelTokenSpend{{Model: "gpt-test", InputTokens: 95}}},
			}, pricing)
			decision, err := checker.CheckDispatch(context.Background(), DispatchRequest{Model: "gpt-test", Now: now, Estimate: TokenEstimate{InputTokens: 10}})
			if err != nil {
				t.Fatalf("CheckDispatch() error = %v", err)
			}
			if decision.Allowed != tt.wantAllowed {
				t.Fatalf("Allowed = %v, want %v", decision.Allowed, tt.wantAllowed)
			}
		})
	}
}

func TestSetOverrideGuardrails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	validCap := 200.0
	tooHigh := 301.0
	tests := []struct {
		name    string
		req     OverrideRequest
		wantErr string
	}{
		{name: "valid override", req: OverrideRequest{ProjectID: "detent", PerDayMaxUSD: &validCap, Duration: time.Hour, Reason: "release", Now: now}},
		{name: "duration exceeds maximum", req: OverrideRequest{ProjectID: "detent", PerDayMaxUSD: &validCap, Duration: 49 * time.Hour, Reason: "release", Now: now}, wantErr: "exceeds maximum"},
		{name: "multiplier exceeds maximum", req: OverrideRequest{ProjectID: "detent", PerDayMaxUSD: &tooHigh, Duration: time.Hour, Reason: "release", Now: now}, wantErr: "3.00x base cap"},
		{name: "reason required", req: OverrideRequest{ProjectID: "detent", PerDayMaxUSD: &validCap, Duration: time.Hour, Now: now}, wantErr: "reason is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeOverrideStore{}
			got, err := SetOverride(context.Background(), fake, Config{PerDayMaxUSD: 100, PerIssueMaxUSD: 10}, OverrideLimits{}, tt.req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("SetOverride() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetOverride() error = %v", err)
			}
			if got.ProjectID != "detent" || got.ExpiresAt != now.Add(time.Hour) {
				t.Fatalf("SetOverride() = %#v", got)
			}
		})
	}
}

func TestSetOverrideCannotExtendPastOriginalMaximumLifetime(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	now := createdAt.Add(47 * time.Hour)
	cap := 200.0
	fake := &fakeOverrideStore{row: &store.BudgetOverride{
		ProjectID:    "detent",
		PerDayMaxUSD: &cap,
		CreatedAt:    createdAt,
		ExpiresAt:    now.Add(30 * time.Minute),
	}}
	_, err := SetOverride(context.Background(), fake, Config{PerDayMaxUSD: 100}, OverrideLimits{}, OverrideRequest{
		ProjectID:    "detent",
		PerDayMaxUSD: &cap,
		Duration:     2 * time.Hour,
		Reason:       "extend",
		Now:          now,
	})
	if err == nil || !strings.Contains(err.Error(), "maximum lifetime") {
		t.Fatalf("SetOverride() error = %v, want maximum lifetime rejection", err)
	}
}

type fakeOverrideStore struct {
	row *store.BudgetOverride
}

func (f *fakeOverrideStore) SetBudgetOverride(_ context.Context, value store.BudgetOverride) (store.BudgetOverride, error) {
	f.row = &value
	return value, nil
}

func (f *fakeOverrideStore) ActiveBudgetOverride(_ context.Context, _ string, now time.Time) (store.BudgetOverride, error) {
	if f.row == nil || !f.row.ExpiresAt.After(now) {
		return store.BudgetOverride{}, store.ErrNotFound
	}
	return *f.row, nil
}

func (f *fakeOverrideStore) ListActiveBudgetOverrides(_ context.Context, now time.Time) ([]store.BudgetOverride, error) {
	row, err := f.ActiveBudgetOverride(context.Background(), "", now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return []store.BudgetOverride{row}, nil
}

func (f *fakeOverrideStore) ClearBudgetOverride(context.Context, string) error {
	f.row = nil
	return nil
}
