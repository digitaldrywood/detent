package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestRefreshProjectCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		intake     string
		mutate     func(t *testing.T, fixture projectRefreshTestFixture)
		assertPlan func(t *testing.T, fixture projectRefreshTestFixture, plan projectRefreshPlan)
		assertFile func(t *testing.T, fixture projectRefreshTestFixture)
	}{
		{
			name:   "preserves explicit settings and dated comments",
			intake: "manual_intake",
			mutate: func(t *testing.T, fixture projectRefreshTestFixture) {
				t.Helper()
				raw := readProjectRefreshTestFile(t, fixture.configPath)
				raw = replaceProjectRefreshTestText(t, raw, "    github_graphql_warn_remaining: 500\n", "")
				raw = replaceProjectRefreshTestText(t, raw, "    merge_method: squash\n", "    merge_method: merge\n")
				raw = replaceProjectRefreshTestText(
					t,
					raw,
					"    max_concurrent_agents: 5\n",
					"    # 2026-07-14: Keep two slots so release work has headroom.\n    max_concurrent_agents: 2\n",
				)
				raw = replaceProjectRefreshTestText(t, raw, "    followups:\n        enabled: true\n", "")
				raw = strings.TrimRight(raw, "\n") + "\n# 2026-07-15: Admission remains operator-triggered.\nbacklog_admission:\n    enabled: false\n"
				writeOnboardingWorkflowBuilderFile(t, fixture.configPath, raw)
				writeOnboardingWorkflowBuilderFile(t, fixture.agentsPath, "# Project-owned agent guidance\n\n## Issue effort selection\n\nChoose effort after review.\n")
			},
			assertPlan: func(t *testing.T, fixture projectRefreshTestFixture, plan projectRefreshPlan) {
				t.Helper()
				assertProjectRefreshSetting(t, plan.Result.PreservedSettings, "deliverable.merge_method", "merge")
				assertProjectRefreshSetting(t, plan.Result.PreservedSettings, "agent.max_concurrent_agents", "2")
				assertProjectRefreshSetting(t, plan.Result.DefaultUpdates, "tracker.github_graphql_warn_remaining", "")
				assertProjectRefreshSetting(t, plan.Result.DefaultUpdates, "agent.followups.enabled", "")
				if !strings.Contains(plan.Result.Diff, "github_graphql_warn_remaining") {
					t.Fatalf("refresh diff does not show the new default:\n%s", plan.Result.Diff)
				}
				if !projectRefreshFeaturePresent(plan.Result.OptInFeatures, "agent.followups.enabled") {
					t.Fatalf("opt-in features = %#v, want follow-ups", plan.Result.OptInFeatures)
				}
				if !projectRefreshFeatureWithStatus(plan.Result.OptInFeatures, "backlog_admission.enabled", "explicitly disabled; preserved") {
					t.Fatalf("opt-in features = %#v, want preserved disabled admission", plan.Result.OptInFeatures)
				}
				candidate := projectRefreshTestChange(t, plan, fixture.configPath).after
				if !bytes.Contains(candidate, []byte("# 2026-07-14: Keep two slots so release work has headroom.")) {
					t.Fatalf("dated operator rationale was discarded:\n%s", candidate)
				}
				if !bytes.Contains(candidate, []byte(refreshManagedDefaultPrefix)) {
					t.Fatalf("new defaults are not marked for later refreshes:\n%s", candidate)
				}
				agents := projectRefreshTestChange(t, plan, fixture.agentsPath).after
				for _, want := range []string{"# Project-owned agent guidance", "## Issue effort selection", "```detent-agent", "`medium`", "`high`", "`xhigh`", "`max`"} {
					if !bytes.Contains(agents, []byte(want)) {
						t.Fatalf("refreshed AGENTS.md missing %q:\n%s", want, agents)
					}
				}
			},
			assertFile: func(t *testing.T, fixture projectRefreshTestFixture) {
				t.Helper()
				raw := readProjectRefreshTestFile(t, fixture.configPath)
				for _, want := range []string{
					"# 2026-07-14: Keep two slots so release work has headroom.",
					"max_concurrent_agents: 2",
					"merge_method: merge",
					"github_graphql_warn_remaining: 500",
					"followups:",
					"# 2026-07-15: Admission remains operator-triggered.",
				} {
					if !strings.Contains(raw, want) {
						t.Fatalf("refreshed config missing %q:\n%s", want, raw)
					}
				}
				parsed, err := parseProjectRefreshConfig([]byte(raw), []byte(readProjectRefreshTestFile(t, fixture.workflowPath)))
				if err != nil {
					t.Fatalf("parseProjectRefreshConfig() error = %v", err)
				}
				if parsed.Agent.Followups.Enabled {
					t.Fatal("Agent.Followups.Enabled = true, want opt-in feature to remain disabled")
				}
				if parsed.BacklogAdmission.Enabled {
					t.Fatal("BacklogAdmission.Enabled = true, want explicit disabled setting preserved")
				}
				agents := readProjectRefreshTestFile(t, fixture.agentsPath)
				if !strings.Contains(agents, "# Project-owned agent guidance") || !strings.Contains(agents, "## Issue effort selection") {
					t.Fatalf("refreshed AGENTS.md did not preserve project guidance and add the effort rubric:\n%s", agents)
				}
			},
		},
		{
			name:   "generates admission prerequisites with enabled config",
			intake: "assisted_intake",
			mutate: func(t *testing.T, fixture projectRefreshTestFixture) {
				t.Helper()
				raw := readProjectRefreshTestFile(t, fixture.workflowPath)
				marker := "\n## Admission Criteria\n"
				index := strings.Index(raw, marker)
				if index < 0 {
					t.Fatalf("fixture workflow does not contain admission criteria:\n%s", raw)
				}
				writeOnboardingWorkflowBuilderFile(t, fixture.workflowPath, strings.TrimRight(raw[:index], "\n")+"\n")
			},
			assertPlan: func(t *testing.T, fixture projectRefreshTestFixture, plan projectRefreshPlan) {
				t.Helper()
				change := projectRefreshTestChange(t, plan, fixture.workflowPath)
				for _, want := range []string{"## Admission Criteria", "### Alignment", "### Readiness", "### Size", "### Safety Gates"} {
					if !bytes.Contains(change.after, []byte(want)) {
						t.Fatalf("refreshed workflow missing %q:\n%s", want, change.after)
					}
				}
			},
			assertFile: func(t *testing.T, fixture projectRefreshTestFixture) {
				t.Helper()
				workflow, err := workflowconfig.LoadProjectDefinition(fixture.workflowPath)
				if err != nil {
					t.Fatalf("LoadProjectDefinition() after refresh error = %v", err)
				}
				if !workflow.Config.BacklogAdmission.Enabled {
					t.Fatal("BacklogAdmission.Enabled = false, want preserved true")
				}
				if !strings.Contains(workflow.SharedPrompt, "## Admission Criteria") {
					t.Fatalf("refreshed shared workflow missing admission criteria:\n%s", workflow.SharedPrompt)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newProjectRefreshTestFixture(t, tt.intake)
			tt.mutate(t, fixture)
			before := projectRefreshTestSnapshot(t, fixture)

			preview := executeProjectRefreshTestCommand(t, fixture.globalPath, false)
			if !strings.Contains(preview, "Diff:") || !strings.Contains(preview, "Preview only; no files changed") {
				t.Fatalf("preview output does not present a non-writing diff:\n%s", preview)
			}
			assertProjectRefreshTestSnapshot(t, fixture, before)

			plan, err := planProjectRefresh(context.Background(), projectRefreshConfig{
				ConfigPath: fixture.globalPath,
				ProjectID:  "api",
				Options:    defaultOptions(),
			})
			if err != nil {
				t.Fatalf("planProjectRefresh() error = %v", err)
			}
			if plan.Result.Noop || plan.Result.Diff == "" {
				t.Fatalf("refresh plan = %#v, want changes", plan.Result)
			}
			tt.assertPlan(t, fixture, plan)

			applied := executeProjectRefreshTestCommand(t, fixture.globalPath, true)
			if !strings.Contains(applied, "Refresh applied.") {
				t.Fatalf("apply output missing confirmation:\n%s", applied)
			}
			tt.assertFile(t, fixture)

			second, err := planProjectRefresh(context.Background(), projectRefreshConfig{
				ConfigPath: fixture.globalPath,
				ProjectID:  "api",
				Options:    defaultOptions(),
			})
			if err != nil {
				t.Fatalf("second planProjectRefresh() error = %v", err)
			}
			if !second.Result.Noop || second.Result.Diff != "" || len(second.changes) != 0 {
				t.Fatalf("second refresh = %#v with %d changes, want empty diff", second.Result, len(second.changes))
			}
		})
	}
}

