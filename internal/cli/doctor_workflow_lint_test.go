package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestCheckDoctorWorkflowLintStaticLandmines(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	base := workflowconfig.Default()
	base.Workspace.SourceRoot = workdir
	base.Deliverable.Kind = workflowconfig.DeliverableArtifact

	tests := []struct {
		name       string
		cfg        workflowconfig.Config
		deps       doctorDeps
		wantName   string
		wantDetail []string
		wantHint   []string
	}{
		{
			name: "missing gate executable",
			cfg: func() workflowconfig.Config {
				cfg := base
				cfg.Gate.Run = "missing-check verify"
				return cfg
			}(),
			deps: doctorDeps{
				resolveCommandInDir: func(context.Context, string, []string, string) (string, error) {
					return "", errors.New("not found")
				},
			},
			wantName:   "workflow lint gate command",
			wantDetail: []string{"WORKFLOW.md", `gate.run "missing-check verify"`, `command -v "missing-check" failed`},
			wantHint:   []string{"Install the executable", "rerun detent doctor"},
		},
		{
			name: "missing make target",
			cfg:  base,
			deps: doctorDeps{
				resolveCommandInDir: func(context.Context, string, []string, string) (string, error) {
					return "/usr/bin/make", nil
				},
				runCommandInDir: func(_ context.Context, dir string, environment []string, path string, args ...string) error {
					return errors.New("No rule to make target 'check'")
				},
			},
			wantName:   "workflow lint gate command",
			wantDetail: []string{`dry-run "make -n check" failed`, "No rule to make target 'check'"},
			wantHint:   []string{"Add or fix the make target", "update gate.run"},
		},
		{
			name: "missing ship skill",
			cfg: func() workflowconfig.Config {
				cfg := workflowconfig.Default()
				cfg.Workspace.SourceRoot = workdir
				cfg.Gate.Kind = gate.KindHumanReview
				return cfg
			}(),
			deps: doctorDeps{
				lookupEnv: func(key string) string {
					if key == "CODEX_HOME" {
						return "/codex-home"
					}
					return ""
				},
				shipSkillProbe: func(root string) (doctorShipSkill, error) {
					return doctorShipSkill{}, errors.New("skills/ship/SKILL.md is missing")
				},
			},
			wantName:   "workflow lint ship skill",
			wantDetail: []string{"deliverable.kind=pull_request", "$go-workflow:ship", "skills/ship/SKILL.md is missing"},
			wantHint:   []string{"go-workflow@gopher-ai", ".codex-plugin/plugin.json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checks := checkDoctorWorkflowLint(context.Background(), "alpha", globalconfig.Project{
				ID:       "alpha",
				Workflow: "WORKFLOW.md",
				Workdir:  workdir,
			}, tt.cfg, "", doctorWorkflowDefaultTokenThreshold, "", tt.deps)
			if len(checks) != 1 {
				t.Fatalf("checks = %#v, want one warning", checks)
			}
			check := checks[0]
			if check.Status != doctorWarn || !strings.Contains(check.Name, tt.wantName) {
				t.Fatalf("check = %#v, want WARN named %q", check, tt.wantName)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
			for _, want := range tt.wantHint {
				if !strings.Contains(check.Hint, want) {
					t.Fatalf("Hint = %q, want containing %q", check.Hint, want)
				}
			}
		})
	}
}

