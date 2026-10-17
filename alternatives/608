package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestDoctorBackendCapacityDiagnostics(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE work_attempts (
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
	if _, err := db.ExecContext(t.Context(),
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
	recordedDetail := doctorBackendCapacityDetail(diagnostics, 1, false)
	if !strings.Contains(recordedDetail, "may still be active") || !strings.Contains(recordedDetail, "enforcement unknown") {
		t.Fatalf("recorded detail = %q", recordedDetail)
	}
	lastProbeAt := now.Add(5 * time.Minute)
	nextProbeAt := now.Add(15 * time.Minute)
	diagnostics = reconcileDoctorBackendCapacity(diagnostics, []telemetry.BackendOutage{{
		ProjectID:       "detent",
		BackendID:       "codex",
		BackendKind:     "codex",
		Provider:        "openai",
		DetectedAt:      now,
		LastObservedAt:  now,
		ResumeAt:        resumeAt,
		NextProbeAt:     &nextProbeAt,
		LastProbeAt:     &lastProbeAt,
		LastProbeResult: "capacity_exhausted",
		LastProbeDetail: "provider usage limit reached",
		ProbeAttempts:   1,
	}}, true)
	got = diagnostics[0]
	if !got.Active || !got.Enforced || got.LastProbeAt == nil || got.LastProbeResult != "capacity_exhausted" {
		t.Fatalf("reconciled diagnostic = %#v", got)
	}
	detail := doctorBackendCapacityDetail(diagnostics, 1, true)
	if !strings.Contains(detail, "provider-recorded") || !strings.Contains(detail, "last probe") || !strings.Contains(detail, "capacity exhausted") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestDoctorOverloadRetryCount(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE work_attempts (
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
		if _, err := db.ExecContext(t.Context(),
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

	check := checkDoctorBlockedRecoveryLive(t.Context(), "Project detent blocked recovery", reader, cfg, now, nil)
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

func TestReadDoctorBackendCapacityLive(t *testing.T) {
	t.Parallel()

	port := 4000
	deps := doctorDeps{
		httpDo: func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://127.0.0.1:4000/health" {
				t.Fatalf("URL = %s", request.URL)
			}
			body := `{"backend_outages":[{"project_id":"detent","backend_id":"codex","provider":"openai","last_probe_result":"capacity_exhausted"},{"project_id":"other","backend_id":"claude"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		},
	}
	outages, ok := readDoctorBackendCapacityLive(t.Context(), BootConfig{
		Host: "127.0.0.1",
		Port: &port,
	}, "detent", deps)
	if !ok || len(outages) != 1 || outages[0].BackendID != "codex" || outages[0].LastProbeResult != "capacity_exhausted" {
		t.Fatalf("outages = %#v, available %v", outages, ok)
	}
}

func TestCheckDoctorBackendCapacityUsesLiveStateWithoutHistory(t *testing.T) {
	t.Parallel()

	port := 4000
	check := checkDoctorBackendCapacity(t.Context(), globalconfig.PathResolution{
		Path: "/tmp/global.yaml",
	}, BootConfig{Host: "127.0.0.1", Port: &port}, "detent", doctorDeps{
		openSQLiteReadOnly: func(context.Context, string) (doctorTelemetryStore, error) {
			return nil, errDoctorTelemetryStoreUnavailable
		},
		httpDo: func(*http.Request) (*http.Response, error) {
			body := `{"backend_outages":[{"project_id":"detent","backend_id":"codex","provider":"openai","reason":"provider usage limit reached","resume_at":"2026-07-10T05:00:00Z"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		},
	}, time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC))

	if check.Status != doctorWarn || len(check.BackendCapacity) != 1 || !check.BackendCapacity[0].Enforced {
		t.Fatalf("check = %#v, want enforced live outage warning", check)
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