func TestPlanProjectRefreshRejectsWorkflowRef(t *testing.T) {
	t.Parallel()

	fixture := newProjectRefreshTestFixture(t, "manual_intake")
	raw := readProjectRefreshTestFile(t, fixture.globalPath)
	raw = replaceProjectRefreshTestText(t, raw, "    workflow: "+fixture.workflowPath+"\n", "    workflow: "+fixture.workflowPath+"\n    workflow_ref: origin/main\n")
	writeOnboardingWorkflowBuilderFile(t, fixture.globalPath, raw)

	_, err := planProjectRefresh(context.Background(), projectRefreshConfig{
		ConfigPath: fixture.globalPath,
		ProjectID:  "api",
		Options:    defaultOptions(),
	})
	if err == nil || !strings.Contains(err.Error(), "workflow_ref-backed project") {
		t.Fatalf("planProjectRefresh() error = %v, want workflow_ref rejection", err)
	}
}

func TestApplyProjectRefreshRollsBackRenameFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		firstExists bool
	}{
		{name: "restores replaced file", firstExists: true},
		{name: "removes newly created file", firstExists: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			firstPath := filepath.Join(root, "first.md")
			secondPath := filepath.Join(root, "second.yaml")
			firstBefore := []byte("first before\n")
			if tt.firstExists {
				writeOnboardingWorkflowBuilderFile(t, firstPath, string(firstBefore))
			}
			firstPlanBefore := firstBefore
			if !tt.firstExists {
				firstPlanBefore = nil
			}
			secondBefore := []byte("second before\n")
			writeOnboardingWorkflowBuilderFile(t, secondPath, string(secondBefore))
			plan := projectRefreshPlan{changes: []projectRefreshChange{
				{path: firstPath, before: firstPlanBefore, after: []byte("first after\n"), mode: 0o644, existed: tt.firstExists},
				{path: secondPath, before: secondBefore, after: []byte("second after\n"), mode: 0o644, existed: true},
			}}

			failed := false
			rename := func(oldPath string, newPath string) error {
				if !failed && newPath == secondPath && strings.Contains(filepath.Base(oldPath), ".detent-refresh-") {
					failed = true
					return errors.New("injected rename failure")
				}
				return os.Rename(oldPath, newPath)
			}
			if err := applyProjectRefreshWithRename(plan, rename); err == nil || !strings.Contains(err.Error(), "injected rename failure") {
				t.Fatalf("applyProjectRefreshWithRename() error = %v, want injected failure", err)
			}

			first, err := os.ReadFile(firstPath)
			if tt.firstExists {
				if err != nil || !bytes.Equal(first, firstBefore) {
					t.Fatalf("first file after rollback = %q, %v; want %q", first, err, firstBefore)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new first file remains after rollback: %q, %v", first, err)
			}
			second, err := os.ReadFile(secondPath)
			if err != nil || !bytes.Equal(second, secondBefore) {
				t.Fatalf("second file after rollback = %q, %v; want %q", second, err, secondBefore)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("ReadDir() error = %v", err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".detent-refresh-") {
					t.Fatalf("refresh artifact remains after rollback: %s", entry.Name())
				}
			}
		})
	}
}

