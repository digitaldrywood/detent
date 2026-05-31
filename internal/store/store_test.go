package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/store/sqlc"
)

func TestOpenSQLiteAppliesMigrationsAndPragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "symphony.db")

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
	if got := queryInt(t, sqliteBackend.db, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('symphony_runs', 'codex_sessions')"); got != 2 {
		t.Fatalf("migrated table count = %d, want 2", got)
	}
}

func TestSQLiteQueriesRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "symphony.db")

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

	run, err := backend.Queries().CreateSymphonyRun(ctx, sqlc.CreateSymphonyRunParams{
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
		t.Fatalf("CreateSymphonyRun() error = %v", err)
	}

	session, err := backend.Queries().CreateCodexSession(ctx, sqlc.CreateCodexSessionParams{
		RunID:          sql.NullInt64{Int64: run.ID, Valid: true},
		IssueID:        sql.NullString{String: "I_kwDOSskuwc8AAAABD42cNw", Valid: true},
		Identifier:     sql.NullString{String: "digitaldrywood/symphony-go#5", Valid: true},
		IssueUrl:       sql.NullString{String: "https://github.com/digitaldrywood/symphony-go/issues/5", Valid: true},
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
	if got.Identifier.String != "digitaldrywood/symphony-go#5" {
		t.Fatalf("session identifier = %q, want digitaldrywood/symphony-go#5", got.Identifier.String)
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
				Identifier: "digitaldrywood/symphony-go#6",
				IssueURL:   "https://github.com/digitaldrywood/symphony-go/issues/6",
				StartedAt:  time.Date(2026, 5, 30, 12, 1, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}

			if err := backend.FinishSession(ctx, sessionID, SessionFinish{
				CompletedAt:    time.Date(2026, 5, 30, 12, 5, 0, 0, time.UTC),
				Turns:          2,
				InputTokens:    100,
				OutputTokens:   25,
				TotalTokens:    125,
				RuntimeSeconds: 240,
				FinalState:     "Human Review",
				Model:          "gpt-5",
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

			run, err := backend.Queries().GetSymphonyRun(ctx, runID)
			if err != nil {
				t.Fatalf("GetSymphonyRun() error = %v", err)
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

			spend, err := backend.DailyTokenSpend(ctx, time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("DailyTokenSpend() error = %v", err)
			}
			if spend.InputTokens != 100 || spend.OutputTokens != 25 || spend.TotalTokens != 125 || spend.Sessions != 1 {
				t.Fatalf("DailyTokenSpend() = %#v", spend)
			}
			if len(spend.ByModel) != 1 || spend.ByModel[0].Model != "gpt-5" {
				t.Fatalf("DailyTokenSpend().ByModel = %#v", spend.ByModel)
			}
		})
	}
}

func TestOpenRejectsUnsupportedBackend(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), Config{
		Backend: Backend("postgres"),
		Path:    filepath.Join(t.TempDir(), "symphony.db"),
	})
	if err == nil {
		t.Fatal("Open() error = nil, want unsupported backend error")
	}
}

func TestOpenUsesSQLiteBackendByDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Path:        filepath.Join(t.TempDir(), "symphony.db"),
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
		Path:    filepath.Join(t.TempDir(), "symphony.db"),
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