func TestCheckDoctorWorkflowLintWarnsForPathFilteredRequiredCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		on          string
		wantWarning bool
		wantDetail  string
	}{
		{name: "pull request paths", on: "pull_request:\n    paths:\n      - '**/*.go'", wantWarning: true, wantDetail: "pull_request paths filter"},
		{name: "pull request types exclude synchronize", on: "pull_request:\n    types: [opened, reopened]", wantWarning: true, wantDetail: "types exclude synchronize"},
		{name: "pull request synchronize", on: "pull_request:\n    types: [opened, synchronize]"},
		{name: "unfiltered pull request", on: "pull_request:"},
		{name: "unfiltered push", on: "push:"},
		{name: "filtered push", on: "push:\n    branches: [main]", wantWarning: true, wantDetail: "push branch filter"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workdir := t.TempDir()
			workflowDir := filepath.Join(workdir, ".github", "workflows")
			if err := os.MkdirAll(workflowDir, 0o700); err != nil {
				t.Fatalf("mkdir workflows: %v", err)
			}
			workflow := []byte("name: CI\non:\n  " + tt.on + "\njobs:\n  test:\n    name: Test\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n")
			if err := os.WriteFile(filepath.Join(workflowDir, "ci.yml"), workflow, 0o600); err != nil {
				t.Fatalf("write workflow: %v", err)
			}
			cfg := workflowconfig.Default()
			cfg.Workspace.SourceRoot = workdir
			cfg.Gate.RequiredStatusChecks = []string{"Test"}

			checks := checkDoctorWorkflowLint(context.Background(), "alpha", globalconfig.Project{
				ID:       "alpha",
				Workflow: "WORKFLOW.md",
				Workdir:  workdir,
			}, cfg, "", doctorWorkflowDefaultTokenThreshold, "", doctorDeps{})

			if !tt.wantWarning {
				if len(checks) != 0 {
					t.Fatalf("checks = %#v, want none", checks)
				}
				return
			}
			if len(checks) != 1 {
				t.Fatalf("checks = %#v, want one warning", checks)
			}
			check := checks[0]
			if check.Status != doctorWarn || !strings.Contains(check.Name, "required check triggers") {
				t.Fatalf("check = %#v, want required check trigger warning", check)
			}
			for _, want := range []string{"Test", "ci.yml", tt.wantDetail, "every pull-request head"} {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
		})
	}
}

func TestCheckDoctorWorkflowLintWarnsForUnconfiguredLabelGatedRequiredCheck(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	workflowDir := filepath.Join(workdir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o700); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	workflow := []byte("name: CI\non:\n  pull_request:\n    types: [labeled, synchronize]\njobs:\n  test:\n    name: Test\n    runs-on: self-hosted\n    if: github.event.label.name == 'ci:ready'\n    steps:\n      - run: go test ./...\n")
	if err := os.WriteFile(filepath.Join(workflowDir, "ci.yml"), workflow, 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	cfg := workflowconfig.Default()
	cfg.Workspace.SourceRoot = workdir
	cfg.Gate.RequiredStatusChecks = []string{"Test"}

	checks := checkDoctorWorkflowLint(context.Background(), "alpha", globalconfig.Project{
		ID:       "alpha",
		Workflow: "WORKFLOW.md",
		Workdir:  workdir,
	}, cfg, "", doctorWorkflowDefaultTokenThreshold, "", doctorDeps{})

	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want one warning", checks)
	}
	check := checks[0]
	for _, want := range []string{"Test", "ci.yml", "label-gated", "gate.ci_trigger_label"} {
		if !strings.Contains(check.Detail+" "+check.Hint, want) {
			t.Fatalf("check = %#v, want containing %q", check, want)
		}
	}
}

