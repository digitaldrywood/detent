package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestOnboardingInstructionBudget(t *testing.T) {
	t.Parallel()
	for _, preset := range []string{"label", "project_v2", "issue_field", "github_local", "non_code_artifact"} {
		for _, ui := range []bool{false, true} {
			name := preset + "/backend"
			if ui {
				name = preset + "/ui"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := initOnboardingWorkflowBuilderGitRepository(t, "https://github.com/acme/api.git")
				if ui {
					writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "views", "index.templ"), "package views\n")
				}
				answers := onboardingWorkflowBuilderVariantAnswers(root, preset, "review_gate", "GATE_RUN=printf 'gate' && false")
				// Effort answers are no longer prerequisites, even with admission enabled.
				var lines []string
				for line := range strings.SplitSeq(answers, "\n") {
					if !strings.HasPrefix(line, "EFFORT_") {
						lines = append(lines, line)
					}
				}
				result, err := buildOnboardingWorkflow(context.Background(), onboardingBuildWorkflowConfig{AnswersPath: writeOnboardingWorkflowBuilderAnswers(t, strings.Join(lines, "\n"))})
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Agents) >= 4000 || len(result.Agents)+len(result.Workflow) >= 10000 {
					t.Fatalf("instruction budget exceeded: agents=%d workflow=%d", len(result.Agents), len(result.Workflow))
				}
				for _, forbidden := range []string{"```detent-status", "### For ", "effort: high", "`xhigh`"} {
					if strings.Contains(result.Workflow+result.Agents, forbidden) {
						t.Fatalf("generated forbidden instruction %q", forbidden)
					}
				}
				wantBrowser := ui && preset != "non_code_artifact"
				if strings.Contains(result.Workflow, "browser / e2e") != wantBrowser {
					t.Fatalf("browser guidance = %s", result.Workflow)
				}
				if wantBrowser && !strings.Contains(result.Workflow, "Only when the diff touches the detected UI surface (views)") {
					t.Fatal("browser checks must depend on diff")
				}
				workflow, err := parseOnboardingBuildWorkflowResult(result)
				if err != nil {
					t.Fatal(err)
				}
				states := []string{"Todo", "In Progress", "Rework", "Merging"}
				if preset == "non_code_artifact" {
					states = []string{"Todo", "Production", "Rework"}
				}
				for _, state := range states {
					prompt, err := runner.BuildPrompt(workflow, connector.Issue{State: state}, runner.PromptOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if preset != "non_code_artifact" {
						if strings.Count(prompt, "## Validation gate") != 1 || strings.Contains(prompt, "bash -o pipefail -c") {
							t.Fatal("runtime gate must be the sole command execution authority")
						}
						if !strings.Contains(prompt, "In Merging, run a focused rebase/smoke gate") {
							t.Fatal("runtime merging optimization lost")
						}
					}
					// The appended runtime contract can mention states, but only the current
					// workflow lane heading should survive template rendering.
					if strings.Count(prompt, "### State:") != 1 || !strings.Contains(prompt, "### State: "+state) {
						t.Fatalf("state %s was not selected:\n%s", state, prompt)
					}
				}
				if preset == "non_code_artifact" {
					return
				}
				if strings.Contains(result.Workflow, workflow.Config.Gate.Run) {
					t.Fatal("workflow duplicates the runtime gate command")
				}
			})
		}
	}
}

func TestProbeOnboardingUISurfaces(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"index.html", "index.htm", "page.templ", "page.jsx", "page.tsx", "page.vue", "page.svelte", "server.go", "package.json"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeOnboardingWorkflowBuilderFile(t, filepath.Join(root, "src", file), "{}")
			probe, err := probeOnboardingRepository(root)
			if err != nil {
				t.Fatal(err)
			}
			want := file != "server.go" && file != "package.json"
			if (len(probe.UISurfacePaths) > 0) != want {
				t.Fatalf("UI paths=%v", probe.UISurfacePaths)
			}
		})
	}
}

