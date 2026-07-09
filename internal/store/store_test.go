package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func TestOpenSQLiteAppliesMigrationsAndPragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")

	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	sqliteBackend, ok := backend.(*sqliteStore)
	if !ok {
		t.Fatalf("Open() returned %T, want *sqliteStore", backend)
	}

	if got := queryString(t, sqliteBackend.db, "PRAGMA journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %q, want wal", got)
	}
	if got := queryInt(t, sqliteBackend.db, "PRAGMA busy_timeout"); got != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", got)
	}
	if got := queryInt(t, sqliteBackend.db, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('detent_runs', 'codex_sessions', 'fair_share_usage', 'usage_events', 'workflow_phase_events', 'work_attempts', 'scheduler_decisions', 'validator_verdicts', 'api_keys', 'api_usage_logs')"); got != 10 {
		t.Fatalf("migrated table count = %d, want 10", got)
	}
}

func TestCachedTokenTelemetryMigrationUpDown(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})
	if err := configureSQLite(ctx, db, 0); err != nil {
		t.Fatalf("configureSQLite() error = %v", err)
	}

	migrationMu.Lock()
	defer migrationMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose.SetDialect() error = %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 6); err != nil {
		t.Fatalf("goose.UpToContext(6) error = %v", err)
	}

	if _, err := db.ExecContext(ctx, `
INSERT INTO codex_sessions (started_at, input_tokens, output_tokens, total_tokens)
VALUES ('2026-05-31T13:00:00Z', 10, 2, 12);
INSERT INTO usage_events (project_id, model, input_tokens, output_tokens, total_tokens, runtime_seconds, started_at, finished_at, event_day, outcome, cost_usd)
VALUES ('detent', 'gpt-5', 10, 2, 12, 5, '2026-05-31T13:00:00Z', '2026-05-31T13:00:05Z', '2026-05-31', 'completed', 0.001);
INSERT INTO workflow_phase_events (project_id, phase_type, phase_name, started_at, duration_seconds, event_day, input_tokens, output_tokens, total_tokens)
VALUES ('detent', 'agent_session', 'agent_active', '2026-05-31T13:00:00Z', 5, '2026-05-31', 10, 2, 12);
`); err != nil {
		t.Fatalf("seed old schema rows error = %v", err)
	}
	for _, table := range []string{"codex_sessions", "usage_events", "workflow_phase_events"} {
		assertColumnAbsent(t, db, table, "cached_input_tokens")
	}

	if err := goose.UpToContext(ctx, db, "migrations", 8); err != nil {
		t.Fatalf("goose.UpToContext(8) error = %v", err)
	}
	for _, table := range []string{"codex_sessions", "usage_events", "workflow_phase_events"} {
		assertColumnPresent(t, db, table, "cached_input_tokens")
		assertColumnPresent(t, db, table, "reasoning_output_tokens")
		assertColumnPresent(t, db, table, "model_context_window")
		assertTelemetryColumnsNull(t, db, table)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 6); err != nil {
		t.Fatalf("goose.DownToContext(6) error = %v", err)
	}
	for _, table := range []string{"codex_sessions", "usage_events", "workflow_phase_events"} {
		assertColumnAbsent(t, db, table, "cached_input_tokens")
		assertColumnAbsent(t, db, table, "reasoning_output_tokens")
		assertColumnAbsent(t, db, table, "model_context_window")
	}
}

func TestAgentResumeStateMigrationUpDown(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})
	if err := configureSQLite(ctx, db, 0); err != nil {
		t.Fatalf("configureSQLite() error = %v", err)
	}

	migrationMu.Lock()
	defer migrationMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose.SetDialect() error = %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 8); err != nil {
		t.Fatalf("goose.UpToContext(8) error = %v", err)
	}

	for _, column := range []string{"requested_model", "agent_backend_id", "agent_backend_kind", "agent_role", "provider_thread_id", "provider_session_id", "resumed_from_session_id"} {
		assertColumnAbsent(t, db, "codex_sessions", column)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 9); err != nil {
		t.Fatalf("goose.UpToContext(9) error = %v", err)
	}
	for _, column := range []string{"requested_model", "agent_backend_id", "agent_backend_kind", "agent_role", "provider_thread_id", "provider_session_id", "resumed_from_session_id"} {
		assertColumnPresent(t, db, "codex_sessions", column)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 8); err != nil {
		t.Fatalf("goose.DownToContext(8) error = %v", err)
	}
	for _, column := range []string{"requested_model", "agent_backend_id", "agent_backend_kind", "agent_role", "provider_thread_id", "provider_session_id", "resumed_from_session_id"} {
		assertColumnAbsent(t, db, "codex_sessions", column)
	}
}

func TestRuntimeEvidenceReportsSQLiteTelemetry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	finishedAt := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	if _, err := backend.RecordUsageEvent(ctx, UsageEvent{
		ProjectID:      "detent",
		Model:          "gpt-5-codex",
		StartedAt:      finishedAt.Add(-5 * time.Minute),
		FinishedAt:     finishedAt,
		RuntimeSeconds: 300,
		TotalTokens:    1200,
		Outcome:        "success",
	}); err != nil {
		t.Fatalf("RecordUsageEvent() error = %v", err)
	}
	if _, err := backend.RecordWorkflowPhaseEvent(ctx, WorkflowPhaseEvent{
		ProjectID:       "detent",
		IssueID:         "issue-755",
		Identifier:      "digitaldrywood/detent#755",
		PhaseType:       WorkflowPhaseTypeLane,
		PhaseName:       "In Progress",
		Status:          "completed",
		StartedAt:       finishedAt.Add(-30 * time.Minute),
		FinishedAt:      finishedAt,
		DurationSeconds: int64((30 * time.Minute) / time.Second),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}

	evidence, err := backend.RuntimeEvidence(ctx, RuntimeEvidenceQuery{ProjectID: "detent"})
	if err != nil {
		t.Fatalf("RuntimeEvidence() error = %v", err)
	}

	if evidence.Backend != BackendSQLite || evidence.Path != dbPath || !evidence.Healthy {
		t.Fatalf("RuntimeEvidence() = %#v, want healthy sqlite evidence for %q", evidence, dbPath)
	}
	if evidence.MigrationVersion < 7 || evidence.MigrationStatus == "" {
		t.Fatalf("migration evidence = version %d status %q, want applied version", evidence.MigrationVersion, evidence.MigrationStatus)
	}
	if got := runtimeEvidenceTableCount(evidence.Tables, "usage_events"); got != 1 {
		t.Fatalf("usage_events row count = %d, want 1", got)
	}
	if got := runtimeEvidenceTableCount(evidence.Tables, "workflow_phase_events"); got != 1 {
		t.Fatalf("workflow_phase_events row count = %d, want 1", got)
	}
	if got := runtimeEvidenceTableCount(evidence.Tables, "api_keys"); got != 0 {
		t.Fatalf("api_keys row count = %d, want 0", got)
	}
	if evidence.WorkflowPhaseEvents.RowCount != 1 {
		t.Fatalf("WorkflowPhaseEvents.RowCount = %d, want 1", evidence.WorkflowPhaseEvents.RowCount)
	}
	if evidence.WorkflowPhaseEvents.OldestFinishedAt == nil || !evidence.WorkflowPhaseEvents.OldestFinishedAt.Equal(finishedAt) {
		t.Fatalf("OldestFinishedAt = %#v, want %s", evidence.WorkflowPhaseEvents.OldestFinishedAt, finishedAt)
	}
	if evidence.WorkflowPhaseEvents.NewestFinishedAt == nil || !evidence.WorkflowPhaseEvents.NewestFinishedAt.Equal(finishedAt) {
		t.Fatalf("NewestFinishedAt = %#v, want %s", evidence.WorkflowPhaseEvents.NewestFinishedAt, finishedAt)
	}
}

func TestSQLiteValidatorVerdictRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	recordedAt := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	key := ValidatorVerdictKey{ProjectID: "detent", IssueID: "issue-858", HeadSHA: "abc123"}
	if err := backend.RecordValidatorVerdict(ctx, ValidatorVerdict{
		ProjectID:  key.ProjectID,
		IssueID:    key.IssueID,
		HeadSHA:    key.HeadSHA,
		Identifier: "digitaldrywood/detent#858",
		IssueURL:   "https://github.test/digitaldrywood/detent/issues/858",
		PRNumber:   int64Pointer(875),
		Submitted:  true,
		Verdict:    "pass",
		Score:      0.93,
		Summary:    "acceptance criteria pass",
		Findings: []ValidatorFinding{{
			Severity: "p2",
			Body:     "minor follow-up",
			URL:      "https://github.test/digitaldrywood/detent/pull/875#discussion_r1",
			Path:     "internal/orchestrator/autopromote_tick.go",
			Line:     42,
		}},
		RecordedAt: recordedAt,
		UpdatedAt:  recordedAt,
	}); err != nil {
		t.Fatalf("RecordValidatorVerdict() error = %v", err)
	}

	got, err := backend.ValidatorVerdict(ctx, key)
	if err != nil {
		t.Fatalf("ValidatorVerdict() error = %v", err)
	}
	if got.ProjectID != key.ProjectID || got.IssueID != key.IssueID || got.HeadSHA != key.HeadSHA {
		t.Fatalf("verdict key = %#v, want %#v", got, key)
	}
	if !got.Submitted || got.Verdict != "pass" || got.Score != 0.93 || got.Summary != "acceptance criteria pass" {
		t.Fatalf("verdict result = %#v, want submitted pass score", got)
	}
	if got.PRNumber == nil || *got.PRNumber != 875 {
		t.Fatalf("PRNumber = %#v, want 875", got.PRNumber)
	}
	if len(got.Findings) != 1 || got.Findings[0].Severity != "p2" || got.Findings[0].Path != "internal/orchestrator/autopromote_tick.go" {
		t.Fatalf("Findings = %#v, want persisted finding", got.Findings)
	}
	if got.Commented {
		t.Fatalf("Commented = true, want false")
	}

	commentedAt := recordedAt.Add(time.Minute)
	if err := backend.MarkValidatorVerdictCommented(ctx, key, commentedAt); err != nil {
		t.Fatalf("MarkValidatorVerdictCommented() error = %v", err)
	}
	got, err = backend.ValidatorVerdict(ctx, key)
	if err != nil {
		t.Fatalf("ValidatorVerdict() after commented error = %v", err)
	}
	if !got.Commented || !got.UpdatedAt.Equal(commentedAt) {
		t.Fatalf("commented verdict = %#v, want commented at %s", got, commentedAt)
	}
}

