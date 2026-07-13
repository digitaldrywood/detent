package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestCheckDoctorValidatorHealthReportsProductionFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "detent.db")
	backend, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	prNumber := int64(1298)
	retryAt := now.Add(time.Minute)
	if err := backend.RecordValidatorVerdict(ctx, store.ValidatorVerdict{
		ProjectID:       "detent",
		IssueID:         "issue-1298",
		HeadSHA:         "head-1298",
		Identifier:      "digitaldrywood/detent#1298",
		PRNumber:        &prNumber,
		Verdict:         gate.ValidatorVerdictError,
		Summary:         "validator review production attempt 1/3 failed: empty output",
		FailureAttempts: 1,
		NextRetryAt:     &retryAt,
		RecordedAt:      now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("RecordValidatorVerdict() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	check := checkDoctorValidatorHealth(ctx, "detent", path, doctorDeps{openSQLiteReadOnly: openDoctorSQLiteReadOnly}, now.Add(time.Second))
	if check.Status != doctorWarn {
		t.Fatalf("Status = %q, want %q", check.Status, doctorWarn)
	}
	for _, fragment := range []string{"digitaldrywood/detent#1298", "PR #1298", "attempt 1", "empty output"} {
		if !strings.Contains(check.Detail, fragment) {
			t.Fatalf("Detail = %q, want %q", check.Detail, fragment)
		}
	}
	if len(check.ValidatorFailures) != 1 || check.ValidatorFailures[0].NextRetryAt == "" {
		t.Fatalf("ValidatorFailures = %#v", check.ValidatorFailures)
	}
}
