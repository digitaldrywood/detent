package cli

import (
	"context"
	"database/sql"
	"os"
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
		`CREATE TABLE workflow_phase_events (id INTEGER PRIMARY KEY, project_id TEXT, identifier TEXT, phase_name TEXT, previous_phase_name TEXT, reason TEXT, started_at TEXT, issue_id TEXT, phase_type TEXT, metadata_json TEXT)`,
		`CREATE TABLE work_attempts (id INTEGER PRIMARY KEY, project_id TEXT, identifier TEXT, error_class TEXT, completed_at TEXT)`,
		`CREATE TABLE lane_ledger (id INTEGER PRIMARY KEY, project_id TEXT, written_at TEXT, issue_id TEXT, from_state TEXT, to_state TEXT, reason TEXT, result TEXT, origin TEXT, resolved_at TEXT)`,
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
			want:   map[string]doctorStatus{"INV-1": doctorOK, "INV-2": doctorOK, "INV-3": doctorOK, "INV-4": doctorOK, "INV-9": doctorOK},
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
			want:   map[string]doctorStatus{"INV-4": doctorFail},
		},
		{
			name: "successful programmatic merge beside a queue",
			statements: []string{
				`INSERT INTO workflow_phase_events(project_id,identifier,phase_name,previous_phase_name,reason,started_at) VALUES ('alpha','owner/repo#4','Done','Merging','merge_worker_programmatic_merge','` + recent + `')`,
				`INSERT INTO lane_ledger(project_id,written_at) VALUES ('alpha','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main", MergeQueue: true},
			want:   map[string]doctorStatus{"INV-4": doctorFail},
		},
		{
			name: "programmatic merges before the queue window are not evidence",
			statements: []string{
				`INSERT INTO workflow_phase_events(project_id,identifier,phase_name,previous_phase_name,reason,started_at) VALUES ('alpha','owner/repo#5','Done','Merging','merge_worker_programmatic_merge','` + now.Add(-36*time.Hour).Format(time.RFC3339Nano) + `')`,
				`INSERT INTO work_attempts(project_id,identifier,error_class,completed_at) VALUES ('alpha','owner/repo#5','programmatic_merge_failed','` + now.Add(-30*time.Hour).Format(time.RFC3339Nano) + `')`,
				`INSERT INTO lane_ledger(project_id,written_at) VALUES ('alpha','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main", MergeQueue: true},
			want:   map[string]doctorStatus{"INV-4": doctorOK},
		},
		{
			name: "post-turn runner error is not an infrastructure park",
			statements: []string{
				`INSERT INTO work_attempts(project_id,identifier,error_class,completed_at) VALUES ('alpha','owner/repo#8','runner_error','` + recent + `')`,
				`INSERT INTO workflow_phase_events(project_id,identifier,phase_name,previous_phase_name,reason,started_at) VALUES ('alpha','owner/repo#8','Blocked','In Progress','terminal_attempt_retry_limit','` + later + `')`,
				`INSERT INTO lane_ledger(project_id,written_at) VALUES ('alpha','` + recent + `')`,
			},
			policy: ghconnector.BranchMergePolicy{Branch: "main"},
			want:   map[string]doctorStatus{"INV-2": doctorOK},
		},
		{
			name:   "rules unavailable on plan",
			policy: ghconnector.BranchMergePolicy{RulesUnavailableOnPlan: true},
			want:   map[string]doctorStatus{},
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

func TestDoctorRespectsProjectCIPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, event := range []string{"pull_request", "merge_group", "pull_request_target"} {
		t.Run(event, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			workflows := filepath.Join(root, ".github", "workflows")
			if err := os.MkdirAll(workflows, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workflows, "ci.yml"), []byte("on:\n  "+event+":\njobs:\n  verify:\n    runs-on: ubuntu-latest\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = "github"
			cfg.Tracker.Repository = "owner/repo"
			cfg.Workspace.SourceRoot = root
			cfg.Deliverable.Kind = workflowconfig.DeliverablePullRequest
			deps := successfulDoctorDeps()
			deps.githubBranchPolicy = func(context.Context, workflowconfig.Config, string) (ghconnector.BranchMergePolicy, error) {
				return ghconnector.BranchMergePolicy{Branch: "main", Strict: true}, nil
			}
			checks := checkDoctorInvariantEvidence(t.Context(), "alpha", globalconfig.Project{ID: "alpha"}, cfg, doctorInvariantFixtureDB(t), deps)
			foundPolicy := false
			for _, check := range checks {
				if strings.Contains(check.Name, " INV-5 ") || strings.Contains(check.Name, " INV-8 ") || strings.Contains(check.Hint, "zero PR CI") || strings.Contains(check.Hint, "Disable strict") {
					t.Errorf("repository development policy leaked into project diagnostics: %#v", check)
				}
				if check.Name == "Project alpha branch policy" {
					foundPolicy = true
					if check.Status != doctorOK || !strings.Contains(check.Detail, "requires up-to-date branches: true") || check.Hint != "" {
						t.Errorf("strict branch policy must remain supported: %#v", check)
					}
				}
			}
			if !foundPolicy {
				t.Fatal("missing branch policy evidence")
			}
		})
	}
}
