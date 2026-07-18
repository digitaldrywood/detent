package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectDefinitionMigrationPreservesContentAndSemantics(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	legacy := "---\r\n# retained config comment\r\ntracker:\r\n  kind: memory\r\n  api_key: $DETENT_TEST_TOKEN\r\nagent:\r\n  instructions_by_state:\r\n    Todo: |\r\n      Keep this multiline value.\r\n---\r\n# Agent direction\r\n\r\nPreserve this body.\r\n"
	writeProjectDefinitionTestFile(t, workflowPath, legacy, 0o750)
	localPath := filepath.Join(dir, "WORKFLOW.local.md")
	local := "---\r\nagent:\r\n  max_turns: 12\r\n---\r\nLocal prose.\r\n"
	writeProjectDefinitionTestFile(t, localPath, local, 0o640)

	before, err := LoadProjectDefinition(workflowPath)
	if err != nil {
		t.Fatalf("LoadProjectDefinition(before) error = %v", err)
	}
	plan, err := PlanProjectDefinitionMigration(workflowPath)
	if err != nil {
		t.Fatalf("PlanProjectDefinitionMigration() error = %v", err)
	}
	if plan.Noop {
		t.Fatal("Noop = true, want migration")
	}
	if plan.SemanticDiff != "effective Detent configuration: unchanged" {
		t.Fatalf("SemanticDiff = %q", plan.SemanticDiff)
	}
	if err := ApplyProjectDefinitionMigration(plan); err != nil {
		t.Fatalf("ApplyProjectDefinitionMigration() error = %v", err)
	}

	assertProjectDefinitionFile(t, workflowPath, "# Agent direction\r\n\r\nPreserve this body.\r\n", 0o750)
	assertProjectDefinitionFile(t, localPath, "Local prose.\r\n", 0o640)
	detentRaw := readProjectDefinitionTestFile(t, filepath.Join(dir, "detent.yaml"))
	for _, want := range []string{
		"schema: 1\r\n",
		"# retained config comment\r\n",
		"api_key: $DETENT_TEST_TOKEN\r\n",
		"Todo: |-\r\n            Keep this multiline value.\r\n",
	} {
		if !strings.Contains(detentRaw, want) {
			t.Fatalf("detent.yaml = %q, want containing %q", detentRaw, want)
		}
	}
	localConfigRaw := readProjectDefinitionTestFile(t, filepath.Join(dir, "detent.local.yaml"))
	if localConfigRaw != "schema: 1\r\nagent:\r\n    max_turns: 12\r\n" {
		t.Fatalf("detent.local.yaml = %q", localConfigRaw)
	}

	after, err := LoadProjectDefinition(workflowPath)
	if err != nil {
		t.Fatalf("LoadProjectDefinition(after) error = %v", err)
	}
	if before.Prompt != after.Prompt {
		t.Fatalf("Prompt changed from %q to %q", before.Prompt, after.Prompt)
	}
	if before.Config.Agent.MaxTurns != after.Config.Agent.MaxTurns {
		t.Fatalf("MaxTurns changed from %d to %d", before.Config.Agent.MaxTurns, after.Config.Agent.MaxTurns)
	}

	second, err := PlanProjectDefinitionMigration(workflowPath)
	if err != nil {
		t.Fatalf("second PlanProjectDefinitionMigration() error = %v", err)
	}
	if !second.Noop || len(second.Operations) != 0 {
		t.Fatalf("second plan = %#v, want no-op", second)
	}
}

