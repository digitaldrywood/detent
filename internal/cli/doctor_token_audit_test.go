package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorTokenAuditWorkflowSourceDrift(t *testing.T) {
	for _, tt := range []struct {
		name, ref, branch string
		modify            bool
		want              doctorStatus
	}{
		{"default branch", "", "main", false, doctorOK},
		{"side branch", "", "side", false, doctorWarn},
		{"pinned side branch", "origin/main", "side", false, doctorOK},
		{"pinned content drift", "origin/main", "main", true, doctorWarn},
		{"missing ref", "missing", "main", false, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := initDoctorWorkflowSourceRepository(t)
			if tt.branch != "main" {
				runDoctorWorkflowSourceGit(t, root, "checkout", "-b", tt.branch)
			}
			if tt.modify {
				writeDoctorWorkflowSourceFile(t, filepath.Join(root, "WORKFLOW.md"), "Changed instructions")
			}
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "github"
			cfg.Tracker.Repository = "owner/repo"
			deps := doctorDeps{githubRepositoryInfo: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error) {
				return ghconnector.RepositoryInfo{DefaultBranch: "main"}, nil
			}}
			got := checkDoctorWorkflowSourceDrift(t.Context(), "p", globalconfig.Project{Workdir: root, Workflow: filepath.Join(root, "WORKFLOW.md"), WorkflowRef: tt.ref}, cfg, deps)
			if got.Status != tt.want || !strings.Contains(got.Detail, "bytes") {
				t.Fatalf("got %+v, want %s", got, tt.want)
			}
		})
	}
}