func TestSQLiteListValidatorVerdictsFiltersAndSorts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	verdicts := []ValidatorVerdict{
		{
			ProjectID:  "detent",
			IssueID:    "issue-1",
			HeadSHA:    "aaa111",
			Identifier: "digitaldrywood/detent#1",
			PRNumber:   int64Pointer(11),
			Verdict:    "pass",
			RecordedAt: base,
			UpdatedAt:  base,
		},
		{
			ProjectID:  "detent",
			IssueID:    "issue-2",
			HeadSHA:    "bbb222",
			Identifier: "digitaldrywood/detent#2",
			PRNumber:   int64Pointer(12),
			Verdict:    "rework",
			RecordedAt: base.Add(time.Hour),
			UpdatedAt:  base.Add(time.Hour),
		},
		{
			ProjectID:  "video",
			IssueID:    "render-1",
			HeadSHA:    "ccc333",
			Identifier: "video/render-1",
			Verdict:    "pass",
			RecordedAt: base.Add(2 * time.Hour),
			UpdatedAt:  base.Add(2 * time.Hour),
		},
	}
	for _, verdict := range verdicts {
		if err := backend.RecordValidatorVerdict(ctx, verdict); err != nil {
			t.Fatalf("RecordValidatorVerdict(%s) error = %v", verdict.IssueID, err)
		}
	}

	got, err := backend.ListValidatorVerdicts(ctx, ValidatorVerdictQuery{})
	if err != nil {
		t.Fatalf("ListValidatorVerdicts() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListValidatorVerdicts() len = %d, want 3", len(got))
	}
	if got[0].IssueID != "render-1" || got[1].IssueID != "issue-2" || got[2].IssueID != "issue-1" {
		t.Fatalf("ListValidatorVerdicts() order = %#v", []string{got[0].IssueID, got[1].IssueID, got[2].IssueID})
	}

	filtered, err := backend.ListValidatorVerdicts(ctx, ValidatorVerdictQuery{
		ProjectID: "detent",
		From:      base.Add(30 * time.Minute),
		To:        base.Add(90 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ListValidatorVerdicts(filtered) error = %v", err)
	}
	if len(filtered) != 1 || filtered[0].IssueID != "issue-2" {
		t.Fatalf("filtered verdicts = %#v, want issue-2", filtered)
	}
}

func TestSQLiteQueriesRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "detent.db")

	backend, err := Open(ctx, Config{
		Backend:     BackendSQLite,
		Path:        dbPath,
		BusyTimeout: 2500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	run, err := backend.Queries().CreateDetentRun(ctx, sqlc.CreateDetentRunParams{
		StartedAt:            "2026-05-30T12:00:00Z",
		StoppedAt:            sql.NullString{},
		RestartReason:        sql.NullString{},
		PeakConcurrentAgents: 3,
		SessionsLaunched:     1,
		InputTokens:          120,
		OutputTokens:         30,
		TotalTokens:          150,
		RuntimeSeconds:       90,
	})
	if err != nil {
		t.Fatalf("CreateDetentRun() error = %v", err)
	}

	session, err := backend.Queries().CreateCodexSession(ctx, sqlc.CreateCodexSessionParams{
		RunID:          sql.NullInt64{Int64: run.ID, Valid: true},
		IssueID:        sql.NullString{String: "I_kwDOSskuwc8AAAABD42cNw", Valid: true},
		Identifier:     sql.NullString{String: "digitaldrywood/detent#5", Valid: true},
		IssueURL:       sql.NullString{String: "https://github.com/digitaldrywood/detent/issues/5", Valid: true},
		StartedAt:      sql.NullString{String: "2026-05-30T12:01:00Z", Valid: true},
		CompletedAt:    sql.NullString{String: "2026-05-30T12:02:00Z", Valid: true},
		Turns:          2,
		InputTokens:    100,
		OutputTokens:   20,
		TotalTokens:    120,
		RuntimeSeconds: 60,
		FinalState:     sql.NullString{String: "Human Review", Valid: true},
		Model:          sql.NullString{String: "gpt-5", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateCodexSession() error = %v", err)
	}

	got, err := backend.Queries().GetCodexSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetCodexSession() error = %v", err)
	}

	if got.RunID.Int64 != run.ID {
		t.Fatalf("session run_id = %d, want %d", got.RunID.Int64, run.ID)
	}
	if got.Identifier.String != "digitaldrywood/detent#5" {
		t.Fatalf("session identifier = %q, want digitaldrywood/detent#5", got.Identifier.String)
	}
}

func runtimeEvidenceTableCount(tables []RuntimeTableEvidence, name string) int64 {
	for _, table := range tables {
		if table.Name == name {
			return table.RowCount
		}
	}
	return -1
}

func TestWorkAttemptStoreRoundTripDecisionsAndRecovery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)

	prNumber := int64(737)
	attemptID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
		ProjectID:            " detent ",
		IssueID:              " issue-737 ",
		Identifier:           " digitaldrywood/detent#737 ",
		IssueURL:             " https://github.com/digitaldrywood/detent/issues/737 ",
		PRNumber:             &prNumber,
		Repo:                 " digitaldrywood/detent ",
		WorkerType:           " agent ",
		WorkerHost:           " worker-a ",
		Lane:                 " In Progress ",
		AttemptNumber:        2,
		StartedAt:            base,
		LeaseExpiresAt:       base.Add(5 * time.Minute),
		WorkerMetadataJSON:   `{"mode":"implement"}`,
		CapacitySnapshotJSON: `{"global_used":1}`,
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	if attemptID <= 0 {
		t.Fatalf("StartWorkAttempt() id = %d, want positive", attemptID)
	}

	if err := backend.RecordWorkAttemptHeartbeat(ctx, WorkAttemptHeartbeat{
		AttemptID:              attemptID,
		HeartbeatAt:            base.Add(time.Minute),
		LeaseExpiresAt:         base.Add(6 * time.Minute),
		Phase:                  "testing",
		StatusMessage:          "running focused tests",
		WaitReason:             "github_checks",
		GitHubRateSnapshotJSON: `{"rest_remaining":4878}`,
		CapacitySnapshotJSON:   `{"global_available":1}`,
		MetricsJSON:            `{"test_runs":1}`,
		NextAction:             "wait for CI",
	}); err != nil {
		t.Fatalf("RecordWorkAttemptHeartbeat() error = %v", err)
	}

	if _, err := backend.RecordSchedulerDecision(ctx, SchedulerDecision{
		ProjectID:            " detent ",
		IssueID:              " issue-738 ",
		Identifier:           " digitaldrywood/detent#738 ",
		Repo:                 " digitaldrywood/detent ",
		Lane:                 " Rework ",
		QueuePosition:        3,
		Result:               SchedulerDecisionResultSkipped,
		Reason:               " repo_merge_lock ",
		DecisionAt:           base.Add(2 * time.Minute),
		CapacitySnapshotJSON: `{"repo_merge_lock":"digitaldrywood/detent"}`,
	}); err != nil {
		t.Fatalf("RecordSchedulerDecision() error = %v", err)
	}

	active, err := backend.ListActiveWorkAttempts(ctx, WorkAttemptQuery{ProjectID: "detent"})
	if err != nil {
		t.Fatalf("ListActiveWorkAttempts() error = %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active attempts len = %d, want 1: %#v", len(active), active)
	}
	got := active[0]
	if got.ProjectID != "detent" || got.IssueID != "issue-737" || got.AttemptNumber != 2 || got.Phase != "testing" {
		t.Fatalf("active attempt = %#v, want normalized heartbeat", got)
	}
	if got.Status != WorkAttemptStatusActive {
		t.Fatalf("active status = %q, want %q", got.Status, WorkAttemptStatusActive)
	}

	decisions, err := backend.ListRecentSchedulerDecisions(ctx, SchedulerDecisionQuery{ProjectID: "detent", Limit: 5})
	if err != nil {
		t.Fatalf("ListRecentSchedulerDecisions() error = %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("decisions len = %d, want 1: %#v", len(decisions), decisions)
	}
	if decisions[0].Result != SchedulerDecisionResultSkipped || decisions[0].Reason != "repo_merge_lock" {
		t.Fatalf("decision = %#v, want skipped repo_merge_lock", decisions[0])
	}

	if err := backend.CompleteWorkAttempt(ctx, WorkAttemptCompletion{
		AttemptID:          attemptID,
		CompletedAt:        base.Add(3 * time.Minute),
		Status:             WorkAttemptStatusTerminal,
		TerminalState:      WorkAttemptTerminalNoProgress,
		Phase:              "completed",
		WorkerMetadataJSON: `{"completion_progress":{"outcome":"no_progress","current_head_sha":"abc123"}}`,
	}); err != nil {
		t.Fatalf("CompleteWorkAttempt() error = %v", err)
	}
	active, err = backend.ListActiveWorkAttempts(ctx, WorkAttemptQuery{ProjectID: "detent"})
	if err != nil {
		t.Fatalf("ListActiveWorkAttempts() after complete error = %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active attempts after complete = %#v, want none", active)
	}
	history, err := backend.ListRecentTerminalWorkAttempts(ctx, WorkAttemptHistoryQuery{
		ProjectID:  "detent",
		IssueID:    "issue-737",
		WorkerType: "agent",
		Limit:      5,
	})
	if err != nil {
		t.Fatalf("ListRecentTerminalWorkAttempts() error = %v", err)
	}
	if len(history) != 1 || history[0].ID != attemptID || history[0].TerminalState != WorkAttemptTerminalNoProgress {
		t.Fatalf("history = %#v, want completed no_progress attempt %d", history, attemptID)
	}
	if history[0].WorkerMetadataJSON != `{"completion_progress":{"outcome":"no_progress","current_head_sha":"abc123"}}` {
		t.Fatalf("history WorkerMetadataJSON = %q", history[0].WorkerMetadataJSON)
	}

	staleID, err := backend.StartWorkAttempt(ctx, WorkAttemptStart{
		ProjectID:      "detent",
		IssueID:        "issue-stale",
		Identifier:     "digitaldrywood/detent#739",
		Repo:           "digitaldrywood/detent",
		WorkerType:     "merge",
		Lane:           "Merging",
		AttemptNumber:  1,
		StartedAt:      base.Add(-10 * time.Minute),
		LeaseExpiresAt: base.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() stale error = %v", err)
	}
	recovered, err := backend.TimeoutExpiredWorkAttempts(ctx, WorkAttemptTimeout{
		Now:           base,
		TerminalState: WorkAttemptTerminalTimedOut,
		ErrorClass:    "stale_lease",
		ErrorMessage:  "worker lease expired",
	})
	if err != nil {
		t.Fatalf("TimeoutExpiredWorkAttempts() error = %v", err)
	}
	if len(recovered) != 1 || recovered[0].ID != staleID || recovered[0].TerminalState != WorkAttemptTerminalTimedOut {
		t.Fatalf("recovered stale attempts = %#v, want stale timeout id %d", recovered, staleID)
	}
}

func TestStatsStoreRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  RunStart
	}{
		{
			name: "persists run and session stats",
			run: RunStart{
				StartedAt:            time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
				PeakConcurrentAgents: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			backend := openTestStore(t, ctx)

			runID, err := backend.StartRun(ctx, tt.run)
			if err != nil {
				t.Fatalf("StartRun() error = %v", err)
			}

			if err := backend.UpdateRun(ctx, runID, RunUpdate{
				PeakConcurrentAgents: 3,
				SessionsLaunched:     1,
				InputTokens:          100,
				OutputTokens:         25,
				TotalTokens:          125,
				RuntimeSeconds:       240,
			}); err != nil {
				t.Fatalf("UpdateRun() error = %v", err)
			}

			sessionID, err := backend.StartSession(ctx, SessionStart{
				RunID:      runID,
				IssueID:    "I_kwDOSskuwc8AAAABD42c3Q",
				Identifier: "digitaldrywood/detent#6",
				IssueURL:   "https://github.com/digitaldrywood/detent/issues/6",
				StartedAt:  time.Date(2026, 5, 30, 12, 1, 0, 0, time.UTC),
				Model:      "gpt-5",
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			modelContextWindow := int64(200000)

			if err := backend.FinishSession(ctx, sessionID, SessionFinish{
				CompletedAt:           time.Date(2026, 5, 30, 12, 5, 0, 0, time.UTC),
				Turns:                 2,
				InputTokens:           100,
				CachedInputTokens:     40,
				OutputTokens:          25,
				ReasoningOutputTokens: 7,
				TotalTokens:           125,
				ModelContextWindow:    &modelContextWindow,
				RuntimeSeconds:        240,
				FinalState:            "Human Review",
				Model:                 "gpt-5-resolved",
			}); err != nil {
				t.Fatalf("FinishSession() error = %v", err)
			}

			if err := backend.StopRun(ctx, runID, RunStop{
				StoppedAt:            time.Date(2026, 5, 30, 12, 5, 0, 0, time.UTC),
				RestartReason:        "complete",
				PeakConcurrentAgents: 3,
				SessionsLaunched:     1,
				InputTokens:          100,
				OutputTokens:         25,
				TotalTokens:          125,
				RuntimeSeconds:       240,
			}); err != nil {
				t.Fatalf("StopRun() error = %v", err)
			}

			run, err := backend.Queries().GetDetentRun(ctx, runID)
			if err != nil {
				t.Fatalf("GetDetentRun() error = %v", err)
			}
			if run.StartedAt != "2026-05-30T12:00:00Z" {
				t.Fatalf("run started_at = %q, want 2026-05-30T12:00:00Z", run.StartedAt)
			}
			if run.StoppedAt.String != "2026-05-30T12:05:00Z" {
				t.Fatalf("run stopped_at = %q, want 2026-05-30T12:05:00Z", run.StoppedAt.String)
			}
			if run.TotalTokens != 125 {
				t.Fatalf("run total_tokens = %d, want 125", run.TotalTokens)
			}

			session, err := backend.Queries().GetCodexSession(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetCodexSession() error = %v", err)
			}
			if session.RunID.Int64 != runID {
				t.Fatalf("session run_id = %d, want %d", session.RunID.Int64, runID)
			}
			if session.CompletedAt.String != "2026-05-30T12:05:00Z" {
				t.Fatalf("session completed_at = %q, want 2026-05-30T12:05:00Z", session.CompletedAt.String)
			}
			if session.FinalState.String != "Human Review" {
				t.Fatalf("session final_state = %q, want Human Review", session.FinalState.String)
			}
			if session.Model.String != "gpt-5-resolved" {
				t.Fatalf("session model = %q, want gpt-5-resolved", session.Model.String)
			}
			if !session.CachedInputTokens.Valid || session.CachedInputTokens.Int64 != 40 {
				t.Fatalf("session cached_input_tokens = %#v, want 40", session.CachedInputTokens)
			}
			if !session.ReasoningOutputTokens.Valid || session.ReasoningOutputTokens.Int64 != 7 {
				t.Fatalf("session reasoning_output_tokens = %#v, want 7", session.ReasoningOutputTokens)
			}
			if !session.ModelContextWindow.Valid || session.ModelContextWindow.Int64 != modelContextWindow {
				t.Fatalf("session model_context_window = %#v, want %d", session.ModelContextWindow, modelContextWindow)
			}

			spend, err := backend.DailyTokenSpend(ctx, time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("DailyTokenSpend() error = %v", err)
			}
			if spend.InputTokens != 100 || spend.CachedInputTokens != 40 || spend.OutputTokens != 25 || spend.ReasoningOutputTokens != 7 || spend.TotalTokens != 125 || spend.Sessions != 1 {
				t.Fatalf("DailyTokenSpend() = %#v", spend)
			}
			if len(spend.ByModel) != 1 || spend.ByModel[0].Model != "gpt-5-resolved" || spend.ByModel[0].CachedInputTokens != 40 || spend.ByModel[0].ReasoningOutputTokens != 7 {
				t.Fatalf("DailyTokenSpend().ByModel = %#v", spend.ByModel)
			}

			issueSpend, err := backend.IssueTokenSpend(ctx, IssueIdentity{IssueID: "I_kwDOSskuwc8AAAABD42c3Q"})
			if err != nil {
				t.Fatalf("IssueTokenSpend() error = %v", err)
			}
			if issueSpend.InputTokens != 100 || issueSpend.CachedInputTokens != 40 || issueSpend.OutputTokens != 25 || issueSpend.ReasoningOutputTokens != 7 || issueSpend.TotalTokens != 125 || issueSpend.Sessions != 1 {
				t.Fatalf("IssueTokenSpend() = %#v", issueSpend)
			}
			if len(issueSpend.ByModel) != 1 || issueSpend.ByModel[0].Model != "gpt-5-resolved" || issueSpend.ByModel[0].CachedInputTokens != 40 || issueSpend.ByModel[0].ReasoningOutputTokens != 7 {
				t.Fatalf("IssueTokenSpend().ByModel = %#v", issueSpend.ByModel)
			}

			identifierSpend, err := backend.IssueTokenSpend(ctx, IssueIdentity{Identifier: "digitaldrywood/detent#6"})
			if err != nil {
				t.Fatalf("IssueTokenSpend(identifier) error = %v", err)
			}
			if identifierSpend.TotalTokens != 125 {
				t.Fatalf("IssueTokenSpend(identifier).TotalTokens = %d, want 125", identifierSpend.TotalTokens)
			}

			urlSpend, err := backend.IssueTokenSpend(ctx, IssueIdentity{IssueURL: "https://github.com/digitaldrywood/detent/issues/6"})
			if err != nil {
				t.Fatalf("IssueTokenSpend(url) error = %v", err)
			}
			if urlSpend.TotalTokens != 125 {
				t.Fatalf("IssueTokenSpend(url).TotalTokens = %d, want 125", urlSpend.TotalTokens)
			}

			lifetime, err := backend.LifetimeTotals(ctx)
			if err != nil {
				t.Fatalf("LifetimeTotals() error = %v", err)
			}
			if lifetime.InputTokens != 100 || lifetime.CachedInputTokens != 40 || lifetime.OutputTokens != 25 || lifetime.ReasoningOutputTokens != 7 || lifetime.TotalTokens != 125 || lifetime.RuntimeSeconds != 240 {
				t.Fatalf("LifetimeTotals() token/runtime totals = %#v", lifetime)
			}
			if lifetime.Sessions != 1 || lifetime.Runs != 1 {
				t.Fatalf("LifetimeTotals() sessions/runs = %#v, want 1/1", lifetime)
			}
		})
	}
}

func TestLatestCompletedAgentResumeStateMatchesIssueBackendAndModel(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	startedAt := time.Date(2026, 7, 2, 17, 0, 0, 0, time.UTC)
	failedID, err := backend.StartSession(ctx, SessionStart{
		IssueID:          "issue-859",
		Identifier:       "digitaldrywood/detent#859",
		IssueURL:         "https://github.com/digitaldrywood/detent/issues/859",
		StartedAt:        startedAt,
		Model:            "gpt-5-codex",
		RequestedModel:   "gpt-5-codex",
		AgentBackendID:   "codex",
		AgentBackendKind: "codex",
		AgentRole:        "code",
	})
	if err != nil {
		t.Fatalf("StartSession(failed) error = %v", err)
	}
	if err := backend.FinishSession(ctx, failedID, SessionFinish{
		CompletedAt:       startedAt.Add(time.Minute),
		FinalState:        "failed",
		Model:             "gpt-5-codex",
		ProviderThreadID:  "thread-failed",
		ProviderSessionID: "session-failed",
	}); err != nil {
		t.Fatalf("FinishSession(failed) error = %v", err)
	}

	firstID, err := backend.StartSession(ctx, SessionStart{
		IssueID:          "issue-859",
		Identifier:       "digitaldrywood/detent#859",
		IssueURL:         "https://github.com/digitaldrywood/detent/issues/859",
		StartedAt:        startedAt.Add(2 * time.Minute),
		Model:            "gpt-5-codex",
		RequestedModel:   "gpt-5-codex",
		AgentBackendID:   "codex",
		AgentBackendKind: "codex",
		AgentRole:        "code",
	})
	if err != nil {
		t.Fatalf("StartSession(first) error = %v", err)
	}
	if err := backend.FinishSession(ctx, firstID, SessionFinish{
		CompletedAt:       startedAt.Add(3 * time.Minute),
		FinalState:        "completed",
		Model:             "gpt-5-codex-resolved",
		ProviderThreadID:  "thread-first",
		ProviderSessionID: "session-first",
	}); err != nil {
		t.Fatalf("FinishSession(first) error = %v", err)
	}

	secondID, err := backend.StartSession(ctx, SessionStart{
		IssueID:          "issue-859",
		Identifier:       "digitaldrywood/detent#859",
		IssueURL:         "https://github.com/digitaldrywood/detent/issues/859",
		StartedAt:        startedAt.Add(4 * time.Minute),
		Model:            "gpt-5-codex",
		RequestedModel:   "gpt-5-codex",
		AgentBackendID:   "codex",
		AgentBackendKind: "codex",
		AgentRole:        "code",
	})
	if err != nil {
		t.Fatalf("StartSession(second) error = %v", err)
	}
	if err := backend.FinishSession(ctx, secondID, SessionFinish{
		CompletedAt:          startedAt.Add(5 * time.Minute),
		FinalState:           "completed",
		Model:                "gpt-5-codex-resolved",
		ProviderThreadID:     "thread-second",
		ProviderSessionID:    "session-second",
		ResumedFromSessionID: firstID,
	}); err != nil {
		t.Fatalf("FinishSession(second) error = %v", err)
	}

	validatorID, err := backend.StartSession(ctx, SessionStart{
		IssueID:          "issue-859",
		Identifier:       "digitaldrywood/detent#859",
		IssueURL:         "https://github.com/digitaldrywood/detent/issues/859",
		StartedAt:        startedAt.Add(6 * time.Minute),
		Model:            "gpt-5-codex",
		RequestedModel:   "gpt-5-codex",
		AgentBackendID:   "codex",
		AgentBackendKind: "codex",
		AgentRole:        "validator",
	})
	if err != nil {
		t.Fatalf("StartSession(validator) error = %v", err)
	}
	if err := backend.FinishSession(ctx, validatorID, SessionFinish{
		CompletedAt:       startedAt.Add(7 * time.Minute),
		FinalState:        "completed",
		Model:             "gpt-5-codex-resolved",
		ProviderThreadID:  "thread-validator",
		ProviderSessionID: "session-validator",
	}); err != nil {
		t.Fatalf("FinishSession(validator) error = %v", err)
	}

	got, err := backend.LatestCompletedAgentResumeState(ctx, AgentResumeLookup{
		IssueID:          "issue-859",
		Identifier:       "digitaldrywood/detent#859",
		IssueURL:         "https://github.com/digitaldrywood/detent/issues/859",
		RequestedModel:   "gpt-5-codex",
		AgentBackendID:   "codex",
		AgentBackendKind: "codex",
		AgentRole:        "code",
	})
	if err != nil {
		t.Fatalf("LatestCompletedAgentResumeState() error = %v", err)
	}
	if got.DetentSessionID != secondID || got.ProviderThreadID != "thread-second" || got.ProviderSessionID != "session-second" {
		t.Fatalf("resume state = %#v, want newest completed second session", got)
	}
	if got.RequestedModel != "gpt-5-codex" || got.Model != "gpt-5-codex-resolved" || got.AgentRole != "code" {
		t.Fatalf("resume models = %#v, want requested and resolved model", got)
	}

	_, err = backend.LatestCompletedAgentResumeState(ctx, AgentResumeLookup{
		IssueID:          "issue-859",
		RequestedModel:   "gpt-5-codex-mini",
		AgentBackendID:   "codex",
		AgentBackendKind: "codex",
		AgentRole:        "code",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestCompletedAgentResumeState(model mismatch) error = %v, want ErrNotFound", err)
	}

	_, err = backend.LatestCompletedAgentResumeState(ctx, AgentResumeLookup{
		IssueID:          "issue-859",
		RequestedModel:   "gpt-5-codex",
		AgentBackendID:   "codex",
		AgentBackendKind: "codex",
		AgentRole:        "merge",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestCompletedAgentResumeState(role mismatch) error = %v, want ErrNotFound", err)
	}
}

func TestBudgetCostEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)

	events := []UsageEvent{
		{
			ProjectID:      "detent",
			Model:          "gpt-5",
			CostUSD:        1.25,
			StartedAt:      time.Date(2026, 6, 1, 5, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 5, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
		{
			ProjectID:      "pyroapex",
			Model:          "gpt-5",
			CostUSD:        3.5,
			StartedAt:      time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 6, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
		{
			ProjectID:      "detent",
			Model:          "gpt-5",
			CostUSD:        2.75,
			StartedAt:      time.Date(2026, 6, 1, 7, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 1, 7, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
		{
			ProjectID:      "detent",
			Model:          "gpt-5",
			CostUSD:        9,
			StartedAt:      time.Date(2026, 6, 2, 1, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 2, 1, 1, 0, 0, time.UTC),
			Outcome:        "completed",
			RuntimeSeconds: 60,
		},
	}

	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}

	got, err := backend.BudgetCostEvents(ctx, BudgetCostQuery{
		ProjectIDs: []string{"detent"},
		From:       time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		To:         time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BudgetCostEvents() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("BudgetCostEvents() len = %d, want 2: %#v", len(got), got)
	}
	if got[0].ProjectID != "detent" || got[0].CostUSD != 1.25 || got[1].CostUSD != 2.75 {
		t.Fatalf("BudgetCostEvents() = %#v, want detent costs in time order", got)
	}
}

func TestRecentModelTokenQuantilesUsesRecentCompletedSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	seedQuantileSession(t, ctx, backend, "gpt-5", base, 1_000_000, 100_000, 100_000)
	for index, inputTokens := range []int64{10, 20, 30, 40, 50} {
		seedQuantileSession(
			t,
			ctx,
			backend,
			"gpt-5",
			base.Add(time.Duration(index+1)*time.Minute),
			inputTokens,
			inputTokens/10,
			inputTokens/5,
		)
	}
	seedQuantileSession(t, ctx, backend, "other-model", base.Add(10*time.Minute), 9_000, 900, 900)
	if _, err := backend.StartSession(ctx, SessionStart{
		StartedAt: base.Add(11 * time.Minute),
		Model:     "gpt-5",
	}); err != nil {
		t.Fatalf("StartSession() incomplete error = %v", err)
	}

	quantiles, err := backend.RecentModelTokenQuantiles(ctx, ModelTokenQuantileQuery{
		Model: " GPT-5 ",
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("RecentModelTokenQuantiles() error = %v", err)
	}
	if quantiles.Sessions != 5 {
		t.Fatalf("Sessions = %d, want 5", quantiles.Sessions)
	}
	if quantiles.P50InputTokens != 30 || quantiles.P90InputTokens != 50 {
		t.Fatalf("input quantiles = p50 %d p90 %d, want 30/50", quantiles.P50InputTokens, quantiles.P90InputTokens)
	}
	if quantiles.P50CachedInputTokens != 3 || quantiles.P90CachedInputTokens != 5 {
		t.Fatalf("cached quantiles = p50 %d p90 %d, want 3/5", quantiles.P50CachedInputTokens, quantiles.P90CachedInputTokens)
	}
	if quantiles.P50OutputTokens != 6 || quantiles.P90OutputTokens != 10 {
		t.Fatalf("output quantiles = p50 %d p90 %d, want 6/10", quantiles.P50OutputTokens, quantiles.P90OutputTokens)
	}
	if quantiles.P50TotalTokens != 36 || quantiles.P90TotalTokens != 60 {
		t.Fatalf("total quantiles = p50 %d p90 %d, want 36/60", quantiles.P50TotalTokens, quantiles.P90TotalTokens)
	}

	empty, err := backend.RecentModelTokenQuantiles(ctx, ModelTokenQuantileQuery{Model: "missing", Limit: 5})
	if err != nil {
		t.Fatalf("RecentModelTokenQuantiles(missing) error = %v", err)
	}
	if empty.Sessions != 0 || empty.P90InputTokens != 0 {
		t.Fatalf("missing quantiles = %#v, want zero values", empty)
	}
}

func TestCycleTimeReportFromCompletedSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-215",
		Identifier:  "digitaldrywood/detent#215",
		StartedAt:   base.Add(-time.Hour),
		CompletedAt: base.Add(-30 * time.Minute),
		FinalState:  "failed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-215",
		Identifier:  "digitaldrywood/detent#215",
		StartedAt:   base,
		CompletedAt: base.Add(90 * time.Minute),
		FinalState:  "completed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-216",
		Identifier:  "digitaldrywood/detent#216",
		StartedAt:   base.Add(30 * time.Minute),
		CompletedAt: base.Add(2 * time.Hour),
		FinalState:  "failed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-215",
		Identifier:  "digitaldrywood/detent#215",
		StartedAt:   base.Add(2 * time.Hour),
		CompletedAt: base.Add(3 * time.Hour),
		FinalState:  "completed",
	})
	seedCycleSession(t, ctx, backend, cycleSessionSeed{
		IssueID:     "issue-217",
		Identifier:  "digitaldrywood/detent#217",
		StartedAt:   base.Add(-24 * time.Hour),
		CompletedAt: base.Add(24 * time.Hour),
		FinalState:  "Human Review",
	})

	report, err := backend.CycleTimeReport(ctx)
	if err != nil {
		t.Fatalf("CycleTimeReport() error = %v", err)
	}

	if len(report.Issues) != 2 {
		t.Fatalf("CycleTimeReport().Issues len = %d, want 2: %#v", len(report.Issues), report.Issues)
	}
	if report.Issues[0].Key != "digitaldrywood/detent#217" || report.Issues[0].DurationSeconds != int64(48*time.Hour/time.Second) {
		t.Fatalf("first issue = %#v, want #217 at 48h", report.Issues[0])
	}
	if report.Issues[1].Key != "digitaldrywood/detent#215" || report.Issues[1].DurationSeconds != int64(4*time.Hour/time.Second) || report.Issues[1].Sessions != 3 {
		t.Fatalf("second issue = %#v, want #215 at 4h across 3 sessions", report.Issues[1])
	}
	if report.AverageSeconds != int64((48*time.Hour+4*time.Hour)/2/time.Second) {
		t.Fatalf("AverageSeconds = %d, want 93600", report.AverageSeconds)
	}
	if len(report.Buckets) != 5 || report.Buckets[2].Count != 1 || report.Buckets[4].Count != 1 {
		t.Fatalf("Buckets = %#v, want counts in 4-8h and 1-3d", report.Buckets)
	}
}

func TestCycleTimeSeconds(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  int64
		ok    bool
	}{
		{name: "same instant is zero seconds", start: base, end: base, want: 0, ok: true},
		{name: "whole seconds between timestamps", start: base, end: base.Add(90*time.Minute + 12*time.Second), want: 5412, ok: true},
		{name: "missing start is invalid", end: base, ok: false},
		{name: "missing end is invalid", start: base, ok: false},
		{name: "end before start is invalid", start: base, end: base.Add(-time.Second), ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := cycleTimeSeconds(tt.start, tt.end)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("cycleTimeSeconds() = %d, %v; want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCycleTimeBuckets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		issues []CycleTimeIssue
		want   []CycleTimeBucket
	}{
		{name: "no durations returns no buckets"},
		{
			name: "assigns fixed lead time ranges and trims trailing empties",
			issues: []CycleTimeIssue{
				{Key: "fast", DurationSeconds: int64(30 * time.Minute / time.Second)},
				{Key: "medium", DurationSeconds: int64(2 * time.Hour / time.Second)},
				{Key: "same range", DurationSeconds: int64(3 * time.Hour / time.Second)},
				{Key: "slow", DurationSeconds: int64(9 * 24 * time.Hour / time.Second)},
			},
			want: []CycleTimeBucket{
				{Label: "<1h", MinSeconds: 0, MaxSeconds: 3600, Count: 1},
				{Label: "1-4h", MinSeconds: 3600, MaxSeconds: 14400, Count: 2},
				{Label: "4-8h", MinSeconds: 14400, MaxSeconds: 28800},
				{Label: "8-24h", MinSeconds: 28800, MaxSeconds: 86400},
				{Label: "1-3d", MinSeconds: 86400, MaxSeconds: 259200},
				{Label: "3-7d", MinSeconds: 259200, MaxSeconds: 604800},
				{Label: "7d+", MinSeconds: 604800, Count: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := cycleTimeBuckets(tt.issues)
			if len(got) != len(tt.want) {
				t.Fatalf("cycleTimeBuckets() len = %d, want %d: %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("bucket %d = %#v, want %#v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestWorkflowMetricsStoreRoundTripAndAggregates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	events := []WorkflowPhaseEvent{
		{
			ProjectID:         " detent ",
			IssueID:           " issue-722 ",
			Identifier:        " digitaldrywood/detent#722 ",
			IssueURL:          " https://github.com/digitaldrywood/detent/issues/722 ",
			PhaseType:         WorkflowPhaseTypeLane,
			PhaseName:         " In Progress ",
			PreviousPhaseName: "Todo",
			Status:            " exited ",
			StartedAt:         base,
			FinishedAt:        base.Add(10 * time.Minute),
			Reason:            "transition_to:Human Review",
		},
		{
			ProjectID:  "detent",
			IssueID:    "issue-723",
			Identifier: "digitaldrywood/detent#723",
			PhaseType:  WorkflowPhaseTypeLane,
			PhaseName:  "In Progress",
			Status:     "exited",
			StartedAt:  base.Add(time.Hour),
			FinishedAt: base.Add(time.Hour + 20*time.Minute),
		},
		{
			ProjectID:      "detent",
			IssueID:        "issue-722",
			Identifier:     "digitaldrywood/detent#722",
			PhaseType:      WorkflowPhaseTypeAgentSession,
			PhaseName:      "agent_active",
			Status:         "completed",
			StartedAt:      base.Add(time.Minute),
			FinishedAt:     base.Add(9 * time.Minute),
			Turns:          3,
			InputTokens:    1000,
			OutputTokens:   250,
			TotalTokens:    1250,
			MetadataJSON:   `{"session_id":42}`,
			EndpointFamily: "codex",
		},
	}
	for _, event := range events {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
		}
	}

	report, err := backend.WorkflowMetricsReport(ctx, WorkflowMetricsQuery{
		ProjectID: "detent",
		From:      base.Add(-time.Minute),
		To:        base.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WorkflowMetricsReport() error = %v", err)
	}

	if len(report.Lanes) != 1 {
		t.Fatalf("WorkflowMetricsReport().Lanes len = %d, want 1: %#v", len(report.Lanes), report.Lanes)
	}
	lane := report.Lanes[0]
	if lane.ProjectID != "detent" || lane.PhaseName != "In Progress" || lane.Count != 2 {
		t.Fatalf("lane metric = %#v, want detent In Progress count 2", lane)
	}
	if lane.AverageSeconds != 900 || lane.P50Seconds != 600 || lane.P90Seconds != 1200 || lane.P95Seconds != 1200 {
		t.Fatalf("lane durations = %#v, want average 900 p50 600 p90/p95 1200", lane)
	}

	if len(report.SubPhases) != 1 {
		t.Fatalf("WorkflowMetricsReport().SubPhases len = %d, want 1: %#v", len(report.SubPhases), report.SubPhases)
	}
	subphase := report.SubPhases[0]
	if subphase.PhaseType != string(WorkflowPhaseTypeAgentSession) || subphase.PhaseName != "agent_active" || subphase.TotalSeconds != 480 {
		t.Fatalf("subphase metric = %#v, want 480s agent_active", subphase)
	}

	timeline, err := backend.IssueWorkflowTimeline(ctx, IssueIdentity{Identifier: "digitaldrywood/detent#722"})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	if len(timeline.Events) != 2 {
		t.Fatalf("IssueWorkflowTimeline().Events len = %d, want 2: %#v", len(timeline.Events), timeline.Events)
	}
	if timeline.Events[0].ProjectID != "detent" || timeline.Events[0].PhaseName != "In Progress" || timeline.Events[0].DurationSeconds != 600 {
		t.Fatalf("timeline first event = %#v, want normalized In Progress lane", timeline.Events[0])
	}
	if timeline.Events[1].Turns != 3 || timeline.Events[1].TotalTokens != 1250 || timeline.Events[1].MetadataJSON != `{"session_id":42}` {
		t.Fatalf("timeline agent event = %#v, want turns/tokens/metadata", timeline.Events[1])
	}
}

func TestWorkflowMetricsReportComputesLaneFlowEfficiency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		events        []WorkflowPhaseEvent
		phaseName     string
		activeSeconds int64
		waitSeconds   int64
		activePercent float64
	}{
		{
			name:      "splits active intervals from uncovered lane time",
			phaseName: "In Progress",
			events: []WorkflowPhaseEvent{
				workflowMetricTestEvent("detent", "issue-1", WorkflowPhaseTypeLane, "In Progress", 0, 10*time.Minute),
				workflowMetricTestEvent("detent", "issue-1", WorkflowPhaseTypeAgentSession, "agent_active", 2*time.Minute, 2*time.Minute),
				workflowMetricTestEvent("detent", "issue-1", WorkflowPhaseTypeCI, "ci", 5*time.Minute, 3*time.Minute),
			},
			activeSeconds: 300,
			waitSeconds:   300,
			activePercent: 50,
		},
		{
			name:      "caps overlapping active intervals at their union",
			phaseName: "Rework",
			events: []WorkflowPhaseEvent{
				workflowMetricTestEvent("detent", "issue-2", WorkflowPhaseTypeLane, "Rework", 0, 10*time.Minute),
				workflowMetricTestEvent("detent", "issue-2", WorkflowPhaseTypeAgentSession, "agent_active", time.Minute, 5*time.Minute),
				workflowMetricTestEvent("detent", "issue-2", WorkflowPhaseTypeLocalCheck, "make check", 4*time.Minute, 5*time.Minute),
			},
			activeSeconds: 480,
			waitSeconds:   120,
			activePercent: 80,
		},
		{
			name:      "treats explicit wait and unrelated work as wait",
			phaseName: "Merging",
			events: []WorkflowPhaseEvent{
				workflowMetricTestEvent("detent", "issue-3", WorkflowPhaseTypeLane, "Merging", 0, 10*time.Minute),
				workflowMetricTestEvent("detent", "issue-3", WorkflowPhaseTypeGitHubBackoff, "github_backoff", time.Minute, 4*time.Minute),
				workflowMetricTestEvent("detent", "issue-3", WorkflowPhaseTypeCI, "ci", 7*time.Minute, 2*time.Minute),
				workflowMetricTestEvent("detent", "issue-other", WorkflowPhaseTypeAgentSession, "agent_active", time.Minute, 8*time.Minute),
			},
			activeSeconds: 120,
			waitSeconds:   480,
			activePercent: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			backend := openTestStore(t, ctx)
			for _, event := range tt.events {
				if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
					t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
				}
			}

			report, err := backend.WorkflowMetricsReport(ctx, WorkflowMetricsQuery{
				ProjectID: "detent",
				From:      workflowMetricTestBase.Add(-time.Minute),
				To:        workflowMetricTestBase.Add(time.Hour),
			})
			if err != nil {
				t.Fatalf("WorkflowMetricsReport() error = %v", err)
			}

			lane := workflowMetricTestLane(t, report.Lanes, tt.phaseName)
			if lane.ActiveSeconds != tt.activeSeconds || lane.WaitSeconds != tt.waitSeconds {
				t.Fatalf("%s active/wait = %d/%d, want %d/%d", tt.phaseName, lane.ActiveSeconds, lane.WaitSeconds, tt.activeSeconds, tt.waitSeconds)
			}
			if math.Abs(lane.ActivePercent-tt.activePercent) > 0.01 {
				t.Fatalf("%s active percent = %.2f, want %.2f", tt.phaseName, lane.ActivePercent, tt.activePercent)
			}
		})
	}
}

func TestWorkflowMetricsReportIncludesFlowActiveEventsAcrossWindowBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		lane          WorkflowPhaseEvent
		active        WorkflowPhaseEvent
		from          time.Time
		to            time.Time
		activeSeconds int64
		waitSeconds   int64
	}{
		{
			name:          "active event finished before report window",
			lane:          workflowMetricTestEvent("detent", "issue-boundary-before", WorkflowPhaseTypeLane, "In Progress", 0, 10*time.Minute),
			active:        workflowMetricTestEvent("detent", "issue-boundary-before", WorkflowPhaseTypeAgentSession, "agent_active", 2*time.Minute, 2*time.Minute),
			from:          workflowMetricTestBase.Add(9 * time.Minute),
			to:            workflowMetricTestBase.Add(20 * time.Minute),
			activeSeconds: 120,
			waitSeconds:   480,
		},
		{
			name:          "active event finished after report window",
			lane:          workflowMetricTestEvent("detent", "issue-boundary-after", WorkflowPhaseTypeLane, "Merging", 8*time.Minute, 4*time.Minute),
			active:        workflowMetricTestEvent("detent", "issue-boundary-after", WorkflowPhaseTypeCI, "ci", 11*time.Minute, 3*time.Minute),
			from:          workflowMetricTestBase.Add(9 * time.Minute),
			to:            workflowMetricTestBase.Add(13 * time.Minute),
			activeSeconds: 60,
			waitSeconds:   180,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			backend := openTestStore(t, ctx)
			for _, event := range []WorkflowPhaseEvent{tt.lane, tt.active} {
				if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
					t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
				}
			}

			report, err := backend.WorkflowMetricsReport(ctx, WorkflowMetricsQuery{
				ProjectID: "detent",
				From:      tt.from,
				To:        tt.to,
			})
			if err != nil {
				t.Fatalf("WorkflowMetricsReport() error = %v", err)
			}

			lane := workflowMetricTestLane(t, report.Lanes, tt.lane.PhaseName)
			if lane.ActiveSeconds != tt.activeSeconds || lane.WaitSeconds != tt.waitSeconds {
				t.Fatalf("%s active/wait = %d/%d, want %d/%d", tt.lane.PhaseName, lane.ActiveSeconds, lane.WaitSeconds, tt.activeSeconds, tt.waitSeconds)
			}
			if len(report.SubPhases) != 0 {
				t.Fatalf("WorkflowMetricsReport().SubPhases len = %d, want 0: %#v", len(report.SubPhases), report.SubPhases)
			}
		})
	}
}

func TestWorkflowMetricsReportIncludesRepresentativeRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	events := []WorkflowPhaseEvent{
		workflowMetricTestEvent("detent", "issue-1", WorkflowPhaseTypeLane, "In Progress", 0, 10*time.Minute),
		workflowMetricTestEvent("detent", "issue-1", WorkflowPhaseTypeAgentSession, "agent_active", time.Minute, 2*time.Minute),
		workflowMetricTestEvent("detent", "issue-2", WorkflowPhaseTypeLane, "In Progress", 10*time.Minute, 10*time.Minute),
		workflowMetricTestEvent("detent", "issue-2", WorkflowPhaseTypeAgentSession, "agent_active", 12*time.Minute, 2*time.Minute),
		workflowMetricTestEvent("detent", "issue-3", WorkflowPhaseTypeLane, "In Progress", 20*time.Minute, 10*time.Minute),
		workflowMetricTestEvent("detent", "issue-3", WorkflowPhaseTypeAgentSession, "agent_active", 22*time.Minute, 2*time.Minute),
		workflowMetricTestEvent("detent", "issue-4", WorkflowPhaseTypeLane, "In Progress", 30*time.Minute, 10*time.Minute),
		workflowMetricTestEvent("detent", "issue-4", WorkflowPhaseTypeAgentSession, "agent_active", 32*time.Minute, 2*time.Minute),
		workflowMetricTestEvent("detent", "issue-other", WorkflowPhaseTypeAgentSession, "agent_active", 35*time.Minute, 2*time.Minute),
	}
	for i := range events {
		if events[i].PhaseType == WorkflowPhaseTypeAgentSession {
			events[i].RunID = 100 + int64(i)
			events[i].SessionID = 200 + int64(i)
			events[i].TotalTokens = 1_000 + int64(i)
		}
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, events[i]); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
		}
	}

	report, err := backend.WorkflowMetricsReport(ctx, WorkflowMetricsQuery{
		ProjectID: "detent",
		From:      workflowMetricTestBase.Add(-time.Minute),
		To:        workflowMetricTestBase.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("WorkflowMetricsReport() error = %v", err)
	}

	lane := workflowMetricTestLane(t, report.Lanes, "In Progress")
	if len(lane.Representatives) != 3 {
		t.Fatalf("Representatives len = %d, want 3: %#v", len(lane.Representatives), lane.Representatives)
	}
	wantRunIDs := []int64{107, 105, 103}
	for i, want := range wantRunIDs {
		if lane.Representatives[i].RunID != want {
			t.Fatalf("Representatives[%d].RunID = %d, want %d: %#v", i, lane.Representatives[i].RunID, want, lane.Representatives)
		}
	}
	if lane.Representatives[0].Identifier != "digitaldrywood/detent#4" || lane.Representatives[0].SessionID != 207 {
		t.Fatalf("Representatives[0] = %#v, want issue-4 active session", lane.Representatives[0])
	}
}

