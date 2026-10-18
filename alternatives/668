package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestDoctorRoutineDiagnostics(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	if _, err := db.ExecContext(ctx, `
CREATE TABLE routine_runs (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  routine_name TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  proposed_count INTEGER NOT NULL,
  filed_count INTEGER NOT NULL,
  deduplicated_count INTEGER NOT NULL,
  limited_count INTEGER NOT NULL,
  error TEXT
);
INSERT INTO routine_runs (project_id, routine_name, completed_at, proposed_count, filed_count, deduplicated_count, limited_count, error) VALUES
  ('detent', 'dependencies', '2026-07-17T10:00:00Z', 4, 2, 1, 1, NULL),
  ('detent', 'dependencies', '2026-07-17T09:00:00Z', 4, 2, 1, 1, NULL),
  ('detent', 'dependencies', '2026-07-17T08:00:00Z', 4, 2, 1, 1, NULL),
  ('detent', 'flaky-tests', '2026-07-17T10:03:00Z', 0, 0, 0, 0, 'third failure'),
  ('detent', 'flaky-tests', '2026-07-17T10:02:00Z', 0, 0, 0, 0, 'second failure'),
  ('detent', 'flaky-tests', '2026-07-17T10:01:00Z', 0, 0, 0, 0, 'first failure');
`); err != nil {
		t.Fatalf("seed routine_runs error = %v", err)
	}
	definitions := []workflowconfig.Routine{
		{Name: "never", Schedule: "0 1 * * *", Prompt: "Inspect."},
		{Name: "dependencies", Schedule: "0 2 * * *", Prompt: "Inspect."},
		{Name: "flaky-tests", Schedule: "0 3 * * *", Prompt: "Inspect."},
	}
	diagnostics, err := doctorRoutineDiagnostics(ctx, db, "detent", definitions)
	if err != nil {
		t.Fatalf("doctorRoutineDiagnostics() error = %v", err)
	}
	check := doctorRoutineCheck("routines", diagnostics, "")
	if check.Status != doctorWarn {
		t.Fatalf("Status = %q, want %q", check.Status, doctorWarn)
	}
	for _, want := range []string{
		"never run: never",
		"repeatedly failing: flaky-tests (3 consecutive",
		"repeatedly hitting finding ceilings: dependencies (3 consecutive runs",
		"dependencies at 2026-07-17T10:00:00Z proposed=4 filed=2 deduplicated=1 limited=1",
	} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("Detail = %q, want %q", check.Detail, want)
		}
	}
	if len(check.Routines) != 3 || !check.Routines[2].NeverRun {
		t.Fatalf("Routines = %#v", check.Routines)
	}
}

func TestDoctorRoutineDiagnosticsHandlesMissingTableAsNeverRun(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	diagnostics, err := doctorRoutineDiagnostics(context.Background(), db, "detent", []workflowconfig.Routine{{Name: "audit", Schedule: "0 * * * *", Prompt: "Inspect."}})
	if err != nil {
		t.Fatalf("doctorRoutineDiagnostics() error = %v", err)
	}
	if len(diagnostics) != 1 || !diagnostics[0].NeverRun {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}