func TestDoctorTokenAuditInstructionBudget(t *testing.T) {
	for _, tt := range []struct {
		size int
		want doctorStatus
	}{
		{24 * 1024, doctorOK}, {24*1024 + 1, doctorWarn}, {48 * 1024, doctorWarn}, {48*1024 + 1, doctorFail},
	} {
		t.Run(strconv.Itoa(tt.size), func(t *testing.T) {
			got := checkDoctorInstructionBudget("p", []doctorInstructionFile{{path: "WORKFLOW.md", bytes: tt.size}}, nil)
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditInstructionFiles(t *testing.T) {
	for _, tt := range []struct {
		name              string
		override, missing bool
	}{
		{name: "chain cap and directives"}, {name: "override precedence", override: true}, {name: "missing reference", missing: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := initDoctorWorkflowSourceRepository(t)
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("a", 40*1024))
			if tt.override {
				writeDoctorWorkflowSourceFile(t, filepath.Join(root, "AGENTS.override.md"), "override")
			}
			sub := filepath.Join(root, "sub")
			if err := os.Mkdir(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			writeDoctorWorkflowSourceFile(t, filepath.Join(sub, "AGENTS.md"), "nested")
			if !tt.missing {
				writeDoctorWorkflowSourceFile(t, filepath.Join(sub, "CLAUDE.md"), "Run make check.")
			}
			prompt := "Follow AGENTS.md and CLAUDE.md.\nRead CLAUDE.md."
			files, problems := doctorInstructionFiles(t.Context(), sub, prompt, "")
			if (len(problems) > 0) != tt.missing {
				t.Fatalf("problems %v", problems)
			}
			total := 0
			claude := 0
			for _, f := range files {
				if strings.Contains(f.path, "AGENTS") {
					total += f.bytes
				}
				if strings.HasSuffix(f.path, "CLAUDE.md") {
					claude++
				}
			}
			want := 32 * 1024
			if tt.override {
				want = len("override") + len("nested")
			}
			if total != want || (!tt.missing && claude != 1) {
				t.Fatalf("total %d, files %+v", total, files)
			}
		})
	}
}

func TestDoctorTokenAuditTransitiveInstructions(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "AGENTS.override.md"} {
		t.Run(name, func(t *testing.T) {
			root, _ := initDoctorWorkflowSourceRepository(t)
			instructions := "Follow policy.md."
			policy := "Read nested.md.\nRun make check.\n" + strings.Repeat("x", 25*1024)
			nested := "Follow policy.md and AGENTS.md."
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, name), instructions)
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "policy.md"), policy)
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "nested.md"), nested)
			if name == "AGENTS.override.md" {
				writeDoctorWorkflowSourceFile(t, filepath.Join(root, "AGENTS.md"), "ignored")
			}
			prompt := "Do not run make check."
			files, problems := doctorInstructionFiles(t.Context(), root, prompt, "")
			if len(problems) != 0 {
				t.Fatal(problems)
			}
			counts := map[string]int{}
			for _, file := range files {
				counts[filepath.Base(file.path)]++
			}
			if counts["policy.md"] != 1 || counts["nested.md"] != 1 {
				t.Fatalf("reference counts: %v", counts)
			}
			if got := checkDoctorInstructionBudget("p", files, problems); got.Status != doctorWarn {
				t.Fatalf("budget: %+v", got)
			}
			if got := checkDoctorGateInstructionConflict("p", "make check", files, problems); got.Status != doctorWarn {
				t.Fatalf("conflict: %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditGateConflict(t *testing.T) {
	for _, tt := range []struct {
		name, a, b string
		want       doctorStatus
	}{
		{"single", "Run make check.", "Other advice", doctorOK},
		{"duplicate", "Run make check.", "make check is required.", doctorWarn},
		{"contradiction", "Run make check.", "Do not run make check unless safety critical.", doctorWarn},
		{"command boundary", "Run make check-fast.", "Run make check.", doctorOK},
		{"mentions only", "Example: make check", "Example: make check", doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := checkDoctorGateInstructionConflict("p", "make check", []doctorInstructionFile{{path: "AGENTS.md", text: tt.a}, {path: "WORKFLOW.md", text: tt.b}}, nil)
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func doctorTokenAuditDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, s := range []string{
		`CREATE TABLE usage_events(project_id TEXT,total_tokens INTEGER,finished_at TEXT)`,
		`CREATE TABLE work_attempts(project_id TEXT,metrics_json TEXT,started_at TEXT,completed_at TEXT)`,
		`CREATE TABLE codex_sessions(project_id TEXT,started_at TEXT,runtime_identity_json TEXT,reasoning_effort TEXT,reasoning_effort_provenance TEXT)`,
	} {
		if _, err := db.ExecContext(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}

func TestDoctorTokenAuditAccounting(t *testing.T) {
	for _, tt := range []struct {
		name            string
		usage, attempts int
		want            doctorStatus
	}{
		{"empty", 0, 0, doctorOK}, {"equal", 100, 100, doctorOK}, {"five percent", 100, 95, doctorOK}, {"under", 100, 94, doctorWarn}, {"over", 100, 106, doctorWarn}, {"zero usage", 0, 5, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, db := doctorTokenAuditDB(t)
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			recent := now.Add(-time.Hour).Format(time.RFC3339)
			for _, row := range []struct {
				id, at string
				tokens int
			}{{"p", recent, tt.usage}, {"other", recent, 5000}, {"p", now.Add(-25 * time.Hour).Format(time.RFC3339), 5000}, {"p", now.Add(time.Hour).Format(time.RFC3339), 5000}} {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO usage_events VALUES(?,?,?)`, row.id, row.tokens, row.at); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO work_attempts VALUES(?,?,?,?)`, "p", fmt.Sprintf(`{"total_tokens":%d}`, tt.attempts), recent, recent); err != nil {
				t.Fatal(err)
			}
			got := checkDoctorTokenAccounting(t.Context(), "p", path, doctorDeps{now: func() time.Time { return now }})
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditModelPolicy(t *testing.T) {
	for _, tt := range []struct {
		name   string
		above  int
		source string
		want   doctorStatus
	}{
		{"none", 0, "issue.effort", doctorOK}, {"ten percent", 1, "issue.effort", doctorOK}, {"above threshold", 2, "issue.effort", doctorWarn}, {"configured effort", 2, "instance", doctorOK},
		{"code override", 2, "issue.code.effort", doctorWarn},
		{"rework override", 2, "issue.rework.effort", doctorWarn},
		{"merge override", 2, "issue.merge.effort", doctorWarn},
		{"plan override", 2, "issue.plan.effort", doctorWarn},
		{"routine override", 2, "issue.routine.effort", doctorWarn},
		{"validator override", 2, "issue.validator.effort", doctorWarn},
		{"security audit override", 2, "issue.security_audit.effort", doctorWarn},
		{"role threshold", 1, "issue.code.effort", doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, db := doctorTokenAuditDB(t)
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			cfg := workflowconfig.Config{}
			cfg.Agents.ModelSelection = workflowconfig.ModelSelection{Enabled: new(true), NormalModel: new("gpt-6-astra"), Levels: map[string]workflowconfig.ModelSelectionDefaults{"normal": {Model: new("normal"), Effort: new("low")}}}
			for i := range 10 {
				effort := "low"
				if i < tt.above {
					effort = "high"
				}
				raw := fmt.Sprintf(`{"selection":{"level":"normal","effort_source":%q}}`, tt.source)
				if _, err := db.ExecContext(t.Context(), `INSERT INTO codex_sessions VALUES(?,?,?,?,?)`, "p", now.Add(-time.Hour).Format(time.RFC3339), raw, effort, "configured"); err != nil {
					t.Fatal(err)
				}
			}
			got := checkDoctorModelPolicy(t.Context(), "p", path, cfg, doctorDeps{now: func() time.Time { return now }})
			if got.Status != tt.want || !strings.Contains(got.Detail, "gpt-6-astra") || !strings.Contains(got.Detail, "normal_model=project") {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditCITriggerShape(t *testing.T) {
	for _, tt := range []struct {
		name, on, concurrency string
		want                  doctorStatus
	}{
		{"label no concurrency", "pull_request:\n    types: [labeled]", "", doctorWarn},
		{"label branch concurrency", "pull_request:\n    types: [labeled]", "concurrency: '${{ github.head_ref }}'", doctorWarn},
		{"label head concurrency", "pull_request:\n    types: [labeled]", "concurrency:\n  group: 'ci-${{ github.event.pull_request.head.sha }}'", doctorOK},
		{"default PR events", "pull_request", "", doctorOK},
		{"push", "push", "", doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "github"
			cfg.Tracker.Repository = "owner/repo"
			cfg.Gate.RequiredStatusChecks = []string{"Verify"}
			deps := doctorDeps{githubWorkflows: func(_ context.Context, _ workflowconfig.Config, repo string) (map[string]string, error) {
				if repo != "owner/repo" {
					t.Fatal(repo)
				}
				return map[string]string{"ci.yml": "on:\n  " + tt.on + "\n" + tt.concurrency + "\njobs:\n  verify:\n    name: Verify\n    runs-on: ubuntu-latest\n    steps: []\n"}, nil
			}}
			got := checkDoctorCITriggerShape(t.Context(), "p", cfg, deps)
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditUnavailable(t *testing.T) {
	deps := doctorDeps{openSQLiteReadOnly: func(context.Context, string) (doctorTelemetryStore, error) { return nil, errors.New("unavailable") }}
	for _, check := range []doctorCheck{checkDoctorTokenAccounting(t.Context(), "p", "", deps), checkDoctorModelPolicy(t.Context(), "p", "", workflowconfig.Config{}, deps)} {
		if check.Status != doctorWarn || !strings.Contains(check.Detail, "unavailable") {
			t.Fatalf("got %+v", check)
		}
	}
}

func TestDoctorTokenAuditNonGitInstructions(t *testing.T) {
	for _, name := range []string{"rules.go", "Makefile", "POLICY"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, name), "Run make check.")
			prompt := "Read `" + name + "`.\nReview the diff against `origin/main` for `Merging`; read `git diff origin/main...HEAD`.\nDo not run make check.\n" + strings.Repeat("x", 49*1024)
			files, problems := doctorInstructionFiles(t.Context(), root, prompt, "")
			if len(files) != 2 {
				t.Fatalf("file count %d; problems %v", len(files), problems)
			}
			if got := checkDoctorInstructionBudget("p", files, problems); got.Status != doctorFail {
				t.Fatalf("budget %+v", got)
			}
			if got := checkDoctorGateInstructionConflict("p", "make check", files, problems); !strings.Contains(got.Detail, name) || !strings.Contains(got.Detail, "WORKFLOW.md") {
				t.Fatalf("conflict %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditExternalWorkflow(t *testing.T) {
	root, _ := initDoctorWorkflowSourceRepository(t)
	external, _ := initDoctorWorkflowSourceRepository(t)
	runDoctorWorkflowSourceGit(t, root, "checkout", "-b", "side")
	cfg := workflowconfig.Config{}
	cfg.Tracker.Kind = "github"
	cfg.Tracker.Repository = "owner/repo"
	deps := doctorDeps{githubRepositoryInfo: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error) {
		return ghconnector.RepositoryInfo{DefaultBranch: "main"}, nil
	}}
	got := checkDoctorWorkflowSourceDrift(t.Context(), "p", globalconfig.Project{Workdir: root, Workflow: filepath.Join(external, "WORKFLOW.md")}, cfg, deps)
	if got.Status != doctorWarn || !strings.Contains(got.Detail, `checkout branch "side"`) || strings.Count(got.Detail, "17 bytes") != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestDoctorTokenAuditCIEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		readErr      bool
		want         doctorStatus
	}{
		{"job concurrency", "on:\n  pull_request:\n    types: [labeled]\njobs:\n  Verify:\n    concurrency: 'ci-${{ github.event.pull_request.head.sha }}'\n", false, doctorOK},
		{"malformed", "on: [", false, doctorWarn},
		{"unmatched", "on: push\njobs:\n  other:\n    steps: []\n", false, doctorWarn},
		{"read denied", "", true, doctorWarn},
		{"empty", "", false, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "github"
			cfg.Gate.RequiredStatusChecks = []string{"Verify"}
			deps := doctorDeps{githubWorkflows: func(context.Context, workflowconfig.Config, string) (map[string]string, error) {
				if tt.readErr {
					return nil, errors.New("denied")
				}
				return map[string]string{"ci.yml": tt.source}, nil
			}}
			got := checkDoctorCITriggerShape(t.Context(), "p", cfg, deps)
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditMissingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE unrelated(id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []doctorCheck{checkDoctorTokenAccounting(t.Context(), "p", path, doctorDeps{}), checkDoctorModelPolicy(t.Context(), "p", path, workflowconfig.Config{}, doctorDeps{})} {
		if check.Status != doctorWarn || !strings.Contains(check.Detail, "query failed") {
			t.Fatalf("got %+v", check)
		}
	}
}

func TestDoctorTokenAuditFastGateConflict(t *testing.T) {
	files := []doctorInstructionFile{{path: "AGENTS.md", text: "Run make check."}, {path: "WORKFLOW.md", text: "Run make check-fast.\nDo not run make check unless safety-critical files change."}}
	got := checkDoctorGateInstructionConflict("p", "make check-fast", files, nil)
	if got.Status != doctorWarn || !strings.Contains(got.Detail, "full gate:") {
		t.Fatalf("got %+v", got)
	}
}

func TestDoctorReferencedWorkflowGateConflict(t *testing.T) {
	for _, tt := range []struct {
		name, agents string
		want         doctorStatus
	}{
		{"conflicting command", "Run make check.", doctorWarn},
		{"configured command reference", "Use the configured gate.run.", doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := initDoctorWorkflowSourceRepository(t)
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "AGENTS.md"), tt.agents+"\nFollow WORKFLOW.md.")
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "WORKFLOW.md"), "The validation gate is make check-fast.\nDo not run make check unless safety-critical files change.\nFollow AGENTS.md and WORKFLOW.md.")
			files, problems := doctorInstructionFiles(t.Context(), root, "Follow AGENTS.md.", "")
			if len(problems) != 0 {
				t.Fatal(problems)
			}
			got := checkDoctorGateInstructionConflict("p", "make check-fast", files, problems)
			if got.Status != tt.want {
				t.Fatalf("got %+v; want %s", got, tt.want)
			}
			count := 0
			for _, file := range files {
				if file.path == filepath.Join(root, "WORKFLOW.md") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("referenced workflow count = %d; want 1", count)
			}
		})
	}
}

func TestDoctorEffectiveWorkflowDeduplicated(t *testing.T) {
	for _, tt := range []struct {
		name               string
		external, distinct bool
		want               doctorStatus
	}{
		{"colocated", false, false, doctorOK},
		{"external", true, false, doctorOK},
		{"distinct local workflow", true, true, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := initDoctorWorkflowSourceRepository(t)
			agents := "Follow WORKFLOW.md."
			prompt := "The validation gate is make check-fast."
			source := filepath.Join(root, "WORKFLOW.md")
			if tt.external {
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				source = filepath.Join(t.TempDir(), "WORKFLOW.md")
			}
			writeDoctorWorkflowSourceFile(t, source, prompt)
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "AGENTS.md"), agents)
			if tt.distinct {
				writeDoctorWorkflowSourceFile(t, filepath.Join(root, "WORKFLOW.md"), "Do not run make check-fast.")
			}
			files, problems := doctorInstructionFiles(t.Context(), root, prompt, source)
			if len(problems) != 0 {
				t.Fatal(problems)
			}
			got := checkDoctorGateInstructionConflict("p", "make check-fast", files, problems)
			if got.Status != tt.want {
				t.Fatalf("got %+v; want %s", got, tt.want)
			}
			if !tt.distinct {
				total := 0
				for _, file := range files {
					total += file.bytes
				}
				if total != len(agents)+len(prompt) {
					t.Fatalf("instruction bytes = %d; want %d", total, len(agents)+len(prompt))
				}
			}
		})
	}
}
