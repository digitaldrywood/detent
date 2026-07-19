package cli

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCheckDoctorArtifactGateConvergence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "detent.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if db == nil {
			return
		}
		if err := db.Close(); err != nil {
			t.Errorf("db.Close() error = %v", err)
		}
	})
	if _, err := db.Exec(`CREATE TABLE work_attempts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT,
  identifier TEXT,
  terminal_state TEXT,
  completed_at TEXT,
  worker_metadata_json TEXT NOT NULL DEFAULT '{}'
)`); err != nil {
		t.Fatalf("create work_attempts: %v", err)
	}
	insertDoctorArtifactGateAttempt(t, db, "video", "issue-active", "video#47", "2026-07-19T14:00:00Z", true, 3)
	insertDoctorArtifactGateAttempt(t, db, "video", "issue-resolved", "video#48", "2026-07-19T13:00:00Z", false, 0)
	insertDoctorArtifactGateAttempt(t, db, "video", "issue-resolved", "video#48", "2026-07-19T12:00:00Z", true, 3)
	insertDoctorArtifactGateAttempt(t, db, "other", "issue-other", "other#49", "2026-07-19T15:00:00Z", true, 4)
	noisyBase := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	for index := range doctorArtifactGateConvergenceSampleLimit + 1 {
		insertDoctorArtifactGateAttempt(t, db, "video", "issue-noisy", "video#50", noisyBase.Add(time.Duration(index)*time.Second).Format(time.RFC3339), false, 1)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() before read-only check error = %v", err)
	}
	db = nil

	check := checkDoctorArtifactGateConvergence(t.Context(), globalconfig.PathResolution{
		Path: filepath.Join(dir, "global.yaml"),
	}, "video", doctorDeps{openSQLiteReadOnly: openDoctorSQLiteReadOnly})

	if check.Status != doctorWarn {
		t.Fatalf("Status = %s, want %s; detail = %q", check.Status, doctorWarn, check.Detail)
	}
	if len(check.ArtifactGateConvergence) != 1 {
		t.Fatalf("ArtifactGateConvergence = %#v, want one active finding", check.ArtifactGateConvergence)
	}
	diagnostic := check.ArtifactGateConvergence[0]
	if diagnostic.ProjectID != "video" || diagnostic.IssueID != "issue-active" || diagnostic.Identifier != "video#47" {
		t.Fatalf("diagnostic identity = %#v, want video issue-active", diagnostic)
	}
	if diagnostic.StatusField != "render_status" || diagnostic.UnchangedStatus != "recut" || diagnostic.ConsecutiveSuccess != 3 || diagnostic.Limit != 3 {
		t.Fatalf("diagnostic convergence = %#v, want render_status recut 3/3", diagnostic)
	}
	if !strings.Contains(check.Detail, "video#47") || !strings.Contains(check.Hint, "Blocked") {
		t.Fatalf("check = %#v, want actionable issue detail", check)
	}
}

func TestCheckDoctorArtifactGateConvergenceWithoutTrips(t *testing.T) {
	t.Parallel()

	check := checkDoctorArtifactGateConvergence(t.Context(), globalconfig.PathResolution{}, "", doctorDeps{})
	if check.Status != doctorOK || len(check.ArtifactGateConvergence) != 0 {
		t.Fatalf("check = %#v, want unavailable telemetry to be OK", check)
	}
}

func insertDoctorArtifactGateAttempt(
	t *testing.T,
	db *sql.DB,
	projectID string,
	issueID string,
	identifier string,
	completedAt string,
	tripped bool,
	consecutive int,
) {
	t.Helper()
	metadata := `{"artifact_gate_convergence":{"status_field":"render_status","dispatch_status":"recut","completion_status":"recut","unchanged":true,"consecutive_unchanged":` + strconv.Itoa(consecutive) + `,"limit":3,"tripped":` + strconv.FormatBool(tripped) + `}}`
	if _, err := db.Exec(
		`INSERT INTO work_attempts (project_id, issue_id, identifier, terminal_state, completed_at, worker_metadata_json) VALUES (?, ?, ?, 'success', ?, ?)`,
		projectID,
		issueID,
		identifier,
		completedAt,
		metadata,
	); err != nil {
		t.Fatalf("insert work attempt: %v", err)
	}
}