func TestProjectRefreshUnifiedHunks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		before string
		after  string
		want   []string
	}{
		{name: "addition", before: "one\ntwo\n", after: "one\nadded\ntwo\n", want: []string{"@@ -1,2 +1,3 @@", "+added"}},
		{name: "replacement", before: "one\nold\nthree\n", after: "one\nnew\nthree\n", want: []string{"-old", "+new"}},
		{name: "new file", after: "one\n", want: []string{"@@ -0,0 +1,1 @@", "+one"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := projectRefreshUnifiedHunks([]byte(tt.before), []byte(tt.after))
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("projectRefreshUnifiedHunks() missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestMergeProjectRefreshYAMLManagedDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		managed       bool
		want          string
		wantDefaults  int
		wantPreserved int
	}{
		{name: "updates untouched managed default", managed: true, want: "5", wantDefaults: 1},
		{name: "preserves explicit setting", want: "2", wantPreserved: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			existing, err := parseProjectRefreshYAML([]byte("agent:\n  max_concurrent_agents: 2 # 2026-07-16: Capacity rationale.\n"), "existing")
			if err != nil {
				t.Fatalf("parse existing YAML error = %v", err)
			}
			desired, err := parseProjectRefreshYAML([]byte("agent:\n  max_concurrent_agents: 5\n"), "desired")
			if err != nil {
				t.Fatalf("parse desired YAML error = %v", err)
			}
			agent := projectRefreshYAMLPathNode(existing, "agent")
			keyIndex := projectRefreshYAMLKeyIndex(agent, "max_concurrent_agents")
			if tt.managed {
				projectRefreshSetManagedDefault(agent.Content[keyIndex], agent.Content[keyIndex+1])
			}
			result := projectRefreshResult{}
			mergeProjectRefreshYAML(existing, desired, nil, nil, &result)
			value := projectRefreshYAMLPathNode(existing, "agent.max_concurrent_agents")
			if value == nil || value.Value != tt.want {
				t.Fatalf("max_concurrent_agents = %v, want %s", value, tt.want)
			}
			if len(result.DefaultUpdates) != tt.wantDefaults || len(result.PreservedSettings) != tt.wantPreserved {
				t.Fatalf("result = %#v, want %d defaults and %d preserved", result, tt.wantDefaults, tt.wantPreserved)
			}
			if value.LineComment != "# 2026-07-16: Capacity rationale." {
				t.Fatalf("line comment = %q, want dated rationale", value.LineComment)
			}
		})
	}
}

type projectRefreshTestFixture struct {
	root         string
	globalPath   string
	workflowPath string
	configPath   string
	agentsPath   string
}

func newProjectRefreshTestFixture(t *testing.T, intakeProfile string) projectRefreshTestFixture {
	t.Helper()

	root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
	answersPath := writeOnboardingWorkflowBuilderAnswers(t, onboardingWorkflowBuilderVariantAnswers(
		root,
		"label",
		"review_gate",
		"INTAKE_PROFILE="+intakeProfile,
	))
	generated, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{
		AnswersPath: answersPath,
		Write:       false,
	})
	if err != nil {
		t.Fatalf("buildOnboardingWorkflow() fixture error = %v", err)
	}
	fixture := projectRefreshTestFixture{
		root:         root,
		globalPath:   filepath.Join(root, "global.yaml"),
		workflowPath: filepath.Join(root, "WORKFLOW.md"),
		configPath:   filepath.Join(root, "detent.yaml"),
		agentsPath:   filepath.Join(root, "AGENTS.md"),
	}
	writeOnboardingWorkflowBuilderFile(t, fixture.workflowPath, generated.Workflow)
	writeOnboardingWorkflowBuilderFile(t, fixture.configPath, generated.Config)
	writeOnboardingWorkflowBuilderFile(t, fixture.agentsPath, generated.Agents)
	writeOnboardingWorkflowBuilderFile(t, fixture.globalPath, fmt.Sprintf(`apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 4
  scheduling: weighted
projects:
  - id: api
    workflow: %s
    workdir: %s
    weight: 1
    priority: 0
`, fixture.workflowPath, root))
	return fixture
}