func TestCheckDoctorWorkflowLintAcceptsConfiguredLabelGatedRequiredCheck(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name            string
		on              string
		condition       string
		configuredLabel string
		wantWarning     bool
		wantDetail      string
		wantHint        string
	}{
		{name: "explicit labeled and synchronize types", on: "on:\n  pull_request:\n    types: [labeled, synchronize]"},
		{name: "one of multiple matching labels", on: "on:\n  pull_request:\n    types: [labeled, synchronize]", condition: "github.event.label.name == 'ci:ready' || github.event.label.name == 'ci:full'", configuredLabel: "ci:full"},
		{name: "mismatched configured label", on: "on:\n  pull_request:\n    types: [labeled, synchronize]", configuredLabel: "ci:full", wantWarning: true, wantDetail: "does not match gate.ci_trigger_label", wantHint: "Set gate.ci_trigger_label to a label explicitly accepted"},
		{name: "pull request label list matches", on: "on:\n  pull_request:\n    types: [labeled]", condition: "contains(github.event.pull_request.labels.*.name, 'ci:ready')"},
		{name: "pull request label list mismatch", on: "on:\n  pull_request:\n    types: [labeled]", condition: "contains(github.event.pull_request.labels.*.name, 'ci:ready')", configuredLabel: "ci:full", wantWarning: true, wantDetail: "does not match gate.ci_trigger_label", wantHint: "Set gate.ci_trigger_label to a label explicitly accepted"},
		{name: "default pull request mapping", on: "on:\n  pull_request:", wantWarning: true, wantDetail: "job condition requires labeled event"},
		{name: "shorthand pull request", on: "on: pull_request", wantWarning: true, wantDetail: "job condition requires labeled event"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workdir := t.TempDir()
			workflowDir := filepath.Join(workdir, ".github", "workflows")
			if err := os.MkdirAll(workflowDir, 0o700); err != nil {
				t.Fatalf("mkdir workflows: %v", err)
			}
			condition := tt.condition
			if condition == "" {
				condition = "github.event.label.name == 'ci:ready'"
			}
			workflow := []byte("name: CI\n" + tt.on + "\njobs:\n  test:\n    name: Test\n    runs-on: self-hosted\n    if: " + condition + "\n    steps:\n      - run: go test ./...\n")
			if err := os.WriteFile(filepath.Join(workflowDir, "ci.yml"), workflow, 0o600); err != nil {
				t.Fatalf("write workflow: %v", err)
			}
			cfg := workflowconfig.Default()
			cfg.Workspace.SourceRoot = workdir
			cfg.Gate.RequiredStatusChecks = []string{"Test"}
			cfg.Gate.CITriggerLabel = tt.configuredLabel
			if cfg.Gate.CITriggerLabel == "" {
				cfg.Gate.CITriggerLabel = "ci:ready"
			}

			checks := checkDoctorWorkflowLint(context.Background(), "alpha", globalconfig.Project{
				ID:       "alpha",
				Workflow: "WORKFLOW.md",
				Workdir:  workdir,
			}, cfg, "", doctorWorkflowDefaultTokenThreshold, "", doctorDeps{})

			if tt.wantWarning {
				if len(checks) != 1 || !strings.Contains(checks[0].Detail, tt.wantDetail) {
					t.Fatalf("checks = %#v, want unsafe default trigger warning", checks)
				}
				if tt.wantHint != "" && !strings.Contains(checks[0].Hint, tt.wantHint) {
					t.Fatalf("Hint = %q, want containing %q", checks[0].Hint, tt.wantHint)
				}
				return
			}
			if len(checks) != 0 {
				t.Fatalf("checks = %#v, want explicit label gate to be accepted", checks)
			}
		})
	}
}

func TestDoctorWorkflowExecutableIndexSkipsEnvironmentAssignments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fields []string
		want   int
	}{
		{name: "plain command", fields: []string{"make", "check"}, want: 0},
		{name: "environment prefix", fields: []string{"CI=1", "PATH=/usr/bin", "make", "check"}, want: 2},
		{name: "invalid environment name", fields: []string{"9INVALID=value", "make"}, want: 0},
		{name: "empty", want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := doctorWorkflowExecutableIndex(tt.fields); got != tt.want {
				t.Errorf("doctorWorkflowExecutableIndex(%#v) = %d, want %d", tt.fields, got, tt.want)
			}
		})
	}
}

func TestCheckDoctorGateCommandPreservesEnvironmentAssignments(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	cfg := workflowconfig.Default()
	cfg.Workspace.SourceRoot = workdir
	cfg.Gate.Run = "CI=1 PATH=/bin:$PATH make check"
	var gotEnvironment []string
	var gotResolveEnvironment []string
	var gotArgs []string
	checks := checkDoctorGateCommand(context.Background(), "alpha", "WORKFLOW.md", globalconfig.Project{
		ID:      "alpha",
		Workdir: workdir,
	}, cfg, doctorDeps{
		resolveCommandInDir: func(_ context.Context, _ string, environment []string, _ string) (string, error) {
			gotResolveEnvironment = append([]string(nil), environment...)
			return "/usr/bin/make", nil
		},
		runCommandInDir: func(_ context.Context, _ string, environment []string, _ string, args ...string) error {
			gotEnvironment = append([]string(nil), environment...)
			gotArgs = append([]string(nil), args...)
			return nil
		},
	})
	if len(checks) != 0 {
		t.Fatalf("checks = %#v, want no warning", checks)
	}
	if strings.Join(gotEnvironment, " ") != "CI=1 PATH=/bin:$PATH" {
		t.Fatalf("environment = %#v, want CI and expanded PATH assignments", gotEnvironment)
	}
	if strings.Join(gotResolveEnvironment, " ") != "CI=1 PATH=/bin:$PATH" {
		t.Fatalf("resolve environment = %#v, want CI and expanded PATH assignments", gotResolveEnvironment)
	}
	if strings.Join(gotArgs, " ") != "-n check" {
		t.Fatalf("args = %#v, want make dry-run target", gotArgs)
	}
}