func TestWorkflowMetricsReportBuildsTrackedLaneTrends(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	events := []WorkflowPhaseEvent{
		workflowMetricTestEvent("detent", "issue-1", WorkflowPhaseTypeLane, "In Progress", 10*time.Minute, 2*time.Minute),
		workflowMetricTestEvent("detent", "issue-2", WorkflowPhaseTypeLane, "In Progress", 70*time.Minute, 4*time.Minute),
		workflowMetricTestEvent("detent", "issue-3", WorkflowPhaseTypeLane, "Human Review", 130*time.Minute, 6*time.Minute),
		workflowMetricTestEvent("detent", "issue-4", WorkflowPhaseTypeLane, "Merging", 190*time.Minute, 8*time.Minute),
		workflowMetricTestEvent("detent", "issue-5", WorkflowPhaseTypeLane, "Rework", 250*time.Minute, 10*time.Minute),
		workflowMetricTestEvent("detent", "issue-6", WorkflowPhaseTypeLane, "Todo", 310*time.Minute, 12*time.Minute),
	}
	for _, event := range events {
		if _, err := backend.RecordWorkflowPhaseEvent(ctx, event); err != nil {
			t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
		}
	}

	report, err := backend.WorkflowMetricsReport(ctx, WorkflowMetricsQuery{
		ProjectID: "detent",
		From:      workflowMetricTestBase,
		To:        workflowMetricTestBase.Add(8 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WorkflowMetricsReport() error = %v", err)
	}

	if len(report.LaneTrends) != 4 {
		t.Fatalf("LaneTrends len = %d, want 4: %#v", len(report.LaneTrends), report.LaneTrends)
	}
	for _, phaseName := range []string{"In Progress", "Human Review", "Merging", "Rework"} {
		trend := workflowMetricTestTrend(t, report.LaneTrends, phaseName)
		if len(trend.Points) != 8 {
			t.Fatalf("%s trend points len = %d, want 8", phaseName, len(trend.Points))
		}
	}
	for _, trend := range report.LaneTrends {
		if trend.PhaseName == "Todo" {
			t.Fatalf("LaneTrends included Todo: %#v", report.LaneTrends)
		}
	}
}

var workflowMetricTestBase = time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)

func workflowMetricTestEvent(projectID string, issueID string, phaseType WorkflowPhaseType, phaseName string, offset time.Duration, duration time.Duration) WorkflowPhaseEvent {
	startedAt := workflowMetricTestBase.Add(offset)
	return WorkflowPhaseEvent{
		ProjectID:       projectID,
		IssueID:         issueID,
		Identifier:      "digitaldrywood/detent#" + strings.TrimPrefix(issueID, "issue-"),
		IssueURL:        "https://github.com/digitaldrywood/detent/issues/" + strings.TrimPrefix(issueID, "issue-"),
		PhaseType:       phaseType,
		PhaseName:       phaseName,
		Status:          "completed",
		StartedAt:       startedAt,
		FinishedAt:      startedAt.Add(duration),
		DurationSeconds: int64(duration / time.Second),
	}
}

func workflowMetricTestLane(t *testing.T, lanes []WorkflowPhaseMetric, phaseName string) WorkflowPhaseMetric {
	t.Helper()
	for _, lane := range lanes {
		if lane.PhaseName == phaseName {
			return lane
		}
	}
	t.Fatalf("missing lane %q in %#v", phaseName, lanes)
	return WorkflowPhaseMetric{}
}

func workflowMetricTestTrend(t *testing.T, trends []WorkflowLaneTrend, phaseName string) WorkflowLaneTrend {
	t.Helper()
	for _, trend := range trends {
		if trend.PhaseName == phaseName {
			return trend
		}
	}
	t.Fatalf("missing trend %q in %#v", phaseName, trends)
	return WorkflowLaneTrend{}
}

func TestFairShareStoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	dispatchedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	if err := backend.RecordFairShareDispatch(ctx, FairShareDispatch{
		ProjectID:      " alpha ",
		Weight:         2,
		RuntimeSeconds: 30,
		DispatchedAt:   dispatchedAt,
	}); err != nil {
		t.Fatalf("RecordFairShareDispatch() first error = %v", err)
	}
	if err := backend.RecordFairShareDispatch(ctx, FairShareDispatch{
		ProjectID:      "alpha",
		Weight:         2,
		RuntimeSeconds: 45,
		DispatchedAt:   dispatchedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordFairShareDispatch() second error = %v", err)
	}

	usage, err := backend.ListFairShareUsage(ctx)
	if err != nil {
		t.Fatalf("ListFairShareUsage() error = %v", err)
	}
	if len(usage) != 1 {
		t.Fatalf("usage len = %d, want 1: %#v", len(usage), usage)
	}

	got := usage[0]
	if got.ProjectID != "alpha" {
		t.Fatalf("ProjectID = %q, want alpha", got.ProjectID)
	}
	if got.Weight != 2 {
		t.Fatalf("Weight = %d, want 2", got.Weight)
	}
	if got.Dispatches != 2 {
		t.Fatalf("Dispatches = %d, want 2", got.Dispatches)
	}
	if got.RuntimeSeconds != 75 {
		t.Fatalf("RuntimeSeconds = %d, want 75", got.RuntimeSeconds)
	}
	if !got.UpdatedAt.Equal(dispatchedAt.Add(time.Minute)) {
		t.Fatalf("UpdatedAt = %s, want %s", got.UpdatedAt, dispatchedAt.Add(time.Minute))
	}
}

func TestUsageLedgerRoundTrip(t *testing.T) {
	t.Parallel()

	modelContextWindow := int64(128000)
	tests := []struct {
		name  string
		event UsageEvent
	}{
		{
			name: "persists usage event across reopen",
			event: UsageEvent{
				ProjectID:             " detent ",
				RunID:                 11,
				SessionID:             42,
				IssueID:               " I_kwDOSskuwc8AAAABD6psJQ ",
				Identifier:            " digitaldrywood/detent#117 ",
				PRNumber:              int64Ptr(91),
				Model:                 " gpt-5-codex ",
				InputTokens:           123,
				CachedInputTokens:     67,
				OutputTokens:          45,
				ReasoningOutputTokens: 9,
				TotalTokens:           168,
				ModelContextWindow:    &modelContextWindow,
				CostUSD:               0.00123,
				RuntimeSeconds:        73,
				StartedAt:             time.Date(2026, 5, 31, 13, 0, 0, 0, time.UTC),
				FinishedAt:            time.Date(2026, 5, 31, 13, 1, 13, 0, time.UTC),
				Outcome:               " completed ",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "detent.db")

			backend, err := Open(ctx, Config{
				Backend: BackendSQLite,
				Path:    dbPath,
			})
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}

			eventID, err := backend.RecordUsageEvent(ctx, tt.event)
			if err != nil {
				t.Fatalf("RecordUsageEvent() error = %v", err)
			}

			got, err := backend.Queries().GetUsageEvent(ctx, eventID)
			if err != nil {
				t.Fatalf("GetUsageEvent() error = %v", err)
			}
			if got.ProjectID != "detent" {
				t.Fatalf("ProjectID = %q, want detent", got.ProjectID)
			}
			if got.RunID.Int64 != 11 || got.SessionID.Int64 != 42 {
				t.Fatalf("run/session = %d/%d, want 11/42", got.RunID.Int64, got.SessionID.Int64)
			}
			if got.IssueID.String != "I_kwDOSskuwc8AAAABD6psJQ" || got.Identifier.String != "digitaldrywood/detent#117" {
				t.Fatalf("issue identity = %q/%q", got.IssueID.String, got.Identifier.String)
			}
			if got.PrNumber.Int64 != 91 {
				t.Fatalf("pr_number = %d, want 91", got.PrNumber.Int64)
			}
			if got.Model != "gpt-5-codex" {
				t.Fatalf("model = %q, want gpt-5-codex", got.Model)
			}
			if got.InputTokens != 123 || got.OutputTokens != 45 || got.TotalTokens != 168 || got.RuntimeSeconds != 73 {
				t.Fatalf("tokens/runtime = %d/%d/%d/%d", got.InputTokens, got.OutputTokens, got.TotalTokens, got.RuntimeSeconds)
			}
			if !got.CachedInputTokens.Valid || got.CachedInputTokens.Int64 != 67 {
				t.Fatalf("cached_input_tokens = %#v, want 67", got.CachedInputTokens)
			}
			if !got.ReasoningOutputTokens.Valid || got.ReasoningOutputTokens.Int64 != 9 {
				t.Fatalf("reasoning_output_tokens = %#v, want 9", got.ReasoningOutputTokens)
			}
			if !got.ModelContextWindow.Valid || got.ModelContextWindow.Int64 != modelContextWindow {
				t.Fatalf("model_context_window = %#v, want %d", got.ModelContextWindow, modelContextWindow)
			}
			if got.CostUsd != 0.00123 {
				t.Fatalf("cost_usd = %.12f, want 0.001230000000", got.CostUsd)
			}
			if got.StartedAt != "2026-05-31T13:00:00Z" || got.FinishedAt != "2026-05-31T13:01:13Z" {
				t.Fatalf("timestamps = %q/%q", got.StartedAt, got.FinishedAt)
			}
			if got.EventDay != "2026-05-31" {
				t.Fatalf("event_day = %q, want 2026-05-31", got.EventDay)
			}
			if got.Outcome != "completed" {
				t.Fatalf("outcome = %q, want completed", got.Outcome)
			}

			if err := backend.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			reopened, err := Open(ctx, Config{
				Backend: BackendSQLite,
				Path:    dbPath,
			})
			if err != nil {
				t.Fatalf("reopen Open() error = %v", err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Fatalf("reopened Close() error = %v", err)
				}
			})

			persisted, err := reopened.Queries().GetUsageEvent(ctx, eventID)
			if err != nil {
				t.Fatalf("GetUsageEvent() after reopen error = %v", err)
			}
			if persisted.TotalTokens != 168 {
				t.Fatalf("persisted total_tokens = %d, want 168", persisted.TotalTokens)
			}
			if !persisted.CachedInputTokens.Valid || persisted.CachedInputTokens.Int64 != 67 {
				t.Fatalf("persisted cached_input_tokens = %#v, want 67", persisted.CachedInputTokens)
			}
			if !persisted.ReasoningOutputTokens.Valid || persisted.ReasoningOutputTokens.Int64 != 9 {
				t.Fatalf("persisted reasoning_output_tokens = %#v, want 9", persisted.ReasoningOutputTokens)
			}
			if !persisted.ModelContextWindow.Valid || persisted.ModelContextWindow.Int64 != modelContextWindow {
				t.Fatalf("persisted model_context_window = %#v, want %d", persisted.ModelContextWindow, modelContextWindow)
			}
			if persisted.CostUsd != 0.00123 {
				t.Fatalf("persisted cost_usd = %.12f, want 0.001230000000", persisted.CostUsd)
			}
		})
	}
}

