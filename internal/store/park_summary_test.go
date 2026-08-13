package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestParkDefinition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		insert       func(*testing.T, *sqliteStore, time.Time)
		wantAttempts int64
		wantParks    int64
		wantCause    string
	}{
		{name: "terminal no progress", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkAttempt(t, db, at, "terminal", "no_progress", "", `{}`)
		}, wantAttempts: 1, wantParks: 1, wantCause: "no_progress"},
		{name: "brake caused failure", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkAttempt(t, db, at, "terminal", "failure", "runner_error", `{"brake":{"cause":"per_issue_max_usd"}}`)
		}, wantAttempts: 1, wantParks: 1, wantCause: "per_issue_max_usd"},
		{name: "ordinary failure excluded", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkAttempt(t, db, at, "terminal", "failure", "runner_error", `{}`)
		}, wantAttempts: 1},
		{name: "capacity deferral excluded", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkAttempt(t, db, at, "terminal", "capacity", "provider_capacity", `{}`)
		}, wantAttempts: 1},
		{name: "nonterminal heartbeat excluded", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkAttempt(t, db, at, "active", "no_progress", "", `{}`)
		}, wantAttempts: 1},
		{name: "orchestrator blocked transition", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkTransition(t, db, at, "no_progress_limit", `{"blocked_recovery":{"owner":"orchestrator","cause":"no_progress_limit"}}`)
		}, wantParks: 1, wantCause: "no_progress_limit"},
		{name: "human blocked transition excluded", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkTransition(t, db, at, "operator decision", `{"provenance":{"origin":"human","initiator":"human"}}`)
		}},
		{name: "attempt and transition count once", insert: func(t *testing.T, db *sqliteStore, at time.Time) {
			insertParkAttempt(t, db, at, "terminal", "no_progress", "", `{}`)
			insertParkTransition(t, db, at, "no_progress_limit", `{"blocked_recovery":{"owner":"orchestrator","cause":"no_progress_limit"}}`)
		}, wantAttempts: 1, wantParks: 1, wantCause: "no_progress_limit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openParkTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
			at := time.Date(2026, 8, 12, 17, 34, 57, 0, time.UTC)
			tt.insert(t, db, at)
			summary, err := db.IssueParkSummary(t.Context(), parkTestIdentity())
			if err != nil && tt.wantAttempts == 0 && tt.wantParks == 0 {
				if errors.Is(err, ErrNotFound) {
					return
				}
				t.Fatalf("IssueParkSummary() error = %v", err)
			}
			if err != nil {
				t.Fatalf("IssueParkSummary() error = %v", err)
			}
			if summary.AttemptCount != tt.wantAttempts || summary.ParkCount != tt.wantParks {
				t.Fatalf("counts = attempts %d parks %d, want %d/%d", summary.AttemptCount, summary.ParkCount, tt.wantAttempts, tt.wantParks)
			}
			if tt.wantCause != "" && (len(summary.Causes) != 1 || summary.Causes[0].Cause != tt.wantCause || summary.Causes[0].Count != 1) {
				t.Fatalf("Causes = %#v, want one %q", summary.Causes, tt.wantCause)
			}
		})
	}
}

func TestParkSummaryAggregatesCausesAndTokenBreakdown(t *testing.T) {
	t.Parallel()

	db := openParkTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	first := time.Date(2026, 8, 9, 16, 6, 0, 0, time.UTC)
	last := first.Add(72*time.Hour + 28*time.Minute + 57*time.Second)
	insertParkAttempt(t, db, first, "terminal", "failure", "", `{"brake_cause":"per_issue_max_usd"}`)
	insertParkAttempt(t, db, last, "terminal", "failure", "", `{"brake_cause":"per_issue_max_usd"}`)
	insertParkAttempt(t, db, last.Add(time.Second), "terminal", "no_progress", "no_progress_limit", `{}`)
	if _, err := db.db.Exec(`INSERT INTO usage_events (
project_id, issue_id, identifier, model, input_tokens, cached_input_tokens, output_tokens, reasoning_output_tokens,
total_tokens, runtime_seconds, started_at, finished_at, event_day, outcome
) VALUES ('detent', 'issue-6', 'digitaldrywood/detent.build#6', 'gpt', 100, 80, 20, 10, 130, 1, ?, ?, '2026-08-12', 'success')`, first.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}
	summary, err := db.IssueParkSummary(t.Context(), parkTestIdentity())
	if err != nil {
		t.Fatalf("IssueParkSummary() error = %v", err)
	}
	if summary.AttemptCount != 3 || summary.ParkCount != 3 || len(summary.Causes) != 2 {
		t.Fatalf("summary counts = %#v, want 3 attempts, 3 parks, 2 causes", summary)
	}
	if summary.Causes[0].Cause != "per_issue_max_usd" || summary.Causes[0].Count != 2 || !summary.Causes[0].FirstAt.Equal(first) || !summary.Causes[0].LastAt.Equal(last) {
		t.Fatalf("aggregated brake = %#v", summary.Causes[0])
	}
	if summary.Tokens != (ParkTokenTotals{InputTokens: 100, CachedInputTokens: 80, OutputTokens: 20, ReasoningOutputTokens: 10}) {
		t.Fatalf("Tokens = %#v", summary.Tokens)
	}
}