func TestCheckDoctorGateCommandProbesMakeBeforeShellSyntax(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	cfg := workflowconfig.Default()
	cfg.Workspace.SourceRoot = workdir
	cfg.Gate.Run = "make check 2>/dev/null"
	var gotArgs []string
	checks := checkDoctorGateCommand(context.Background(), "alpha", "WORKFLOW.md", globalconfig.Project{ID: "alpha", Workdir: workdir}, cfg, doctorDeps{
		resolveCommandInDir: func(context.Context, string, []string, string) (string, error) {
			return "/usr/bin/make", nil
		},
		runCommandInDir: func(_ context.Context, _ string, _ []string, _ string, args ...string) error {
			gotArgs = append([]string(nil), args...)
			return errors.New("No rule to make target 'check'")
		},
	})
	if strings.Join(gotArgs, " ") != "-n check" {
		t.Fatalf("args = %#v, want only the Make invocation before shell syntax", gotArgs)
	}
	if len(checks) != 1 || !strings.Contains(checks[0].Detail, `dry-run "make -n check" failed`) {
		t.Fatalf("checks = %#v, want missing Make target warning", checks)
	}
}

func TestDoctorCommandEnvironmentOverridesBaseValues(t *testing.T) {
	t.Parallel()

	got := doctorCommandEnvironment([]string{"PATH=/default", "HOME=/home/example", "CI=0"}, []string{"CI=1", "PATH=/project/bin:$PATH"})
	if strings.Join(got, " ") != "HOME=/home/example CI=1 PATH=/project/bin:/default" {
		t.Fatalf("doctorCommandEnvironment() = %#v, want overrides without duplicate variables", got)
	}
}

func TestDoctorCommandEnvironmentExpandsWindowsPathCaseInsensitively(t *testing.T) {
	t.Parallel()

	got := doctorCommandEnvironmentForOS(
		[]string{`Path=C:\\Windows\\System32`, "CI=0"},
		[]string{`PATH=bin;$PATH`},
		"windows",
	)
	if strings.Join(got, " ") != `CI=0 PATH=bin;C:\\Windows\\System32` {
		t.Fatalf("doctorCommandEnvironmentForOS() = %#v, want one expanded Windows PATH", got)
	}
}