func TestUsageReportAggregates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	seedUsageReportEvents(t, ctx, backend)

	tests := []struct {
		name  string
		query UsageReportQuery
		want  []UsageReportRow
	}{
		{
			name:  "by day with inclusive range",
			query: UsageReportQuery{By: UsageReportByDay, From: dateOnly(2026, 5, 31), To: dateOnly(2026, 6, 1)},
			want: []UsageReportRow{
				{
					Key:                   "2026-05-31",
					InputTokens:           150,
					CachedInputTokens:     45,
					OutputTokens:          75,
					ReasoningOutputTokens: 15,
					TotalTokens:           225,
					ModelContextWindow:    200000,
					RuntimeSeconds:        45,
					Events:                2,
				},
				{
					Key:                   "2026-06-01",
					InputTokens:           70,
					CachedInputTokens:     70,
					OutputTokens:          30,
					ReasoningOutputTokens: 12,
					TotalTokens:           100,
					ModelContextWindow:    100000,
					RuntimeSeconds:        25,
					Events:                1,
				},
			},
		},
		{
			name:  "by project",
			query: UsageReportQuery{By: UsageReportByProject, From: dateOnly(2026, 5, 31), To: dateOnly(2026, 6, 1)},
			want: []UsageReportRow{
				{
					Key:                   "detent",
					InputTokens:           220,
					CachedInputTokens:     115,
					OutputTokens:          105,
					ReasoningOutputTokens: 27,
					TotalTokens:           325,
					ModelContextWindow:    200000,
					RuntimeSeconds:        70,
					Events:                3,
				},
			},
		},
		{
			name:  "by issue",
			query: UsageReportQuery{By: UsageReportByIssue},
			want: []UsageReportRow{
				{
					Key:                   "digitaldrywood/detent#117",
					InputTokens:           100,
					CachedInputTokens:     30,
					OutputTokens:          50,
					ReasoningOutputTokens: 10,
					TotalTokens:           150,
					ModelContextWindow:    200000,
					RuntimeSeconds:        30,
					Events:                1,
				},
				{
					Key:                   "digitaldrywood/detent#119",
					InputTokens:           120,
					CachedInputTokens:     85,
					OutputTokens:          55,
					ReasoningOutputTokens: 17,
					TotalTokens:           175,
					ModelContextWindow:    128000,
					RuntimeSeconds:        40,
					Events:                2,
				},
				{
					Key:            "unassigned",
					InputTokens:    5,
					OutputTokens:   2,
					TotalTokens:    7,
					RuntimeSeconds: 3,
					Events:         1,
				},
			},
		},
		{
			name:  "by PR",
			query: UsageReportQuery{By: UsageReportByPR},
			want: []UsageReportRow{
				{
					Key:                   "detent#133",
					InputTokens:           100,
					CachedInputTokens:     30,
					OutputTokens:          50,
					ReasoningOutputTokens: 10,
					TotalTokens:           150,
					ModelContextWindow:    200000,
					RuntimeSeconds:        30,
					Events:                1,
				},
				{
					Key:                   "detent#141",
					InputTokens:           120,
					CachedInputTokens:     85,
					OutputTokens:          55,
					ReasoningOutputTokens: 17,
					TotalTokens:           175,
					ModelContextWindow:    128000,
					RuntimeSeconds:        40,
					Events:                2,
				},
				{
					Key:            "pyroapex#141",
					InputTokens:    5,
					OutputTokens:   2,
					TotalTokens:    7,
					RuntimeSeconds: 3,
					Events:         1,
				},
			},
		},
		{
			name:  "by model",
			query: UsageReportQuery{By: UsageReportByModel, From: dateOnly(2026, 5, 31), To: dateOnly(2026, 6, 1)},
			want: []UsageReportRow{
				{
					Key:                   "gpt-5.4",
					InputTokens:           150,
					CachedInputTokens:     45,
					OutputTokens:          75,
					ReasoningOutputTokens: 15,
					TotalTokens:           225,
					ModelContextWindow:    200000,
					RuntimeSeconds:        45,
					Events:                2,
				},
				{
					Key:                   "gpt-5.4-mini",
					InputTokens:           70,
					CachedInputTokens:     70,
					OutputTokens:          30,
					ReasoningOutputTokens: 12,
					TotalTokens:           100,
					ModelContextWindow:    100000,
					RuntimeSeconds:        25,
					Events:                1,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report, err := backend.UsageReport(ctx, tt.query)
			if err != nil {
				t.Fatalf("UsageReport() error = %v", err)
			}
			assertUsageRows(t, report.Rows, tt.want)
		})
	}
}

