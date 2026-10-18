package detent

import (
	"os"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestLabelWorkflowTemplateOmitsWriteProbeIssue(t *testing.T) {
	t.Parallel()

	template := readRepositoryTextFile(t, "docs/templates/WORKFLOW.label.md")

	if strings.Contains(template, "write_probe_issue:") {
		t.Fatalf("WORKFLOW.label.md contains write_probe_issue default:\n%s", template)
	}
}

func TestWorkflowTemplatesUseSplitProjectDefinition(t *testing.T) {
	t.Parallel()

	for _, variant := range []string{"project_v2", "issue_field", "label"} {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			workflowPath := "docs/templates/WORKFLOW." + variant + ".md"
			workflowRaw, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Fatalf("ReadFile(%s) error = %v", workflowPath, err)
			}
			if strings.HasPrefix(string(workflowRaw), "---\n") {
				t.Fatalf("%s contains structured frontmatter", workflowPath)
			}
			configPath := "docs/templates/detent." + variant + ".yaml"
			configRaw, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile(%s) error = %v", configPath, err)
			}
			if !strings.HasPrefix(strings.ReplaceAll(string(configRaw), "\r\n", "\n"), "schema: 1\n") {
				t.Fatalf("%s missing schema version", configPath)
			}
			if _, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
				WorkflowPath: workflowPath,
				Workflow:     workflowRaw,
				ConfigPath:   configPath,
				Config:       configRaw,
				HasConfig:    true,
			}); err != nil {
				t.Fatalf("ParseProjectDefinition(%s) error = %v", variant, err)
			}
		})
	}
}

func TestWorkflowTemplatesReferenceAppendedContract(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"docs/templates/WORKFLOW.project_v2.md", "docs/templates/WORKFLOW.issue_field.md", "docs/templates/WORKFLOW.label.md"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			content := readRepositoryTextFile(t, path)
			assertContainsWords(t, content, "Detent-appended Blocked handoff")
			assertContainsWords(t, content, "The orchestrator owns all lane transitions")
			if strings.Contains(content, "```detent-status") || strings.Contains(content, "### For ") {
				t.Fatal("template duplicates handoff or legacy lanes")
			}
			if _, err := workflowconfig.ParseWorkflow([]byte(content)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRenderedGitHubWorkflowTemplatesRequireReadyNonDraftPR(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"docs/templates/WORKFLOW.project_v2.md",
		"docs/templates/WORKFLOW.issue_field.md",
		"docs/templates/WORKFLOW.label.md",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			content := readRepositoryTextFile(t, path)
			for _, want := range []string{"open a draft PR", "before marking ready", "Verify current-head check conclusions and reviews before reporting completion", "Report skipped PR checks as skipped", "require successful merge-group checks before merge", "Detent-appended Blocked handoff"} {
				assertContainsWords(t, content, want)
			}
			assertOrder(t, content, "open a draft PR", "before marking ready")

		})
	}
}

func TestWorkflowTemplatesDocumentWorkerModelAndSessionGuardChoices(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"docs/templates/WORKFLOW.project_v2.md",
		"docs/templates/WORKFLOW.issue_field.md",
		"docs/templates/WORKFLOW.label.md",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			content := readRepositoryTextFile(t, path)
			for _, want := range []string{
				"max_session_tokens:",
				"max_session_context_multiplier is an opt-in, coarse",
				"Optional model_reasoning_effort is unset because not every model accepts it.",
				"command: codex app-server # Provider default: upgrades automatically and avoids retirement breakage.",
				"model: \"\"",
			} {
				assertContainsWords(t, content, want)
			}
			if strings.Contains(content, "max_session_context_multiplier:") {
				t.Fatalf("%s emits opt-in max_session_context_multiplier:\n%s", path, content)
			}

			workflow, err := workflowconfig.ParseWorkflow([]byte(content))
			if err != nil {
				t.Fatalf("ParseWorkflow(%s) error = %v", path, err)
			}
			if workflow.Config.Codex.Command != "codex app-server" {
				t.Fatalf("Codex.Command = %q, want provider default", workflow.Config.Codex.Command)
			}
			if workflow.Config.Gate.Validator.Model != "" {
				t.Fatalf("Gate.Validator.Model = %q, want route/provider default", workflow.Config.Gate.Validator.Model)
			}
		})
	}
}

func TestWorkflowTemplatesRecommendRequiredExecutionFlow(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"docs/templates/WORKFLOW.project_v2.md",
		"docs/templates/WORKFLOW.issue_field.md",
		"docs/templates/WORKFLOW.label.md",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			content := readRepositoryTextFile(t, path)
			for _, want := range []string{"## Required Execution Flow", "Current Detent status: {{ issue.state }}", "### State: Todo", "### State: In Progress", "### State: Rework", "### State: Merging", "Detent-appended Blocked handoff"} {
				assertContains(t, content, want)
			}
			assertOrder(t, content, "### State: Todo", "### State: In Progress")
			assertOrder(t, content, "### State: In Progress", "### State: Rework")
			assertOrder(t, content, "### State: Rework", "### State: Merging")

		})
	}
}

func TestDocsDeclareProjectSpecificCIQualityGates(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"docs/templates/WORKFLOW.project_v2.md",
		"docs/templates/WORKFLOW.issue_field.md",
		"docs/templates/WORKFLOW.label.md",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			template := readRepositoryTextFile(t, path)
			for _, want := range []string{
				"## Project CI Quality Gates",
				"<required-stage-category>",
				"<project-command>",
				"<project-check-name>",
				"Whenever you touch CI configuration or perform a review",
				"Require passing PR-head checks when jobs run there",
				"For merge-group-only CI, report expected PR skips and require passing merge-group checks before merge",
				"Do not rely on Detent or `detent doctor` to infer required stages or inspect CI configuration",
			} {
				assertContainsWords(t, template, want)
			}
		})
	}
}

func readRepositoryTextFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	content := string(raw)
	if strings.HasPrefix(path, "docs/templates/WORKFLOW.") && strings.HasSuffix(path, ".md") {
		variant := strings.TrimSuffix(strings.TrimPrefix(path, "docs/templates/WORKFLOW."), ".md")
		configPath := "docs/templates/detent." + variant + ".yaml"
		configRaw, configErr := os.ReadFile(configPath)
		if configErr != nil {
			t.Fatalf("ReadFile(%s) error = %v", configPath, configErr)
		}
		config := strings.ReplaceAll(string(configRaw), "\r\n", "\n")
		config = strings.TrimPrefix(config, "schema: 1\n")
		return "---\n" + config + "---\n" + content
	}
	return content
}

func assertContains(t *testing.T, text string, want string) {
	t.Helper()

	if !strings.Contains(text, want) {
		t.Fatalf("document missing %q", want)
	}
}

func assertContainsWords(t *testing.T, text string, want string) {
	t.Helper()

	normalizedText := strings.Join(strings.Fields(text), " ")
	normalizedWant := strings.Join(strings.Fields(want), " ")
	assertContains(t, normalizedText, normalizedWant)
}

func assertOrder(t *testing.T, text string, before string, after string) {
	t.Helper()

	beforeIndex := strings.Index(text, before)
	if beforeIndex == -1 {
		t.Fatalf("document missing %q", before)
	}
	afterIndex := strings.Index(text, after)
	if afterIndex == -1 {
		t.Fatalf("document missing %q", after)
	}
	if beforeIndex > afterIndex {
		t.Fatalf("document places %q after %q", before, after)
	}
}
