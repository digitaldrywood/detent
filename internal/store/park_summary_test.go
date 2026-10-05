package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
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

func TestWorkflowPhaseMetadataUpdatePreservesParkCount(t *testing.T) {
	t.Parallel()

	db := openParkTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	at := time.Date(2026, 8, 12, 17, 34, 57, 0, time.UTC)
	eventID, err := db.RecordWorkflowPhaseEvent(t.Context(), WorkflowPhaseEvent{
		ProjectID:    "detent",
		IssueID:      "issue-6",
		Identifier:   "digitaldrywood/detent.build#6",
		IssueURL:     "https://github.com/digitaldrywood/detent.build/issues/6",
		PhaseType:    WorkflowPhaseTypeLane,
		PhaseName:    "Blocked",
		Reason:       "no_progress_limit",
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: `{"blocked_recovery":{"owner":"orchestrator","cause":"no_progress_limit","cause_fingerprint":"legacy"}}`,
	})
	if err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	if err := db.UpdateWorkflowPhaseEventMetadata(t.Context(), eventID, `{"blocked_recovery":{"owner":"orchestrator","cause":"no_progress_limit","cause_fingerprint":"current","cause_fingerprint_version":2}}`); err != nil {
		t.Fatalf("UpdateWorkflowPhaseEventMetadata() error = %v", err)
	}

	summary, err := db.IssueParkSummary(t.Context(), parkTestIdentity())
	if err != nil {
		t.Fatalf("IssueParkSummary() error = %v", err)
	}
	if summary.ParkCount != 1 || len(summary.Causes) != 1 || summary.Causes[0].Count != 1 {
		t.Fatalf("park summary = %#v, want one migrated park", summary)
	}
	timeline, err := db.IssueWorkflowTimeline(t.Context(), parkTestIdentity())
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	if len(timeline.Events) != 1 || !strings.Contains(timeline.Events[0].MetadataJSON, `"cause_fingerprint":"current"`) {
		t.Fatalf("workflow timeline = %#v, want one event with current metadata", timeline.Events)
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
	for range 3 {
		if _, err := db.db.ExecContext(t.Context(), `INSERT INTO usage_events (
project_id, issue_id, identifier, model, input_tokens, cached_input_tokens, output_tokens, reasoning_output_tokens,
total_tokens, runtime_seconds, started_at, finished_at, event_day, outcome
) VALUES ('detent', 'issue-6', 'digitaldrywood/detent.build#6', 'gpt', 100, 80, 20, 10, 130, 1, ?, ?, '2026-08-12', 'success')`, first.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
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
	if summary.Tokens != (ParkTokenTotals{InputTokens: 300, CachedInputTokens: 240, OutputTokens: 60, ReasoningOutputTokens: 30}) {
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

func TestParkAcknowledgementDoesNotRegress(t *testing.T) {
	t.Parallel()
	for _, sequence := range []int64{0, 2, 4} {
		t.Run(strconv.FormatInt(sequence, 10), func(t *testing.T) {
			db := openParkTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
			now := time.Now().UTC().Truncate(time.Second)
			for index := range 4 {
				insertParkAttempt(t, db, now.Add(time.Duration(index-4)*time.Minute), "terminal", "no_progress", "no_progress_limit", `{}`)
			}
			if err := db.AcknowledgeIssueParks(t.Context(), parkTestIdentity(), 4, now); err != nil {
				t.Fatal(err)
			}
			if err := db.AcknowledgeIssueParks(t.Context(), parkTestIdentity(), sequence, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			summary, err := db.IssueParkSummary(t.Context(), parkTestIdentity())
			if err != nil || summary.AcknowledgedParkSequence != 4 || summary.AcknowledgedAt == nil || !summary.AcknowledgedAt.Equal(now) {
				t.Fatalf("stale acknowledgement replaced durable generation: %#v, %v", summary, err)
			}
		})
	}
}

func TestParkSummaryCoalescesBridgingAliases(t *testing.T) {
	t.Parallel()

	db := openParkTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	first := time.Date(2026, 8, 9, 16, 6, 0, 0, time.UTC)
	insertParkAttemptIdentity(t, db, first, "issue-legacy", "", "")
	insertParkAttemptIdentity(t, db, first.Add(time.Minute), "", "digitaldrywood/detent#1773", "")
	insertParkAttemptIdentity(t, db, first.Add(2*time.Minute), "issue-legacy", "digitaldrywood/detent#1773", "https://github.com/digitaldrywood/detent/issues/1773")

	summaries, err := db.ListIssueParkSummaries(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ListIssueParkSummaries() error = %v", err)
	}
	if len(summaries) != 1 || summaries[0].AttemptCount != 3 || summaries[0].ParkCount != 3 {
		t.Fatalf("summaries = %#v, want one summary with three attempts and parks", summaries)
	}
	requested := []IssueIdentity{
		{ProjectID: "detent", IssueID: "issue-legacy"},
		{ProjectID: "detent", Identifier: "digitaldrywood/detent#1773"},
		{ProjectID: "detent", IssueURL: "https://github.com/digitaldrywood/detent/issues/1773"},
	}
	byIssue, err := db.IssueParkSummaries(t.Context(), requested)
	if err != nil {
		t.Fatalf("IssueParkSummaries() error = %v", err)
	}
	if len(byIssue) != len(requested) {
		t.Fatalf("IssueParkSummaries() returned %d identities, want %d", len(byIssue), len(requested))
	}
	for _, identity := range requested {
		if summary := byIssue[identity]; summary.AttemptCount != 3 || summary.ParkCount != 3 {
			t.Fatalf("summary for %#v = %#v, want merged counts", identity, summary)
		}
	}
	for index, identity := range requested {
		summary, err := db.IssueParkSummary(t.Context(), identity)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{2, 2, 1}[index]
		if summary.AttemptCount != want || summary.ParkCount != want {
			t.Fatalf("single alias %#v expanded its row set: %#v", identity, summary)
		}
	}
}

func TestParkSummaryFilterScopesRequestedIdentities(t *testing.T) {
	t.Parallel()
	db := openParkTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	insertParkAttempt(t, db, time.Date(2026, 8, 9, 16, 6, 0, 0, time.UTC), "terminal", "failure", "", `{}`)
	if _, err := db.db.ExecContext(t.Context(), `INSERT INTO work_attempts (
project_id, issue_id, identifier, issue_url, worker_type, status, started_at
) SELECT 'other', issue_id, identifier, issue_url, worker_type, status, started_at FROM work_attempts`); err != nil {
		t.Fatal(err)
	}
	identity := parkTestIdentity()
	batch := make([]IssueIdentity, 200)
	for index := range batch {
		batch[index] = IssueIdentity{ProjectID: "detent", IssueID: "missing-" + strconv.Itoa(index), Identifier: "owner/repo#" + strconv.Itoa(index), IssueURL: "https://example.com/" + strconv.Itoa(index)}
	}
	batch = append(batch, identity, identity)
	tests := []struct {
		name            string
		projectID       string
		identities      []IssueIdentity
		includeIssueURL bool
		wantRows        int
		wantEmpty       bool
	}{
		{name: "all aliases count one row", identities: []IssueIdentity{identity}, includeIssueURL: true, wantRows: 1},
		{name: "usage aliases omit URL", identities: []IssueIdentity{identity}, wantRows: 1},
		{name: "URL alone", identities: []IssueIdentity{{ProjectID: "detent", IssueURL: identity.IssueURL}}, includeIssueURL: true, wantRows: 1},
		{name: "usage cannot match URL alone", identities: []IssueIdentity{{ProjectID: "detent", IssueURL: identity.IssueURL}}},
		{name: "partial mismatch keeps matching alias", identities: []IssueIdentity{{ProjectID: "detent", IssueID: "missing", Identifier: identity.Identifier}}, wantRows: 1},
		{name: "missing project matches no row", identities: []IssueIdentity{{Identifier: identity.Identifier}}, wantEmpty: true},
		{name: "empty alias matches no row", identities: []IssueIdentity{{ProjectID: "detent"}}, wantEmpty: true},
		{name: "missing project scope", identities: []IssueIdentity{{ProjectID: "missing", Identifier: identity.Identifier}}, wantEmpty: true},
		{name: "normalized identity", identities: []IssueIdentity{{ProjectID: " detent ", IssueID: " issue-6 "}}, wantRows: 1},
		{name: "board batch deduplicates without compound limit", identities: batch, includeIssueURL: true, wantRows: 1},
		{name: "project list", projectID: "detent", wantRows: 1},
		{name: "global list", wantRows: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			filter, args, err := parkSummaryFilter("WHERE", "work_attempts", tt.projectID, tt.identities, tt.includeIssueURL)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.db.QueryRowContext(t.Context(), "SELECT count(*) FROM work_attempts"+filter, args...).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != tt.wantRows {
				t.Fatalf("matched rows = %d, want %d", count, tt.wantRows)
			}
			if tt.identities != nil {
				summaries, err := db.IssueParkSummaries(t.Context(), tt.identities)
				if err != nil {
					t.Fatal(err)
				}
				if tt.wantEmpty && len(summaries) != 0 {
					t.Fatalf("invalid identities returned summaries: %#v", summaries)
				}
				for requested, summary := range summaries {
					if summary.ProjectID != requested.ProjectID || summary.AttemptCount != 1 {
						t.Fatalf("scoped identity %#v returned duplicated or cross-project summary: %#v", requested, summary)
					}
				}
			}
		})
	}
}

func openParkTestStore(t *testing.T, path string) *sqliteStore {
	t.Helper()
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	if _, err := db.db.ExecContext(t.Context(), `INSERT INTO work_attempts (
project_id, issue_id, identifier, issue_url, worker_type, attempt_number, status, started_at, completed_at,
terminal_state, error_class, worker_metadata_json
) VALUES ('detent', 'issue-6', 'digitaldrywood/detent.build#6', 'https://github.com/digitaldrywood/detent.build/issues/6', 'codex', 1, ?, ?, ?, ?, ?, ?)`, status, time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC).Format(time.RFC3339Nano), completed, terminalState, errorClass, metadata); err != nil {
		t.Fatalf("insert work attempt: %v", err)
	}
}

func insertParkAttemptIdentity(t *testing.T, db *sqliteStore, at time.Time, issueID, identifier, issueURL string) {
	t.Helper()
	if _, err := db.db.ExecContext(t.Context(), `INSERT INTO work_attempts (
project_id, issue_id, identifier, issue_url, worker_type, attempt_number, status, started_at, completed_at,
terminal_state, worker_metadata_json
) VALUES ('detent', NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), 'codex', 1, 'terminal', ?, ?, 'no_progress', '{}')`,
		issueID, identifier, issueURL, at.Add(-time.Minute).Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert work attempt identity: %v", err)
	}
}

func insertParkTransition(t *testing.T, db *sqliteStore, at time.Time, reason, metadata string) {
	t.Helper()
	if _, err := db.db.ExecContext(t.Context(), `INSERT INTO workflow_phase_events (
project_id, issue_id, identifier, issue_url, phase_type, phase_name, reason, status, started_at, event_day, metadata_json
) VALUES ('detent', 'issue-6', 'digitaldrywood/detent.build#6', 'https://github.com/digitaldrywood/detent.build/issues/6', 'lane', 'Blocked', ?, 'entered', ?, '2026-08-12', ?)`, reason, at.Format(time.RFC3339Nano), metadata); err != nil {
		t.Fatalf("insert workflow transition: %v", err)
	}
}
