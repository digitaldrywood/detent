package store

import (
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestProjectLifetimeUsageUsesCompletedProjectHistory(t *testing.T) {
	t.Parallel()

	backend := openTestStore(t, t.Context())
	db := backend.(*sqliteStore).db
	completedAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for index := 1; index <= 18; index++ {
		insertLifetimeHistoryReceipt(t, db, "detent", fmt.Sprintf("issue-%d", index), int64(index), int64(index)*1_000_000, false, completedAt)
	}
	insertLifetimeHistoryReceipt(t, db, "detent", "issue-p95", 60, 200_000_000, false, completedAt)
	insertLifetimeHistoryReceipt(t, db, "detent", "issue-runaway", 450, 374_000_000, false, completedAt)
	insertLifetimeHistoryReceipt(t, db, "detent", "issue-running", 900, 900_000_000, true, completedAt)
	insertLifetimeHistoryReceipt(t, db, "other", "issue-other", 800, 800_000_000, false, completedAt)

	usage, err := backend.(ProjectLifetimeUsageStore).ProjectLifetimeUsage(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ProjectLifetimeUsage() error = %v", err)
	}
	if usage.CompletedIssues != 20 || usage.P95Sessions != 60 || usage.P95Tokens != 200_000_000 {
		t.Fatalf("ProjectLifetimeUsage() = %#v, want 20 completed issues with p95 60/200000000", usage)
	}
	if math.Abs(usage.MeanSessions-34.05) > 0.000001 {
		t.Fatalf("MeanSessions = %v, want 34.05", usage.MeanSessions)
	}
}

func insertLifetimeHistoryReceipt(t *testing.T, db *sql.DB, projectID, issueID string, sessions, tokens int64, inProgress bool, completedAt string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO efficiency_receipts (
project_id, issue_id, sessions, total_tokens, first_dispatched_at, completed_at, in_progress, refreshed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, projectID, issueID, sessions, tokens, completedAt, completedAt, inProgress, completedAt); err != nil {
		t.Fatalf("insert efficiency receipt %s: %v", issueID, err)
	}
}