func TestParkAcknowledgementPersistsAndRearms(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db")
	db := openParkTestStore(t, path)
	first := time.Date(2026, 8, 12, 17, 34, 57, 0, time.UTC)
	insertParkAttempt(t, db, first, "terminal", "no_progress", "no_progress_limit", `{}`)
	summary, err := db.IssueParkSummary(t.Context(), parkTestIdentity())
	if err != nil {
		t.Fatalf("IssueParkSummary() error = %v", err)
	}
	if !summary.ReviewRecommended(1) {
		t.Fatal("ReviewRecommended(1) = false before acknowledgement")
	}
	if err := db.AcknowledgeIssueParks(t.Context(), parkTestIdentity(), summary.ParkCount, first.Add(time.Minute)); err != nil {
		t.Fatalf("AcknowledgeIssueParks() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db = openParkTestStore(t, path)
	summary, err = db.IssueParkSummary(t.Context(), parkTestIdentity())
	if err != nil {
		t.Fatalf("IssueParkSummary() after restart error = %v", err)
	}
	if summary.AcknowledgedParkSequence != 1 || summary.ReviewRecommended(1) {
		t.Fatalf("acknowledged summary = %#v, want sequence 1 and cleared recommendation", summary)
	}
	insertParkAttempt(t, db, first.Add(time.Hour), "terminal", "no_progress", "no_progress_limit", `{}`)
	summary, err = db.IssueParkSummary(t.Context(), parkTestIdentity())
	if err != nil {
		t.Fatalf("IssueParkSummary() rearmed error = %v", err)
	}
	if summary.ParkCount != 2 || !summary.ReviewRecommended(1) {
		t.Fatalf("rearmed summary = %#v", summary)
	}
}

func openParkTestStore(t *testing.T, path string) *sqliteStore {
	t.Helper()
	db, err := openSQLite(context.Background(), Config{Path: path})
	if err != nil {
		t.Fatalf("openSQLite() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func parkTestIdentity() IssueIdentity {
	return IssueIdentity{ProjectID: "detent", IssueID: "issue-6", Identifier: "digitaldrywood/detent.build#6", IssueURL: "https://github.com/digitaldrywood/detent.build/issues/6"}
}

func insertParkAttempt(t *testing.T, db *sqliteStore, at time.Time, status, terminalState, errorClass, metadata string) {
	t.Helper()
	var completed any
	if !at.IsZero() {
		completed = at.Format(time.RFC3339Nano)
	}
	if _, err := db.db.Exec(`INSERT INTO work_attempts (
project_id, issue_id, identifier, issue_url, worker_type, attempt_number, status, started_at, completed_at,
terminal_state, error_class, worker_metadata_json
) VALUES ('detent', 'issue-6', 'digitaldrywood/detent.build#6', 'https://github.com/digitaldrywood/detent.build/issues/6', 'codex', 1, ?, ?, ?, ?, ?, ?)`, status, time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC).Format(time.RFC3339Nano), completed, terminalState, errorClass, metadata); err != nil {
		t.Fatalf("insert work attempt: %v", err)
	}
}

func insertParkTransition(t *testing.T, db *sqliteStore, at time.Time, reason, metadata string) {
	t.Helper()
	if _, err := db.db.Exec(`INSERT INTO workflow_phase_events (
project_id, issue_id, identifier, issue_url, phase_type, phase_name, reason, status, started_at, event_day, metadata_json
) VALUES ('detent', 'issue-6', 'digitaldrywood/detent.build#6', 'https://github.com/digitaldrywood/detent.build/issues/6', 'lane', 'Blocked', ?, 'entered', ?, '2026-08-12', ?)`, reason, at.Format(time.RFC3339Nano), metadata); err != nil {
		t.Fatalf("insert workflow transition: %v", err)
	}
}