func executeProjectRefreshTestCommand(t *testing.T, configPath string, confirmed bool) string {
	t.Helper()

	cmd := NewRootCommand(context.Background(), WithStdoutTTY(func() bool { return true }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	args := []string{"--config", configPath, "refresh-project", "api"}
	if confirmed {
		args = append(args, "--yes")
	}
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("detent %s error = %v", strings.Join(args, " "), err)
	}
	return stdout.String()
}

func projectRefreshTestSnapshot(t *testing.T, fixture projectRefreshTestFixture) map[string][]byte {
	t.Helper()

	return map[string][]byte{
		fixture.workflowPath: []byte(readProjectRefreshTestFile(t, fixture.workflowPath)),
		fixture.configPath:   []byte(readProjectRefreshTestFile(t, fixture.configPath)),
		fixture.agentsPath:   []byte(readProjectRefreshTestFile(t, fixture.agentsPath)),
	}
}

func assertProjectRefreshTestSnapshot(t *testing.T, fixture projectRefreshTestFixture, want map[string][]byte) {
	t.Helper()

	for path, expected := range want {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		if !bytes.Equal(got, expected) {
			t.Fatalf("preview changed %s", path)
		}
	}
}

func projectRefreshTestChange(t *testing.T, plan projectRefreshPlan, path string) projectRefreshChange {
	t.Helper()

	for _, change := range plan.changes {
		if change.path == path {
			return change
		}
	}
	t.Fatalf("refresh plan has no change for %s", path)
	return projectRefreshChange{}
}

func assertProjectRefreshSetting(t *testing.T, settings []projectRefreshSetting, path string, existing string) {
	t.Helper()

	for _, setting := range settings {
		if setting.Path != path {
			continue
		}
		if existing != "" && setting.Existing != existing {
			t.Fatalf("setting %s existing = %q, want %q", path, setting.Existing, existing)
		}
		return
	}
	t.Fatalf("settings = %#v, want path %s", settings, path)
}

func projectRefreshFeaturePresent(features []projectRefreshFeature, path string) bool {
	for _, feature := range features {
		if feature.Path == path {
			return true
		}
	}
	return false
}

func projectRefreshFeatureWithStatus(features []projectRefreshFeature, path string, status string) bool {
	for _, feature := range features {
		if feature.Path == path && feature.Status == status {
			return true
		}
	}
	return false
}

func replaceProjectRefreshTestText(t *testing.T, text string, old string, replacement string) string {
	t.Helper()

	if !strings.Contains(text, old) {
		t.Fatalf("fixture text missing %q:\n%s", old, text)
	}
	return strings.Replace(text, old, replacement, 1)
}

func readProjectRefreshTestFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(raw)
}