func TestProjectDefinitionMigrationGolden(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	input, err := os.ReadFile(filepath.Join("testdata", "migration", "legacy.WORKFLOW.md"))
	if err != nil {
		t.Fatalf("ReadFile(input) error = %v", err)
	}
	if err := os.WriteFile(workflowPath, input, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	plan, err := PlanProjectDefinitionMigration(workflowPath)
	if err != nil {
		t.Fatalf("PlanProjectDefinitionMigration() error = %v", err)
	}
	if err := ApplyProjectDefinitionMigration(plan); err != nil {
		t.Fatalf("ApplyProjectDefinitionMigration() error = %v", err)
	}
	assertProjectDefinitionGolden(t, workflowPath, filepath.Join("testdata", "migration", "split.WORKFLOW.md.golden"))
	assertProjectDefinitionGolden(t, DefinitionPath(workflowPath), filepath.Join("testdata", "migration", "split.detent.yaml.golden"))
}

func TestProjectDefinitionMigrationFailureLeavesOriginalsUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, workflowPath string)
	}{
		{
			name: "conflicting destination",
			run: func(t *testing.T, workflowPath string) {
				writeProjectDefinitionTestFile(t, DefinitionPath(workflowPath), "schema: 1\ntracker:\n  kind: github\n", 0o644)
				_, err := PlanProjectDefinitionMigration(workflowPath)
				if err == nil || !strings.Contains(err.Error(), "ambiguous authority") {
					t.Fatalf("PlanProjectDefinitionMigration() error = %v, want ambiguity", err)
				}
			},
		},
		{
			name: "validation failure",
			run: func(t *testing.T, workflowPath string) {
				invalid := "---\ntracker:\n  kind: unsupported\n---\nPrompt\n"
				writeProjectDefinitionTestFile(t, workflowPath, invalid, 0o644)
				_, err := PlanProjectDefinitionMigration(workflowPath)
				if err == nil || !strings.Contains(err.Error(), "validate legacy effective configuration") {
					t.Fatalf("PlanProjectDefinitionMigration() error = %v, want validation failure", err)
				}
				if got := readProjectDefinitionTestFile(t, workflowPath); got != invalid {
					t.Fatalf("WORKFLOW.md changed to %q", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			workflowPath := filepath.Join(dir, "WORKFLOW.md")
			original := "---\ntracker:\n  kind: memory\n---\nPrompt\n"
			writeProjectDefinitionTestFile(t, workflowPath, original, 0o644)
			tt.run(t, workflowPath)
			if got := readProjectDefinitionTestFile(t, workflowPath); got != original && tt.name != "validation failure" {
				t.Fatalf("WORKFLOW.md changed to %q", got)
			}
		})
	}
}

func TestProjectDefinitionMigrationRollsBackRenameFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	workflow := "---\ntracker:\n  kind: memory\n---\nPrompt\n"
	localPath := filepath.Join(dir, "WORKFLOW.local.md")
	local := "---\nagent:\n  max_turns: 12\n---\nLocal\n"
	writeProjectDefinitionTestFile(t, workflowPath, workflow, 0o644)
	writeProjectDefinitionTestFile(t, localPath, local, 0o600)

	plan, err := PlanProjectDefinitionMigration(workflowPath)
	if err != nil {
		t.Fatalf("PlanProjectDefinitionMigration() error = %v", err)
	}
	renames := 0
	injected := errors.New("injected rename failure")
	err = applyProjectDefinitionMigration(plan, projectDefinitionMigrationOS{
		rename: func(oldPath string, newPath string) error {
			renames++
			if renames == 4 {
				return injected
			}
			return os.Rename(oldPath, newPath)
		},
		remove: os.Remove,
	})
	if !errors.Is(err, injected) {
		t.Fatalf("applyProjectDefinitionMigration() error = %v, want injected failure", err)
	}
	assertProjectDefinitionFile(t, workflowPath, workflow, 0o644)
	assertProjectDefinitionFile(t, localPath, local, 0o600)
	for _, path := range []string{DefinitionPath(workflowPath), LocalDefinitionPath(workflowPath)} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("Stat(%q) error = %v, want not exist", path, statErr)
		}
	}
}

func TestProjectDefinitionMigrationRepairsLegacyLocalFrontmatterAfterSharedSplit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	writeProjectDefinitionTestFile(t, workflowPath, "Shared prompt.\n", 0o644)
	writeProjectDefinitionTestFile(t, DefinitionPath(workflowPath), "schema: 1\ntracker:\n  kind: memory\nagent:\n  max_turns: 10\n", 0o644)
	localPath := LocalWorkflowPath(workflowPath)
	writeProjectDefinitionTestFile(t, localPath, "---\nagent:\n  max_turns: 12\n---\nLocal prompt.\n", 0o600)

	plan, err := PlanProjectDefinitionMigration(workflowPath)
	if err != nil {
		t.Fatalf("PlanProjectDefinitionMigration() error = %v", err)
	}
	if plan.BeforeLayout != ProjectDefinitionMixed {
		t.Fatalf("BeforeLayout = %q, want mixed", plan.BeforeLayout)
	}
	if err := ApplyProjectDefinitionMigration(plan); err != nil {
		t.Fatalf("ApplyProjectDefinitionMigration() error = %v", err)
	}
	assertProjectDefinitionFile(t, workflowPath, "Shared prompt.\n", 0o644)
	assertProjectDefinitionFile(t, localPath, "Local prompt.\n", 0o600)
	workflow, err := LoadProjectDefinition(workflowPath)
	if err != nil {
		t.Fatalf("LoadProjectDefinition() error = %v", err)
	}
	if workflow.Definition.Layout != ProjectDefinitionSplit || workflow.Config.Agent.MaxTurns != 12 {
		t.Fatalf("workflow = %#v, want split with local max_turns=12", workflow)
	}
}

func assertProjectDefinitionFile(t *testing.T, path string, content string, mode os.FileMode) {
	t.Helper()
	if got := readProjectDefinitionTestFile(t, path); got != content {
		t.Fatalf("%s = %q, want %q", path, got, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
	}
}

func readProjectDefinitionTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(raw)
}

func assertProjectDefinitionGolden(t *testing.T, path string, goldenPath string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s = %q, want golden %q", path, got, want)
	}
}
