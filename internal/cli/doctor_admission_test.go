package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestDoctorAdmissionDiagnostics(t *testing.T) {
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
CREATE TABLE backlog_admission_runs (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  outcome TEXT NOT NULL,
  deferred_reason TEXT,
  candidates_found_count INTEGER NOT NULL,
  candidates_count INTEGER NOT NULL,
  proposed_count INTEGER NOT NULL,
  skipped_json TEXT NOT NULL,
  truncated_json TEXT NOT NULL,
  issues_json TEXT NOT NULL,
  error TEXT
);
INSERT INTO backlog_admission_runs (
  project_id, completed_at, outcome, deferred_reason, candidates_found_count,
  candidates_count, proposed_count, skipped_json, truncated_json, issues_json, error
) VALUES
  ('detent', '2026-07-29T10:03:00Z', 'failed', NULL, 12, 4, 1, '{"excluded_label":2}', '{"candidate_cap":8}', '[{"identifier":"digitaldrywood/detent#1535"}]', 'third failure'),
  ('detent', '2026-07-29T10:02:00Z', 'failed', NULL, 10, 4, 0, '{}', '{"candidate_cap":6}', '[]', 'second failure'),
  ('detent', '2026-07-29T10:01:00Z', 'failed', NULL, 8, 4, 0, '{}', '{"candidate_cap":4}', '[]', 'first failure');
`); err != nil {
		t.Fatalf("seed backlog_admission_runs error = %v", err)
	}

	diagnostic, err := readDoctorAdmissionDiagnostic(ctx, db, "detent", doctorAdmissionDiagnostic{
		Schedule:        "0 6 * * 1-5",
		CriteriaSection: "Admission criteria",
		Dimensions:      []string{"Alignment", "Risk"},
		NeverRun:        true,
	})
	if err != nil {
		t.Fatalf("readDoctorAdmissionDiagnostic() error = %v", err)
	}
	check := doctorAdmissionCheck("admission", diagnostic, "")
	if check.Status != doctorWarn {
		t.Fatalf("Status = %q, want %q", check.Status, doctorWarn)
	}
	for _, want := range []string{
		"3 consecutive failures",
		`"Admission criteria"`,
		"2 project-defined dimensions",
		"candidates=12 evaluated=4 proposed=1",
		"skipped=excluded_label:2",
		"truncated=candidate_cap:8",
		"issues=digitaldrywood/detent#1535",
	} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("Detail = %q, want %q", check.Detail, want)
		}
	}
	if diagnostic.NeverRun || diagnostic.CandidatesFound != 12 || diagnostic.CandidatesEvaluated != 4 || diagnostic.Proposed != 1 {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	if diagnostic.Skipped["excluded_label"] != 2 || diagnostic.Truncated["candidate_cap"] != 8 {
		t.Fatalf("diagnostic counts = skipped %#v truncated %#v", diagnostic.Skipped, diagnostic.Truncated)
	}
}

func TestDoctorAdmissionWarnsWhenNeverRunOrCriteriaUnresolvable(t *testing.T) {
	cfg := workflowconfig.Default()
	cfg.BacklogAdmission.Enabled = true
	cfg.BacklogAdmission.CriteriaSection = "Admission criteria"

	unresolved := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config:       cfg,
		SharedPrompt: "# Workflow\n",
	}, "", doctorDeps{})
	if unresolved.Status != doctorWarn || !strings.Contains(unresolved.Detail, "was not found") {
		t.Fatalf("unresolved check = %#v", unresolved)
	}

	neverRun := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config: cfg,
		SharedPrompt: `# Workflow

## Admission criteria

- **Risk** — requires a bounded recovery path.
`,
	}, "", doctorDeps{})
	if neverRun.Status != doctorWarn || !strings.Contains(neverRun.Detail, "runtime store is not configured") {
		t.Fatalf("never-run check = %#v", neverRun)
	}
	if neverRun.BacklogAdmission == nil || len(neverRun.BacklogAdmission.Dimensions) != 1 {
		t.Fatalf("BacklogAdmission = %#v", neverRun.BacklogAdmission)
	}
}