func TestRefreshProjectInstructionTrims(t *testing.T) {
	t.Parallel()
	for _, ui := range []bool{false, true} {
		name := "backend"
		if ui {
			name = "ui"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newProjectRefreshTestFixture(t, "assisted_intake")
			if ui {
				writeOnboardingWorkflowBuilderFile(t, filepath.Join(fixture.root, "views", "index.html"), "<html></html>")
			}
			rawLegacy, err := os.ReadFile("testdata/onboarding/legacy-workflow.md")
			if err != nil {
				t.Fatal(err)
			}
			legacy := strings.Replace(string(rawLegacy), "## Required Execution Flow", "## Validation\n\nPreserve custom release signing verification.\n\n## Required Execution Flow\n\nKeep the operator's deployment policy.", 1)
			legacy = strings.Replace(legacy, "### For Merging", "### For Merging\n\nBefore merging, verify the release signature.", 1)
			legacy = strings.Replace(legacy, "## Project CI Quality Gates", "## Project CI Quality Gates\n\nKeep the custom CI mapping: lint, unit, integration.", 1)

			writeOnboardingWorkflowBuilderFile(t, fixture.workflowPath, legacy)
			writeOnboardingWorkflowBuilderFile(t, fixture.agentsPath, "# Project rules\n\n## Issue effort selection\n\neffort: high\n\n## Custom\n\nKeep me.\n")
			raw := readProjectRefreshTestFile(t, fixture.configPath)
			raw = strings.Replace(raw, "require_effort: false", "require_effort: true\n    effort_file: AGENTS.md\n    effort_section: Issue effort selection", 1)
			writeOnboardingWorkflowBuilderFile(t, fixture.configPath, raw)
			before := projectRefreshTestSnapshot(t, fixture)
			cfg := projectRefreshConfig{ConfigPath: fixture.globalPath, ProjectID: "api", Options: defaultOptions()}
			plan, err := planProjectRefresh(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			assertProjectRefreshTestSnapshot(t, fixture, before)
			workflow := string(projectRefreshTestChange(t, plan, fixture.workflowPath).after)
			for _, want := range []string{"Keep the operator's deployment policy.", "Keep the custom CI mapping", "Preserve custom release signing verification.", "Detent-appended Blocked handoff"} {
				if !strings.Contains(workflow, want) {
					t.Fatalf("missing %q", want)
				}
			}
			for _, bad := range []string{"```detent-status", "### For ", "make check again"} {
				if strings.Contains(workflow, bad) {
					t.Fatalf("retained %q", bad)
				}
			}
			if strings.Count(workflow, "Validation gate block is authoritative") != 1 || strings.Contains(workflow, "browser / e2e") != ui {
				t.Fatalf("incorrect refreshed instructions:\n%s", workflow)
			}
			agents := string(projectRefreshTestChange(t, plan, fixture.agentsPath).after)
			if strings.Contains(agents, "effort: high") || !strings.Contains(agents, "Keep me.") {
				t.Fatalf("agents=%s", agents)
			}
			if strings.Contains(workflow, "verify the release signature") {
				t.Fatal("lane-only policy leaked into shared prompt")
			}
			candidate := string(projectRefreshTestChange(t, plan, fixture.configPath).after)
			parsed, err := parseProjectRefreshConfig([]byte(candidate), []byte(workflow))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(parsed.Agent.InstructionsByState["Merging"], "verify the release signature") {
				t.Fatal("lost scoped merging policy")
			}
			if err := applyProjectRefresh(plan); err != nil {
				t.Fatal(err)
			}
			second, err := planProjectRefresh(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !second.Result.Noop {
				t.Fatalf("second refresh not idempotent:\n%s", second.Result.Diff)
			}
		})
	}
}

func TestOnboardingGuidanceWithoutEffort(t *testing.T) {
	t.Parallel()
	for _, enabled := range []string{"false", "true"} {
		t.Run(enabled, func(t *testing.T) {
			answers := onboardingAnswers{Values: map[string]string{"BACKLOG_ADMISSION_ENABLED": enabled}}
			if enabled == "true" {
				for _, field := range onboardingAdmissionGuidanceFields() {
					answers.Values[field.Key] = "Project criterion."
				}
			}
			if problems := validateOnboardingGuidanceAnswers(answers); len(problems) != 0 {
				t.Fatalf("unexpected guidance requirements: %v", problems)
			}
		})
	}
}

func TestRefreshPreservesTrackerAndAgentInstructions(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, text string }{
		{"local tracker", "This workflow uses `tracker.kind: github_local`. GitHub issues and pull\nrequests are read-only inputs. Detent status, claim fields, audit-trail\ncomments, and close decisions stay in the local SQLite database configured by\n`tracker.local_sqlite.path`; do not add `tracker.github_status_source` to this\nfile.\n"},
		{"custom agent guidance", "## Agent guidance\n\nNever deploy without operator approval.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimProjectRefreshHandoff(tt.text)
			if got != tt.text {
				t.Fatalf("tracker guidance changed: %s", got)
			}
			if tt.name == "custom agent guidance" {
				got, err := renderOnboardingAgentGuidance(tt.text, onboardingAnswers{})
				if err != nil || got != tt.text {
					t.Fatalf("agent guidance changed: %s, %v", got, err)
				}
			}
		})
	}
}

func TestRefreshRecognizesLegacyLocalLaneWording(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/onboarding/legacy-workflow.md")
	if err != nil {
		t.Fatal(err)
	}
	flow := string(raw)[strings.Index(string(raw), "## Required Execution Flow"):]
	for _, local := range []bool{false, true} {
		name := "github"
		if local {
			name = "github_local"
		}
		t.Run(name, func(t *testing.T) {
			input := flow
			if local {
				input = strings.NewReplacer("issues", "local issues", "issue", "local issue").Replace(input)
			}
			got := trimProjectRefreshHandoff(input)
			for _, line := range strings.Split(got, "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					t.Fatalf("retained legacy generated instruction: %s", line)
				}
			}
		})
	}
}

func TestRefreshPreservesUnreplacedCustomSections(t *testing.T) {
	t.Parallel()
	for _, heading := range []string{"## Browser verification", "## Blocked handoff"} {
		t.Run(heading, func(t *testing.T) {
			existing := heading + "\n\nKeep this custom policy.\n\n## Other\n\nOther policy.\n"
			got := refreshProjectWorkflow(existing, "## Validation\n\nGenerated validation.\n", workflowconfig.Config{})
			if !strings.Contains(got, existing) {
				t.Fatalf("custom section structure changed:\n%s", got)
			}
		})
	}
}
