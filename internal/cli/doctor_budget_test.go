package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestDoctorBudgetOverridesShowsActiveOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "global.yaml")
	backend, err := store.Open(context.Background(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(dir, "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	dayCap := 200.0
	now := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	_, err = backend.(store.BudgetOverrideStore).SetBudgetOverride(context.Background(), store.BudgetOverride{
		ProjectID:    "detent",
		PerDayMaxUSD: &dayCap,
		CreatedAt:    now,
		ExpiresAt:    now.Add(4 * time.Hour),
		Reason:       "release work",
	})
	if err != nil {
		t.Fatalf("SetBudgetOverride() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	check := checkDoctorBudgetOverrides(context.Background(), globalconfig.PathResolution{Path: path}, "detent", now, doctorDeps{})
	if check.Status != doctorWarn || !strings.Contains(check.Detail, "expires in 4h0m0s") || !strings.Contains(check.Detail, "release work") {
		t.Fatalf("checkDoctorBudgetOverrides() = %#v", check)
	}
}