func TestResolveDoctorWindowsCommandUsesConfiguredPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	for _, name := range []string{"project-check", "project-check.local"} {
		want := filepath.Join(binDir, name+".EXE")
		if err := os.WriteFile(want, []byte("example"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		got, err := resolveDoctorWindowsCommand(root, []string{
			`Path=C:\\Windows\\System32`,
			"PATH=bin",
			"PATHEXT=.EXE;.CMD",
		}, name)
		if err != nil {
			t.Fatalf("resolveDoctorWindowsCommand(%q) error = %v", name, err)
		}
		if got != want {
			t.Fatalf("resolveDoctorWindowsCommand(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCheckDoctorWorkflowLintWarnsForExplicitInertBlock(t *testing.T) {
	t.Parallel()

	workflow, err := workflowconfig.ParseWorkflow([]byte(`---
agent:
  budget:
    enabled: false
    per_issue_max_usd: 12
gate:
  kind: human_review
deliverable:
  kind: artifact
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	checks := checkDoctorWorkflowLint(context.Background(), "alpha", globalconfig.Project{
		ID:       "alpha",
		Workflow: "WORKFLOW.md",
	}, workflow.Config, "", doctorWorkflowDefaultTokenThreshold, "", doctorDeps{})
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want one inert agent budget warning", checks)
	}
	check := checks[0]
	for _, want := range []string{"WORKFLOW.md", "agent.budget.per_issue_max_usd", "agent.budget.enabled=false", "block is inert"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
		}
	}
	if !strings.Contains(check.Hint, "Set agent.budget.enabled: true") || !strings.Contains(check.Hint, "remove the configured agent.budget sub-settings") {
		t.Fatalf("Hint = %q, want one-line enable-or-remove fix", check.Hint)
	}
}

func TestCheckDoctorWorkflowLintWarnsForExplicitTopLevelBudgetSetting(t *testing.T) {
	t.Parallel()

	workflow, err := workflowconfig.ParseWorkflow([]byte(`---
budget:
  enabled: false
  pricing_path: custom/pricing.yaml
gate:
  kind: human_review
deliverable:
  kind: artifact
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	checks := checkDoctorWorkflowLint(context.Background(), "alpha", globalconfig.Project{
		ID:       "alpha",
		Workflow: "WORKFLOW.md",
	}, workflow.Config, "", doctorWorkflowDefaultTokenThreshold, "", doctorDeps{})
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want one inert top-level budget warning", checks)
	}
	check := checks[0]
	for _, want := range []string{"WORKFLOW.md", "budget.pricing_path", "budget.enabled=false", "block is inert"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
		}
	}
	if !strings.Contains(check.Hint, "Set budget.enabled: true") || !strings.Contains(check.Hint, "remove the configured budget sub-settings") {
		t.Fatalf("Hint = %q, want one-line enable-or-remove fix", check.Hint)
	}
}

func TestCheckDoctorWorkflowLintCleanWorkflowIsQuiet(t *testing.T) {
	t.Parallel()

	workdir := t.TempDir()
	cfg := workflowconfig.Default()
	cfg.Workspace.SourceRoot = workdir
	checks := checkDoctorWorkflowLint(context.Background(), "alpha", globalconfig.Project{
		ID:       "alpha",
		Workflow: "WORKFLOW.md",
		Workdir:  workdir,
	}, cfg, "", doctorWorkflowDefaultTokenThreshold, "", doctorDeps{
		resolveCommandInDir: func(context.Context, string, []string, string) (string, error) {
			return "/usr/bin/make", nil
		},
		runCommandInDir: func(context.Context, string, []string, string, ...string) error {
			return nil
		},
		lookupEnv: func(string) string { return "/codex-home" },
		shipSkillProbe: func(string) (doctorShipSkill, error) {
			return doctorShipSkill{Version: "1.6.0", Path: "/ship/SKILL.md"}, nil
		},
	})
	if len(checks) != 0 {
		t.Fatalf("checks = %#v, want clean workflow lint to be quiet", checks)
	}
}

func TestProbeDoctorShipSkillChecksEnablementCacheAndVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		config      string
		directory   string
		manifest    string
		createSkill bool
		wantVersion string
		wantError   string
	}{
		{name: "valid", config: "[plugins.\"go-workflow@gopher-ai\"]\nenabled = true\n", directory: "1.6.0", manifest: "1.6.0", createSkill: true, wantVersion: "1.6.0"},
		{name: "valid trailing comment", config: "[plugins.\"go-workflow@gopher-ai\"] # managed locally\nenabled = true\n", directory: "1.6.0", manifest: "1.6.0", createSkill: true, wantVersion: "1.6.0"},
		{name: "valid single quoted key", config: "[plugins.'go-workflow@gopher-ai']\nenabled = true\n", directory: "1.6.0", manifest: "1.6.0", createSkill: true, wantVersion: "1.6.0"},
		{name: "later matching section enabled", config: "[plugins.\"go-workflow@stale\"]\nenabled = false\n[plugins.\"go-workflow@gopher-ai\"]\nenabled = true\n", directory: "1.6.0", manifest: "1.6.0", createSkill: true, wantVersion: "1.6.0"},
		{name: "disabled", directory: "1.6.0", manifest: "1.6.0", createSkill: true, wantError: "plugin is not enabled"},
		{name: "different provider enabled", config: "[plugins.\"go-workflow@stale\"]\nenabled = true\n", directory: "1.6.0", manifest: "1.6.0", createSkill: true, wantError: "go-workflow@gopher-ai cache version"},
		{name: "version mismatch", config: "[plugins.\"go-workflow@gopher-ai\"]\nenabled = true\n", directory: "1.6.0", manifest: "1.5.0", createSkill: true, wantError: `version "1.5.0" does not match cache directory "1.6.0"`},
		{name: "ship skill missing", config: "[plugins.\"go-workflow@gopher-ai\"]\nenabled = true\n", directory: "1.6.0", manifest: "1.6.0", wantError: filepath.Join("skills", "ship", "SKILL.md") + " is missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			config := tt.config
			if config == "" {
				config = "[plugins.\"go-workflow@gopher-ai\"]\nenabled = false\n"
			}
			if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
				t.Fatalf("WriteFile(config.toml) error = %v", err)
			}
			cache := filepath.Join(root, "plugins", "cache", "gopher-ai", "go-workflow", tt.directory)
			if err := os.MkdirAll(filepath.Join(cache, ".codex-plugin"), 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			manifest := fmt.Sprintf(`{"name":"go-workflow","version":%q}`, tt.manifest)
			if err := os.WriteFile(filepath.Join(cache, ".codex-plugin", "plugin.json"), []byte(manifest), 0o600); err != nil {
				t.Fatalf("WriteFile(plugin.json) error = %v", err)
			}
			if tt.createSkill {
				skillDir := filepath.Join(cache, "skills", "ship")
				if err := os.MkdirAll(skillDir, 0o755); err != nil {
					t.Fatalf("MkdirAll(skill) error = %v", err)
				}
				if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: ship\n---\n"), 0o600); err != nil {
					t.Fatalf("WriteFile(SKILL.md) error = %v", err)
				}
			}

			got, err := probeDoctorShipSkill(root)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("probeDoctorShipSkill() error = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("probeDoctorShipSkill() error = %v", err)
			}
			if got.Version != tt.wantVersion || !strings.HasSuffix(got.Path, filepath.Join("skills", "ship", "SKILL.md")) {
				t.Fatalf("probeDoctorShipSkill() = %#v", got)
			}
		})
	}
}

