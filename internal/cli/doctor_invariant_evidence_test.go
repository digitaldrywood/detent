package cli

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func doctorInvariantFixtureDB(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "detent.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range append([]string{
		`CREATE TABLE workflow_phase_events (id INTEGER PRIMARY KEY, project_id TEXT, identifier TEXT, phase_name TEXT, previous_phase_name TEXT, reason TEXT, started_at TEXT)`,
		`CREATE TABLE work_attempts (id INTEGER PRIMARY KEY, project_id TEXT, identifier TEXT, error_class TEXT, completed_at TEXT)`,
		`CREATE TABLE lane_ledger (id INTEGER PRIMARY KEY, project_id TEXT, written_at TEXT)`,
	}, statements...) {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return path
}

func doctorInvariantStatus(t *testing.T, checks []doctorCheck, inv string) (doctorStatus, string) {
	t.Helper()
	for _, check := range checks {
		if strings.Contains(check.Name, " "+inv+" ") {
			return check.Status, check.Detail
		}
	}
	t.Fatalf("no %s check in %#v", inv, checks)
	return "", ""
}

func TestDoctorInvariantEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour).Format(time.RFC3339Nano)
	later := now.Add(-time.Hour + 2*time.Minute).Format(time.RFC3339Nano)
	cfg := workflowconfig.Config{}
	cfg.Tracker.Kind = "github"
	cfg.Tracker.Repository = "owner/repo"
	cfg.Deliverable.Kind = workflowconfig.DeliverablePullRequest
	for _, tt := range []struct {
		name       string
		statements []string
		policy     ghconnector.BranchMergePolicy
		want       map[string]doctorStatus
	}{
		{
			name: "clean fleet",
			statements: []string{
				`INSERT INTO workflow_phase_events(project_id,identifier,phase_name,previous_phase_name,reason,started_at) VALUES ('alpha','owner/repo#1','In Progress','Todo','dispatch_start','` + recent + `')`,
				`INSERT INTO lane_ledger(project_id,written_at) VALUES ('alpha','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main", MergeQueue: true},
			want:   map[string]doctorStatus{"INV-1": doctorOK, "INV-2": doctorOK, "INV-3": doctorOK, "INV-4": doctorOK, "INV-8": doctorOK, "INV-9": doctorOK},
		},
		{
			name: "transitions without ledger writes",
			statements: []string{
				`INSERT INTO workflow_phase_events(project_id,identifier,phase_name,previous_phase_name,reason,started_at) VALUES ('alpha','owner/repo#1','In Progress','Todo','dispatch_start','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main"},
			want:   map[string]doctorStatus{"INV-1": doctorFail},
		},
		{
			name: "infrastructure failure parked the issue",
			statements: []string{
				`INSERT INTO work_attempts(project_id,identifier,error_class,completed_at) VALUES ('alpha','owner/repo#7','backend_startup_timeout','` + recent + `')`,
				`INSERT INTO workflow_phase_events(project_id,identifier,phase_name,previous_phase_name,reason,started_at) VALUES ('alpha','owner/repo#7','Blocked','Todo','terminal_attempt_retry_limit','` + later + `')`,
				`INSERT INTO lane_ledger(project_id,written_at) VALUES ('alpha','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main"},
			want:   map[string]doctorStatus{"INV-2": doctorFail},
		},
		{
			name: "retired reason recorded",
			statements: []string{
				`INSERT INTO workflow_phase_events(project_id,identifier,phase_name,previous_phase_name,reason,started_at) VALUES ('alpha','owner/repo#2','Todo','In Progress','worker_lane_revocation_workspace_preserved','` + recent + `')`,
				`INSERT INTO lane_ledger(project_id,written_at) VALUES ('alpha','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main"},
			want:   map[string]doctorStatus{"INV-9": doctorFail},
		},
		{
			name: "queue bypassed and strict protection",
			statements: []string{
				`INSERT INTO work_attempts(project_id,identifier,error_class,completed_at) VALUES ('alpha','owner/repo#3','programmatic_merge_failed','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main", MergeQueue: true, Strict: true},
			want:   map[string]doctorStatus{"INV-4": doctorFail, "INV-8": doctorFail},
		},
		{
			name:   "rules unavailable on plan",
			policy: ghconnector.BranchMergePolicy{RulesUnavailableOnPlan: true},
			want:   map[string]doctorStatus{"INV-8": doctorOK},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := doctorInvariantFixtureDB(t, tt.statements...)
			deps := doctorDeps{
				now:                func() time.Time { return now },
				openSQLiteReadOnly: openDoctorSQLiteReadOnly,
				githubBranchPolicy: func(context.Context, workflowconfig.Config, string) (ghconnector.BranchMergePolicy, error) {
					return tt.policy, nil
				},
			}
			checks := checkDoctorInvariantEvidence(t.Context(), "alpha", globalconfig.Project{ID: "alpha"}, cfg, path, deps)
			for inv, want := range tt.want {
				if got, detail := doctorInvariantStatus(t, checks, inv); got != want {
					t.Errorf("%s = %s (%s), want %s", inv, got, detail, want)
				}
			}
		})
	}
}

func TestDoctorInvariantWorkflowVerdict(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		yaml string
		want doctorStatus
	}{
		{"no pull_request trigger", "on:\n  merge_group:\n    types: [checks_requested]\njobs:\n  verify:\n    runs-on: ubuntu-latest\n", doctorOK},
		{"placeholders only", "on:\n  pull_request:\n    types: [opened]\njobs:\n  placeholders:\n    if: github.event_name == 'pull_request'\n  verify:\n    if: github.event_name != 'pull_request'\n", doctorOK},
		{"real jobs on every push", "on:\n  pull_request:\njobs:\n  lint:\n    runs-on: ubuntu-latest\n  verify:\n    if: github.event.pull_request.draft == false\n", doctorWarn},
		{"malformed", "on: [\n", doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := doctorInvariantWorkflowVerdict("alpha", []byte(tt.yaml)); got.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Detail, tt.want)
			}
		})
	}
}