func TestUsageReportRejectsInvalidRange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)

	_, err := backend.UsageReport(ctx, UsageReportQuery{
		By:   UsageReportByDay,
		From: dateOnly(2026, 6, 2),
		To:   dateOnly(2026, 6, 1),
	})
	if err == nil {
		t.Fatal("UsageReport() error = nil, want invalid date range")
	}
}

func TestOpenRejectsUnsupportedBackend(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), Config{
		Backend: Backend("postgres"),
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err == nil {
		t.Fatal("Open() error = nil, want unsupported backend error")
	}
}

func TestOpenUsesSQLiteBackendByDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Path:        filepath.Join(t.TempDir(), "detent.db"),
		BusyTimeout: 2500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	sqliteBackend, ok := backend.(*sqliteStore)
	if !ok {
		t.Fatalf("Open() returned %T, want *sqliteStore", backend)
	}
	if got := queryInt(t, sqliteBackend.db, "PRAGMA busy_timeout"); got != 2500 {
		t.Fatalf("busy_timeout = %d, want 2500", got)
	}
}

func TestOpenSQLiteRejectsMissingPath(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), Config{
		Backend: BackendSQLite,
	})
	if err == nil {
		t.Fatal("Open() error = nil, want missing path error")
	}
}

func TestBusyTimeoutMillis(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		want    int64
	}{
		{name: "default for zero", timeout: 0, want: 5000},
		{name: "default for negative", timeout: -time.Second, want: 5000},
		{name: "minimum positive", timeout: time.Nanosecond, want: 1},
		{name: "configured duration", timeout: 3 * time.Second, want: 3000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := busyTimeoutMillis(tt.timeout); got != tt.want {
				t.Fatalf("busyTimeoutMillis(%s) = %d, want %d", tt.timeout, got, tt.want)
			}
		})
	}
}

