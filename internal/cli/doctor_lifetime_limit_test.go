package cli

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestDoctorLifetimeLimitsCompareConfiguredCapsToProjectP95(t *testing.T) {
	t.Parallel()

	path := createDoctorLifetimeHistoryStore(t)
	tests := []struct {
		name         string
		sessionLimit int64
		tokenLimit   int64
		wantStatus   doctorStatus
		wantDetail   []string
	}{
		{name: "caps above p95", sessionLimit: 120, tokenLimit: 750_000_000, wantStatus: doctorOK, wantDetail: []string{"20 completed issues", "sessions 60", "tokens 200000000"}},
		{name: "cap equal to p95", sessionLimit: 60, tokenLimit: 200_000_000, wantStatus: doctorOK},
		{name: "session cap below p95", sessionLimit: 59, tokenLimit: 750_000_000, wantStatus: doctorWarn, wantDetail: []string{"lifetime_session_limit 59", "project p95 60"}},
		{name: "token cap below p95", sessionLimit: 120, tokenLimit: 199_999_999, wantStatus: doctorWarn, wantDetail: []string{"lifetime_token_limit 199999999", "project p95 200000000"}},
		{name: "disabled caps", wantStatus: doctorOK, wantDetail: []string{"disabled"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := workflowconfig.Config{}
			cfg.Agent.LifetimeSessionLimit = test.sessionLimit
			cfg.Agent.LifetimeTokenLimit = test.tokenLimit
			check := checkDoctorLifetimeLimits(t.Context(), "detent", path, cfg, doctorDeps{})
			if check.Status != test.wantStatus {
				t.Fatalf("Status = %s, want %s: %#v", check.Status, test.wantStatus, check)
			}
			for _, fragment := range test.wantDetail {
				if !containsAll(check.Detail, fragment) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, fragment)
				}
			}
		})
	}
}

func createDoctorLifetimeHistoryStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "detent.db")
	backend, err := store.Open(t.Context(), store.Config{Path: path})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	completedAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for index := 1; index <= 18; index++ {
		insertDoctorLifetimeReceipt(t, db, fmt.Sprintf("issue-%d", index), int64(index), int64(index)*1_000_000, false, completedAt)
	}
	insertDoctorLifetimeReceipt(t, db, "issue-p95", 60, 200_000_000, false, completedAt)
	insertDoctorLifetimeReceipt(t, db, "issue-runaway", 450, 374_000_000, false, completedAt)
	insertDoctorLifetimeReceipt(t, db, "issue-running", 900, 900_000_000, true, completedAt)
	return path
}

func insertDoctorLifetimeReceipt(t *testing.T, db *sql.DB, issueID string, sessions, tokens int64, inProgress bool, completedAt string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO efficiency_receipts (
project_id, issue_id, sessions, total_tokens, first_dispatched_at, completed_at, in_progress, refreshed_at
) VALUES ('detent', ?, ?, ?, ?, ?, ?, ?)`, issueID, sessions, tokens, completedAt, completedAt, inProgress, completedAt); err != nil {
		t.Fatalf("insert efficiency receipt %s: %v", issueID, err)
	}
}
