package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDoctorBackendCapacityDiagnostics(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE work_attempts (
		project_id TEXT NOT NULL,
		identifier TEXT,
		issue_id TEXT,
		capacity_snapshot_json TEXT NOT NULL,
		error_class TEXT,
		completed_at TEXT
	)`); err != nil {
		t.Fatalf("CREATE TABLE error = %v", err)
	}
	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resumeAt := now.Add(44 * time.Minute)
	snapshot, err := json.Marshal(doctorBackendCapacitySnapshot{BackendOutages: []doctorBackendCapacitySnapshotOutage{{
		BackendID:      "codex",
		BackendKind:    "codex",
		Provider:       "openai",
		DetectedAt:     now,
		LastObservedAt: now,
		ResumeAt:       resumeAt,
	}}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO work_attempts (project_id, identifier, issue_id, capacity_snapshot_json, error_class, completed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"detent", "digitaldrywood/detent#1142", "issue-1142", string(snapshot), "backend_capacity", now.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("INSERT error = %v", err)
	}

	diagnostics, err := doctorBackendCapacityDiagnostics(t.Context(), db, "detent", now)
	if err != nil {
		t.Fatalf("doctorBackendCapacityDiagnostics() error = %v", err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v, want one", diagnostics)
	}
	got := diagnostics[0]
	if !got.Active || got.BackendID != "codex" || len(got.AffectedIssues) != 1 || got.AffectedIssues[0] != "digitaldrywood/detent#1142" {
		t.Fatalf("diagnostic = %#v", got)
	}
}

func TestDoctorOverloadRetryCount(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE work_attempts (
		project_id TEXT NOT NULL,
		error_class TEXT,
		completed_at TEXT
	)`); err != nil {
		t.Fatalf("CREATE TABLE error = %v", err)
	}
	now := time.Date(2026, 7, 12, 21, 0, 0, 0, time.UTC)
	rows := []struct {
		projectID  string
		errorClass string
		completed  time.Time
	}{
		{projectID: "detent", errorClass: "transient_overload", completed: now.Add(-30 * time.Minute)},
		{projectID: "detent", errorClass: "transient_overload", completed: now.Add(-2 * time.Hour)},
		{projectID: "other", errorClass: "transient_overload", completed: now.Add(-20 * time.Minute)},
		{projectID: "detent", errorClass: "backend_capacity", completed: now.Add(-10 * time.Minute)},
	}
	for _, row := range rows {
		if _, err := db.Exec(
			`INSERT INTO work_attempts (project_id, error_class, completed_at) VALUES (?, ?, ?)`,
			row.projectID,
			row.errorClass,
			row.completed.Format(time.RFC3339Nano),
		); err != nil {
			t.Fatalf("INSERT error = %v", err)
		}
	}

	count, err := doctorOverloadRetryCount(t.Context(), db, "detent", now)
	if err != nil {
		t.Fatalf("doctorOverloadRetryCount() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("doctorOverloadRetryCount() = %d, want 1", count)
	}
}

func TestDoctorBlockedRecoveryReportsCapacityParkedIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	issue := connector.Issue{ID: "issue-1142", Identifier: "digitaldrywood/detent#1142", State: "Blocked"}
	reader := &doctorBackendCapacityConnector{
		issues: []connector.Issue{issue},
		comments: map[string][]connector.IssueComment{
			issue.ID: {{Body: `Detent stopped retrying this worker after 5 consecutive instant failures with the same backend error. backend_error_body: {"error":{"type":"usageLimitExceeded","resetAt":1783651140}}`}},
		},
	}
	cfg := workflowconfig.Config{
		Codex: workflowconfig.Codex{ModelProvider: "openai"},
	}

	check := checkDoctorBlockedRecoveryLive(t.Context(), "Project detent blocked recovery", reader, cfg, now)
	if check.Status != doctorWarn || len(check.BackendCapacity) != 1 {
		t.Fatalf("check = %#v, want capacity warning", check)
	}
	if got := check.BackendCapacity[0].ParkedIssues; len(got) != 1 || !strings.Contains(got[0], issue.Identifier) {
		t.Fatalf("ParkedIssues = %#v", got)
	}
	if !strings.Contains(check.Detail, "parked by provider capacity exhaustion") {
		t.Fatalf("Detail = %q", check.Detail)
	}
}

type doctorBackendCapacityConnector struct {
	issues   []connector.Issue
	comments map[string][]connector.IssueComment
}

func (c *doctorBackendCapacityConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return append([]connector.Issue(nil), c.issues...), nil
}

func (c *doctorBackendCapacityConnector) FetchIssueComments(_ context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	return append([]connector.IssueComment(nil), c.comments[issue.ID]...), nil
}