func TestCheckDoctorWorkflowRuntimeLintFindsWaitDeadlockAndCeilingDeaths(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)
	db := openDoctorWorkflowLintDB(t)
	insertSchedulerDecision := func(at time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO scheduler_decisions (project_id, identifier, lane, result, reason, attempt_number, decision_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"alpha", "digitaldrywood/detent#1174", "Todo", "skipped", "artifact_gate_wait_status", 0, at.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert scheduler decision: %v", err)
		}
	}
	insertSchedulerDecision(now.Add(-20 * time.Minute))
	insertSchedulerDecision(now.Add(-time.Minute))
	for index := range 5 {
		errorMessage := ""
		totalTokens := int64(12_000_000 + index*100_000)
		if index < 2 {
			totalTokens = int64(16_100_000 + index*100_000)
			errorMessage = fmt.Sprintf("session token ceiling exceeded: total_tokens=%d ceiling_tokens=16000000 source=max_session_tokens", totalTokens)
		}
		if _, err := db.Exec(`INSERT INTO work_attempts (project_id, worker_type, completed_at, error_message, metrics_json) VALUES (?, ?, ?, ?, ?)`,
			"alpha", "agent", now.Add(-time.Duration(index)*time.Hour).Format(time.RFC3339Nano), errorMessage, fmt.Sprintf(`{"total_tokens":%d}`, totalTokens)); err != nil {
			t.Fatalf("insert work attempt: %v", err)
		}
	}
	for index := range 10 {
		if _, err := db.Exec(`INSERT INTO work_attempts (project_id, worker_type, completed_at, error_message, metrics_json) VALUES (?, ?, ?, '', ?)`,
			"alpha", "merge", now.Add(-time.Duration(index)*time.Minute).Format(time.RFC3339Nano), `{"total_tokens":25000000}`); err != nil {
			t.Fatalf("insert merge work attempt: %v", err)
		}
	}

	cfg := workflowconfig.Default()
	cfg.Gate.Kind = gate.KindArtifact
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	cfg.Polling.IntervalMS = 60_000
	cfg.Agent.MaxSessionTokens = 16_000_000
	checks := checkDoctorWorkflowRuntimeLint(context.Background(), "alpha", "/repo/WORKFLOW.md", "/runtime/detent.db", cfg, doctorDeps{
		openSQLiteReadOnly: func(context.Context, string) (doctorTelemetryStore, error) { return db, nil },
		now:                func() time.Time { return now },
	})
	if len(checks) != 2 {
		t.Fatalf("checks = %#v, want wait-status and ceiling warnings", checks)
	}
	wait := checks[0]
	for _, want := range []string{"/repo/WORKFLOW.md", "attempt==0", "artifact_gate_wait_status", "digitaldrywood/detent#1174", "20m0s", "2 skips"} {
		if !strings.Contains(wait.Detail, want) {
			t.Fatalf("wait Detail = %q, want containing %q", wait.Detail, want)
		}
	}
	ceiling := checks[1]
	for _, want := range []string{"max_session_tokens=16000000", "2 of 5", "40%", "token_ceiling_exceeded", "max observed tokens=16200000", "each death re-burns the full ceiling", "#1221"} {
		if !strings.Contains(ceiling.Detail, want) {
			t.Fatalf("ceiling Detail = %q, want containing %q", ceiling.Detail, want)
		}
	}
	if !strings.Contains(ceiling.Hint, "retry re-burns the full configured cap") {
		t.Fatalf("ceiling Hint = %q, want retry-cost assumption", ceiling.Hint)
	}
}