func openTestStore(t *testing.T, ctx context.Context) Store {
	t.Helper()

	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return backend
}

type cycleSessionSeed struct {
	IssueID     string
	Identifier  string
	StartedAt   time.Time
	CompletedAt time.Time
	FinalState  string
}

func seedCycleSession(t *testing.T, ctx context.Context, backend Store, seed cycleSessionSeed) {
	t.Helper()

	sessionID, err := backend.StartSession(ctx, SessionStart{
		IssueID:    seed.IssueID,
		Identifier: seed.Identifier,
		StartedAt:  seed.StartedAt,
		Model:      "gpt-5-codex",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := backend.FinishSession(ctx, sessionID, SessionFinish{
		CompletedAt:    seed.CompletedAt,
		RuntimeSeconds: int64(seed.CompletedAt.Sub(seed.StartedAt) / time.Second),
		FinalState:     seed.FinalState,
		Model:          "gpt-5-codex",
	}); err != nil {
		t.Fatalf("FinishSession() error = %v", err)
	}
}

func seedQuantileSession(
	t *testing.T,
	ctx context.Context,
	backend Store,
	model string,
	completedAt time.Time,
	inputTokens int64,
	cachedInputTokens int64,
	outputTokens int64,
) {
	t.Helper()

	sessionID, err := backend.StartSession(ctx, SessionStart{
		StartedAt: completedAt.Add(-time.Minute),
		Model:     model,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if err := backend.FinishSession(ctx, sessionID, SessionFinish{
		CompletedAt:       completedAt,
		InputTokens:       inputTokens,
		CachedInputTokens: cachedInputTokens,
		OutputTokens:      outputTokens,
		TotalTokens:       inputTokens + outputTokens,
		FinalState:        "completed",
		Model:             model,
	}); err != nil {
		t.Fatalf("FinishSession() error = %v", err)
	}
}

func seedUsageReportEvents(t *testing.T, ctx context.Context, backend Store) {
	t.Helper()

	contextWindow200K := int64(200000)
	contextWindow128K := int64(128000)
	contextWindow100K := int64(100000)
	events := []UsageEvent{
		{
			ProjectID:             "detent",
			IssueID:               "issue-117",
			Identifier:            "digitaldrywood/detent#117",
			PRNumber:              int64Ptr(133),
			Model:                 "gpt-5.4",
			InputTokens:           100,
			CachedInputTokens:     30,
			OutputTokens:          50,
			ReasoningOutputTokens: 10,
			TotalTokens:           150,
			ModelContextWindow:    &contextWindow200K,
			RuntimeSeconds:        30,
			StartedAt:             time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC),
			FinishedAt:            time.Date(2026, 5, 31, 9, 1, 0, 0, time.UTC),
			Outcome:               "completed",
		},
		{
			ProjectID:             "detent",
			IssueID:               "issue-119",
			Identifier:            "digitaldrywood/detent#119",
			PRNumber:              int64Ptr(141),
			Model:                 "gpt-5.4",
			InputTokens:           50,
			CachedInputTokens:     15,
			OutputTokens:          25,
			ReasoningOutputTokens: 5,
			TotalTokens:           75,
			ModelContextWindow:    &contextWindow128K,
			RuntimeSeconds:        15,
			StartedAt:             time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC),
			FinishedAt:            time.Date(2026, 5, 31, 10, 1, 0, 0, time.UTC),
			Outcome:               "completed",
		},
		{
			ProjectID:             "detent",
			IssueID:               "issue-119",
			Identifier:            "digitaldrywood/detent#119",
			PRNumber:              int64Ptr(141),
			Model:                 "gpt-5.4-mini",
			InputTokens:           70,
			CachedInputTokens:     70,
			OutputTokens:          30,
			ReasoningOutputTokens: 12,
			TotalTokens:           100,
			ModelContextWindow:    &contextWindow100K,
			RuntimeSeconds:        25,
			StartedAt:             time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
			FinishedAt:            time.Date(2026, 6, 1, 11, 1, 0, 0, time.UTC),
			Outcome:               "completed",
		},
		{
			ProjectID:      "pyroapex",
			PRNumber:       int64Ptr(141),
			Model:          "",
			InputTokens:    5,
			OutputTokens:   2,
			TotalTokens:    7,
			RuntimeSeconds: 3,
			StartedAt:      time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC),
			FinishedAt:     time.Date(2026, 6, 2, 12, 1, 0, 0, time.UTC),
			Outcome:        "completed",
		},
	}

	for _, event := range events {
		if _, err := backend.RecordUsageEvent(ctx, event); err != nil {
			t.Fatalf("RecordUsageEvent() error = %v", err)
		}
	}
}

func assertUsageRows(t *testing.T, got []UsageReportRow, want []UsageReportRow) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("rows len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Key != want[i].Key ||
			got[i].InputTokens != want[i].InputTokens ||
			got[i].CachedInputTokens != want[i].CachedInputTokens ||
			got[i].OutputTokens != want[i].OutputTokens ||
			got[i].ReasoningOutputTokens != want[i].ReasoningOutputTokens ||
			got[i].TotalTokens != want[i].TotalTokens ||
			got[i].ModelContextWindow != want[i].ModelContextWindow ||
			got[i].RuntimeSeconds != want[i].RuntimeSeconds ||
			got[i].Events != want[i].Events {
			t.Fatalf("row %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}

func dateOnly(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func int64Pointer(value int64) *int64 {
	return &value
}

func queryString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()

	var value string
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("querying %q: %v", query, err)
	}
	return value
}

func queryInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()

	var value int64
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("querying %q: %v", query, err)
	}
	return value
}

func assertColumnPresent(t *testing.T, db *sql.DB, table string, column string) {
	t.Helper()

	if count := columnCount(t, db, table, column); count != 1 {
		t.Fatalf("%s.%s column count = %d, want 1", table, column, count)
	}
}

func assertColumnAbsent(t *testing.T, db *sql.DB, table string, column string) {
	t.Helper()

	if count := columnCount(t, db, table, column); count != 0 {
		t.Fatalf("%s.%s column count = %d, want 0", table, column, count)
	}
}

func columnCount(t *testing.T, db *sql.DB, table string, column string) int64 {
	t.Helper()

	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", tableIdentifier(t, table), column).Scan(&count); err != nil {
		t.Fatalf("querying %s.%s column: %v", table, column, err)
	}
	return count
}

func assertTelemetryColumnsNull(t *testing.T, db *sql.DB, table string) {
	t.Helper()

	query := "SELECT CASE WHEN cached_input_tokens IS NULL AND reasoning_output_tokens IS NULL AND model_context_window IS NULL THEN 1 ELSE 0 END FROM " + tableIdentifier(t, table) + " LIMIT 1"
	if got := queryInt(t, db, query); got != 1 {
		t.Fatalf("%s new telemetry columns null = %d, want 1", table, got)
	}
}

func tableIdentifier(t *testing.T, table string) string {
	t.Helper()

	switch table {
	case "codex_sessions", "usage_events", "workflow_phase_events":
		return table
	default:
		t.Fatalf("unexpected table %q", table)
		return ""
	}
}
