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