func TestCheckDoctorWorkflowRuntimeLintCleanHistoryIsQuiet(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)
	db := openDoctorWorkflowLintDB(t)
	if _, err := db.Exec(`INSERT INTO scheduler_decisions (project_id, identifier, lane, result, reason, attempt_number, decision_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"alpha", "digitaldrywood/detent#1", "Todo", "skipped", "artifact_gate_wait_status", 0, now.Add(-20*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert scheduler decision: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO scheduler_decisions (project_id, identifier, lane, result, reason, attempt_number, decision_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"alpha", "digitaldrywood/detent#1", "In Progress", "selected", "", 1, now.Add(-10*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert scheduler reset decision: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO scheduler_decisions (project_id, identifier, lane, result, reason, attempt_number, decision_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"alpha", "digitaldrywood/detent#1", "Todo", "skipped", "artifact_gate_wait_status", 0, now.Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert current scheduler decision: %v", err)
	}
	for index := range 5 {
		if _, err := db.Exec(`INSERT INTO work_attempts (project_id, worker_type, completed_at, error_message, metrics_json) VALUES (?, ?, ?, '', ?)`,
			"alpha", "agent", now.Add(-time.Duration(index)*time.Hour).Format(time.RFC3339Nano), `{"total_tokens":12000000}`); err != nil {
			t.Fatalf("insert work attempt: %v", err)
		}
	}
	cfg := workflowconfig.Default()
	cfg.Gate.Kind = gate.KindArtifact
	cfg.Agent.MaxSessionTokens = 16_000_000
	checks := checkDoctorWorkflowRuntimeLint(context.Background(), "alpha", "/repo/WORKFLOW.md", "/runtime/detent.db", cfg, doctorDeps{
		openSQLiteReadOnly: func(context.Context, string) (doctorTelemetryStore, error) { return db, nil },
		now:                func() time.Time { return now },
	})
	if len(checks) != 0 {
		t.Fatalf("checks = %#v, want clean history to be quiet", checks)
	}
}

func TestCheckDoctorWorkflowRuntimeLintTracksWaitIncidentAcrossLaneChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)
	db := openDoctorWorkflowLintDB(t)
	for _, decision := range []struct {
		lane string
		at   time.Time
	}{
		{lane: "Todo", at: now.Add(-20 * time.Minute)},
		{lane: "In Progress", at: now.Add(-time.Minute)},
	} {
		if _, err := db.Exec(`INSERT INTO scheduler_decisions (project_id, identifier, lane, result, reason, attempt_number, decision_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"alpha", "digitaldrywood/detent#1174", decision.lane, "skipped", "artifact_gate_wait_status", 0, decision.at.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert scheduler decision: %v", err)
		}
	}

	cfg := workflowconfig.Default()
	cfg.Gate.Kind = gate.KindArtifact
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	cfg.Polling.IntervalMS = 60_000
	checks := checkDoctorWorkflowRuntimeLint(context.Background(), "alpha", "/repo/WORKFLOW.md", "/runtime/detent.db", cfg, doctorDeps{
		openSQLiteReadOnly: func(context.Context, string) (doctorTelemetryStore, error) { return db, nil },
		now:                func() time.Time { return now },
	})
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want one continuous wait-status warning", checks)
	}
	for _, want := range []string{"digitaldrywood/detent#1174", "In Progress", "20m0s", "2 skips"} {
		if !strings.Contains(checks[0].Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", checks[0].Detail, want)
		}
	}
}

