package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorTokenAuditWorkflowSourceDrift(t *testing.T) {
	for _, tt := range []struct {
		name, ref, branch string
		modify            bool
		crlf              bool
		want              Status
	}{
		{"default branch", "", "main", false, false, OK},
		{"side branch", "", "side", false, false, Warn},
		{"pinned side branch", "origin/main", "side", false, false, OK},
		{"pinned content drift", "origin/main", "main", true, false, Warn},
		{"pinned CRLF checkout", "origin/main", "main", false, true, OK},
		{"missing ref", "missing", "main", false, false, Warn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := initDoctorWorkflowSourceRepository(t)
			if tt.branch != "main" {
				runDoctorWorkflowSourceGit(t, root, "checkout", "-b", tt.branch)
			}
			if tt.crlf {
				writeDoctorWorkflowSourceFile(t, filepath.Join(root, "WORKFLOW.md"), "Agent direction.\r\n")
			}
			if tt.modify {
				writeDoctorWorkflowSourceFile(t, filepath.Join(root, "WORKFLOW.md"), "Changed instructions")
			}
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "github"
			cfg.Tracker.Repository = "owner/repo"
			deps := Deps{GitHubRepositoryInfo: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error) {
				return ghconnector.RepositoryInfo{DefaultBranch: "main"}, nil
			}}
			got := CheckWorkflowSourceDrift(t.Context(), "p", globalconfig.Project{Workdir: root, Workflow: filepath.Join(root, "WORKFLOW.md"), WorkflowRef: tt.ref}, cfg, deps)
			if got.Status != tt.want || !strings.Contains(got.Detail, "bytes") {
				t.Fatalf("got %+v, want %s", got, tt.want)
			}
		})
	}
}

func TestDoctorTokenAuditInstructionBudget(t *testing.T) {
	for _, tt := range []struct {
		size int
		want Status
	}{
		{24 * 1024, OK}, {24*1024 + 1, Warn}, {48 * 1024, Warn}, {48*1024 + 1, Fail},
	} {
		t.Run(strconv.Itoa(tt.size), func(t *testing.T) {
			got := CheckInstructionBudget("p", []InstructionFile{{path: "WORKFLOW.md", bytes: tt.size}}, nil)
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
			files, problems := InstructionFiles(t.Context(), sub, prompt, "")
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
			files, problems := InstructionFiles(t.Context(), root, prompt, "")
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
			if got := CheckInstructionBudget("p", files, problems); got.Status != Warn {
				t.Fatalf("budget: %+v", got)
			}
			if got := CheckGateInstructionConflict("p", "make check", files, problems); got.Status != Warn {
				t.Fatalf("conflict: %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditGateConflict(t *testing.T) {
	for _, tt := range []struct {
		name, a, b string
		want       Status
	}{
		{"single", "Run make check.", "Other advice", OK},
		{"duplicate", "Run make check.", "make check is required.", Warn},
		{"contradiction", "Run make check.", "Do not run make check unless safety critical.", Warn},
		{"command boundary", "Run make check-fast.", "Run make check.", OK},
		{"mentions only", "Example: make check", "Example: make check", OK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckGateInstructionConflict("p", "make check", []InstructionFile{{path: "AGENTS.md", text: tt.a}, {path: "WORKFLOW.md", text: tt.b}}, nil)
			if got.Status != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestDoctorTokenAuditModelPolicy(t *testing.T) {
	cfg := workflowconfig.Config{}
	cfg.Agents.ModelSelection = workflowconfig.ModelSelection{Enabled: new(true), NormalModel: new("gpt-6-astra"), Levels: map[string]workflowconfig.ModelSelectionDefaults{"normal": {Model: new("normal"), Effort: new("low")}}}
	got := CheckModelPolicy("p", cfg)
	if got.Status != OK || !strings.Contains(got.Detail, "gpt-6-astra") || !strings.Contains(got.Detail, "normal_model=project") {
		t.Fatalf("got %+v", got)
	}
}

func TestDoctorTokenAuditNonGitInstructions(t *testing.T) {
	for _, name := range []string{"rules.go", "Makefile", "POLICY"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, name), "Run make check.")
			prompt := "Read `" + name + "`.\nReview the diff against `origin/main` for `Merging`; read `git diff origin/main...HEAD`.\nDo not run make check.\n" + strings.Repeat("x", 49*1024)
			files, problems := InstructionFiles(t.Context(), root, prompt, "")
			if len(files) != 2 {
				t.Fatalf("file count %d; problems %v", len(files), problems)
			}
			if got := CheckInstructionBudget("p", files, problems); got.Status != Fail {
				t.Fatalf("budget %+v", got)
			}
			if got := CheckGateInstructionConflict("p", "make check", files, problems); !strings.Contains(got.Detail, name) || !strings.Contains(got.Detail, "WORKFLOW.md") {
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
	deps := Deps{GitHubRepositoryInfo: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error) {
		return ghconnector.RepositoryInfo{DefaultBranch: "main"}, nil
	}}
	got := CheckWorkflowSourceDrift(t.Context(), "p", globalconfig.Project{Workdir: root, Workflow: filepath.Join(external, "WORKFLOW.md")}, cfg, deps)
	if got.Status != Warn || !strings.Contains(got.Detail, `checkout branch "side"`) || strings.Count(got.Detail, "17 bytes") != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestDoctorTokenAuditFastGateConflict(t *testing.T) {
	files := []InstructionFile{{path: "AGENTS.md", text: "Run make check."}, {path: "WORKFLOW.md", text: "Run make check-fast.\nDo not run make check unless safety-critical files change."}}
	got := CheckGateInstructionConflict("p", "make check-fast", files, nil)
	if got.Status != Warn || !strings.Contains(got.Detail, "full gate:") {
		t.Fatalf("got %+v", got)
	}
}

func TestDoctorReferencedWorkflowGateConflict(t *testing.T) {
	for _, tt := range []struct {
		name, agents string
		want         Status
	}{
		{"conflicting command", "Run make check.", Warn},
		{"configured command reference", "Use the configured gate.run.", OK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := initDoctorWorkflowSourceRepository(t)
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "AGENTS.md"), tt.agents+"\nFollow WORKFLOW.md.")
			writeDoctorWorkflowSourceFile(t, filepath.Join(root, "WORKFLOW.md"), "The validation gate is make check-fast.\nDo not run make check unless safety-critical files change.\nFollow AGENTS.md and WORKFLOW.md.")
			files, problems := InstructionFiles(t.Context(), root, "Follow AGENTS.md.", "")
			if len(problems) != 0 {
				t.Fatal(problems)
			}
			got := CheckGateInstructionConflict("p", "make check-fast", files, problems)
			if got.Status != tt.want {
				t.Fatalf("got %+v; want %s", got, tt.want)
			}
			count := 0
			workflowPath, err := filepath.EvalSymlinks(filepath.Join(root, "WORKFLOW.md"))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.path == workflowPath {
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
		want               Status
	}{
		{"colocated", false, false, OK},
		{"external", true, false, OK},
		{"distinct local workflow", true, true, Warn},
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
			files, problems := InstructionFiles(t.Context(), root, prompt, source)
			if len(problems) != 0 {
				t.Fatal(problems)
			}
			got := CheckGateInstructionConflict("p", "make check-fast", files, problems)
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