func TestCheckDoctorWorkflowRuntimeLintIgnoresDeathsBelowRaisedCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)
	db := openDoctorWorkflowLintDB(t)
	for index := range 5 {
		errorMessage := ""
		totalTokens := int64(12_000_000)
		if index < 2 {
			totalTokens = int64(16_100_000 + index*100_000)
			errorMessage = fmt.Sprintf("session token ceiling exceeded: total_tokens=%d ceiling_tokens=16000000 source=max_session_tokens", totalTokens)
		} else if index == 2 {
			totalTokens = 40_000_000
		}
		if _, err := db.Exec(`INSERT INTO work_attempts (project_id, worker_type, completed_at, error_message, metrics_json) VALUES (?, ?, ?, ?, ?)`,
			"alpha", "agent", now.Add(-time.Duration(index)*time.Hour).Format(time.RFC3339Nano), errorMessage, fmt.Sprintf(`{"total_tokens":%d}`, totalTokens)); err != nil {
			t.Fatalf("insert work attempt: %v", err)
		}
	}

	cfg := workflowconfig.Default()
	cfg.Agent.MaxSessionTokens = 32_000_000
	checks := checkDoctorWorkflowRuntimeLint(context.Background(), "alpha", "/repo/WORKFLOW.md", "/runtime/detent.db", cfg, doctorDeps{
		openSQLiteReadOnly: func(context.Context, string) (doctorTelemetryStore, error) { return db, nil },
		now:                func() time.Time { return now },
	})
	if len(checks) != 0 {
		t.Fatalf("checks = %#v, want old lower-cap deaths to stay quiet after remediation", checks)
	}
}

func TestDoctorWorkflowCapRecommendationStatesRetryCostAssumption(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	findings := doctorWorkflowOptimizationFindings("alpha", "/repo/WORKFLOW.md", cfg, doctorWorkflowOptimizationMetrics{
		SessionCount:        3,
		MedianSessionTokens: 1_000_000,
	})
	for _, finding := range findings {
		if finding.RuleID != doctorWorkflowRuleNoSessionTokenBrake {
			continue
		}
		if !strings.Contains(finding.Detail, doctorWorkflowCapRetryCostAssumption) || finding.Evidence["retry_cost_assumption"] != doctorWorkflowCapRetryCostAssumption {
			t.Fatalf("finding = %#v, want retry-cost assumption in detail and evidence", finding)
		}
		return
	}
	t.Fatalf("findings = %#v, want %s", findings, doctorWorkflowRuleNoSessionTokenBrake)
}

func openDoctorWorkflowLintDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close() error = %v", err)
		}
	})
	statements := []string{
		`CREATE TABLE scheduler_decisions (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  lane TEXT,
  result TEXT,
  reason TEXT,
  attempt_number INTEGER,
  decision_at TEXT NOT NULL
)`,
		`CREATE TABLE work_attempts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
	worker_type TEXT NOT NULL,
  completed_at TEXT,
  error_message TEXT,
  metrics_json TEXT NOT NULL DEFAULT '{}'
)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create workflow lint table: %v", err)
		}
	}
	return db
}
