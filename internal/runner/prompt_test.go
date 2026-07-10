package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/lessons"
	"github.com/digitaldrywood/detent/internal/notes"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestBuildPromptRendersAssignsLessonsAndSkills(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	lessonsPath := filepath.Join(workspace, ".detent", "lessons.md")
	if err := lessons.Append(lessonsPath, lessons.Entry{
		IssueNumber: "21",
		Title:       "Previous failure",
		FailureKind: "workspace HEAD did not advance",
		Symptom:     "Codex produced no diff",
		Hypothesis:  "The command failed before writing files.",
		Hint:        "Check generator aliases before editing.",
	}, lessons.AppendOptions{Date: time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("append lesson: %v", err)
	}

	attempt := 2
	autoBranch := true
	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Tracker: config.Tracker{
				Kind:        config.TrackerMemory,
				Endpoint:    "memory://local",
				ProjectSlug: "memory-project",
			},
			Workspace: config.Workspace{AutoBranch: false},
			Agent: config.Agent{
				Lessons: config.Lessons{
					Enabled: true,
					Path:    ".detent/lessons.md",
					RecallN: 1,
				},
				Skills: config.Skills{
					Enabled: true,
					Path:    ".detent/skills",
					Creation: config.SkillCreation{
						Enabled:         true,
						MaxDraftsPerRun: 1,
					},
				},
			},
		},
		Prompt: "Prompt for {{ issue.identifier }} via {{ tracker.kind }} attempt={{ attempt }} auto={{ workspace.auto_branch }} metadata={{ issue.author_id }} {{ issue.assignees }} {{ issue.fields }}",
	}, connector.Issue{
		ID:          "issue-21",
		Identifier:  "digitaldrywood/detent#21",
		Title:       "Build prompt",
		Description: "Wire prompt builder",
		AuthorID:    "author-1",
		Assignees:   []string{"reviewer-1", "reviewer-2"},
		Labels:      []string{"enhancement", "stage:s3"},
		Fields:      map[string]string{"Status": "Todo"},
	}, PromptOptions{
		Attempt:       &attempt,
		WorkspacePath: workspace,
		AutoBranch:    &autoBranch,
		AvailableSkills: []skills.Skill{
			{Name: "migrate", Description: "Add migrations.", WhenToUse: "Issue mentions schema changes.", BodyPath: "migrate.md"},
		},
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"Prompt for digitaldrywood/detent#21 via memory attempt=2 auto=true",
		"metadata=author-1 reviewer-1, reviewer-2 map[Status:Todo]",
		"## Lessons from prior runs",
		"Check generator aliases before editing.",
		"## Blocked handoff",
		"`status` must be exactly one of `in_progress`, `blocked`, or `complete`; no other value is valid.",
		"The block signals the current work state only. The project's configured flow decides any later review, gate-wait, or merge lane placement.",
		"dependencies/blocked_by",
		"```detent-status",
		"status: blocked",
		"status: complete",
		"Blocked by: #123",
		"Narrative Workpad sentences are never read as blockers",
		"## Validation gate",
		"Run `make check` from the workspace root",
		"In Merging, run a focused rebase/smoke gate after a clean rebase when the PR already passed current-head validation",
		"Keep blocker and human-action declarations in the structured detent-status block",
		"## Available skills",
		"- migrate — Issue mentions schema changes.",
		"## Skill creation loop",
		"Before final handoff, consider whether the successful run exposed",
		"Draft at most 1 candidate skill file under `.detent/skills/`",
		"rerun the required validation gate after the draft",
		"the draft skill enters future prompts only after humans review and merge it",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Add migrations.") {
		t.Fatalf("prompt included skill description, want only when_to_use:\n%s", prompt)
	}
}

func TestBuildPromptDocumentsWorkpadStatusContract(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{
		Prompt: "Base prompt",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#1106",
		Title:      "Document workpad status",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"`status` must be exactly one of `in_progress`, `blocked`, or `complete`; no other value is valid.",
		"The block signals the current work state only. The project's configured flow decides any later review, gate-wait, or merge lane placement.",
		"```detent-status\n" +
			"schema: 1\n" +
			"status: blocked\n" +
			"blockers:\n" +
			"  - ref: \"owner/repo#123\"\n" +
			"    reason: \"waiting for the dependency to merge\"\n" +
			"human_action: null\n" +
			"```",
		"```detent-status\n" +
			"schema: 1\n" +
			"status: complete\n" +
			"blockers: []\n" +
			"human_action: null\n" +
			"```",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	if strings.Contains(prompt, "never use Human Review") {
		t.Fatalf("prompt contains project-specific review policy:\n%s", prompt)
	}
}

func TestBuildPromptSkillCreationInstructionsAreConfigurable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		workflow  config.Workflow
		planOnly  bool
		want      []string
		forbidden []string
	}{
		{
			name: "enabled pull request workflow",
			workflow: config.Workflow{Config: config.Config{
				Agent: config.Agent{Skills: config.Skills{
					Enabled: true,
					Path:    ".detent/team-skills",
					Creation: config.SkillCreation{
						Enabled:         true,
						MaxDraftsPerRun: 2,
					},
				}},
			}},
			want: []string{
				"## Skill creation loop",
				"Draft at most 2 candidate skill files under `.detent/team-skills/`",
			},
		},
		{
			name: "disabled creation",
			workflow: config.Workflow{Config: config.Config{
				Agent: config.Agent{Skills: config.Skills{
					Enabled: true,
					Creation: config.SkillCreation{
						Enabled:         false,
						MaxDraftsPerRun: 1,
					},
				}},
			}},
			forbidden: []string{"## Skill creation loop"},
		},
		{
			name: "artifact workflow",
			workflow: config.Workflow{Config: config.Config{
				Deliverable: config.Deliverable{Kind: config.DeliverableArtifact},
				Agent: config.Agent{Skills: config.Skills{
					Enabled: true,
					Creation: config.SkillCreation{
						Enabled:         true,
						MaxDraftsPerRun: 1,
					},
				}},
			}},
			forbidden: []string{"## Skill creation loop"},
		},
		{
			name:     "plan only workflow",
			planOnly: true,
			workflow: config.Workflow{Config: config.Config{
				Agent: config.Agent{Skills: config.Skills{
					Enabled: true,
					Creation: config.SkillCreation{
						Enabled:         true,
						MaxDraftsPerRun: 1,
					},
				}},
			}},
			forbidden: []string{"## Skill creation loop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt, err := BuildPrompt(tt.workflow, connector.Issue{
				Identifier: "digitaldrywood/detent#931",
				Title:      "Skill creation loop",
			}, PromptOptions{
				PlanOnly: tt.planOnly,
			})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}

			for _, want := range tt.want {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt missing %q:\n%s", want, prompt)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("prompt contains %q:\n%s", forbidden, prompt)
				}
			}
		})
	}
}

func TestBuildPromptFollowupInstructionsAreConfigurable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		config      config.Config
		planOnly    bool
		wantPresent bool
	}{
		{name: "default pull request workflow", config: config.Default(), wantPresent: true},
		{name: "disabled", config: func() config.Config {
			cfg := config.Default()
			cfg.Agent.Followups.Enabled = false
			return cfg
		}()},
		{name: "artifact workflow", config: func() config.Config {
			cfg := config.Default()
			cfg.Deliverable.Kind = config.DeliverableArtifact
			return cfg
		}()},
		{name: "plan only workflow", config: config.Default(), planOnly: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt, err := BuildPrompt(config.Workflow{Config: tt.config}, connector.Issue{
				Identifier: "digitaldrywood/detent#1164",
				Title:      "Follow-up filing guidance",
			}, PromptOptions{PlanOnly: tt.planOnly})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}

			for _, text := range []string{
				"## Out-of-scope discoveries",
				"project's Backlog state",
				"fenced `detent-agent` block",
				"best-guess `effort`",
				"file the issue without a state and say so in the final handoff",
			} {
				if strings.Contains(prompt, text) != tt.wantPresent {
					t.Fatalf("prompt presence of %q = %t, want %t:\n%s", text, strings.Contains(prompt, text), tt.wantPresent, prompt)
				}
			}
		})
	}
}

func TestBuildPromptAppendsTeamKnowledge(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	globalPath := filepath.Join(root, "global.md")
	projectPath := filepath.Join(root, "project.md")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	if err := os.WriteFile(globalPath, []byte("Use allowlist terminology.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(global) error = %v", err)
	}
	if err := os.WriteFile(projectPath, []byte("Run project smoke tests.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(project) error = %v", err)
	}

	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Agent: config.Agent{
				Knowledge: config.Knowledge{
					Enabled:  true,
					MaxBytes: 4096,
					Sources: []config.KnowledgeSource{
						{Name: "Global", Path: globalPath},
						{Name: "Missing", Path: filepath.Join(root, "missing.md")},
						{Name: "Project", Path: projectPath},
					},
				},
			},
		},
		Prompt: "Base prompt",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#930",
		Title:      "Knowledge",
	}, PromptOptions{
		WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"## Team knowledge",
		"Shared context supplied by Detent configuration.",
		"### Global",
		"Use allowlist terminology.",
		"### Project",
		"Run project smoke tests.",
		"## Handoff notes",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "### Missing") {
		t.Fatalf("prompt includes missing knowledge source:\n%s", prompt)
	}
	if strings.Index(prompt, "Use allowlist terminology.") > strings.Index(prompt, "Run project smoke tests.") {
		t.Fatalf("prompt knowledge order = %q, want global before project", prompt)
	}
	if strings.Index(prompt, "## Team knowledge") > strings.Index(prompt, "## Handoff notes") {
		t.Fatalf("prompt places handoff notes before team knowledge:\n%s", prompt)
	}
}

func TestBuildPromptReturnsKnowledgeReadError(t *testing.T) {
	t.Parallel()

	_, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Agent: config.Agent{
				Knowledge: config.Knowledge{
					Enabled: true,
					Sources: []config.KnowledgeSource{{
						Name: "Directory",
						Path: t.TempDir(),
					}},
				},
			},
		},
		Prompt: "Base prompt",
	}, connector.Issue{Identifier: "digitaldrywood/detent#930"}, PromptOptions{})
	if err == nil {
		t.Fatal("BuildPrompt() error = nil, want knowledge read error")
	}
	if !strings.Contains(err.Error(), "read shared knowledge") {
		t.Fatalf("BuildPrompt() error = %v, want knowledge context", err)
	}
}

func TestBuildPromptAppendsNotesAndPriorAttempt(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	notesPath := filepath.Join(workspace, ".detent", "notes.md")
	if err := notes.Append(notesPath, notes.Entry{
		Title: "Implementation handoff",
		Body:  "Key file: internal/runner/prompt.go\nValidation: go test ./internal/runner",
	}, notes.AppendOptions{Now: time.Date(2026, 7, 2, 21, 45, 0, 0, time.UTC)}); err != nil {
		t.Fatalf("append note: %v", err)
	}

	prompt, err := BuildPrompt(config.Workflow{
		Prompt: "Base prompt",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#856",
		Title:      "Handoff notes",
	}, PromptOptions{
		WorkspacePath: workspace,
		PriorAttempt: PriorAttempt{
			Source: "auto_promote",
			Reason: "validator_rework",
			Validator: gate.ValidatorResult{
				Submitted: true,
				Verdict:   gate.ValidatorVerdictRework,
				Score:     0.42,
				Summary:   "Missing deterministic rework context.",
				Findings: []gate.Finding{{
					Severity: "p1",
					Body:     "Rework prompt does not include validator findings.",
					Path:     "internal/runner/prompt.go",
					Line:     44,
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"## Handoff notes",
		"verify important facts against the repository",
		"Maintain `.detent/notes.md`",
		"## 2026-07-02T21:45:00Z - Implementation handoff",
		"Key file: internal/runner/prompt.go",
		"## Prior attempt handoff",
		"- source: auto_promote",
		"- failing gate reason: validator_rework",
		"- validator verdict: rework",
		"- validator score: 0.42",
		"- validator summary: Missing deterministic rework context.",
		"p1: Rework prompt does not include validator findings. (internal/runner/prompt.go:44)",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptRendersGateAssignsAndInstructions(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Gate: gate.Config{
				Kind:          gate.KindHumanReview,
				ApprovalLabel: "Approved-By-Human",
			},
			Plan: gate.PlanConfig{
				Enabled:       true,
				ApprovalLabel: "Plan-Approved",
			},
		},
		Prompt: "Gate {{ gate.kind }} label={{ gate.approval_label }} run={{ gate.run }} ci={{ gate.ci_failure_action }} max={{ gate.validator.max_inline_diff_bytes }} plan={{ plan.approval_label }}",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#266",
		Title:      "Gate prompt",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"Gate human_review label=approved-by-human run= ci=skip max=65536 plan=plan-approved",
		"## Validation gate",
		"Keep the pull request in Human Review until a human applies label `approved-by-human`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptAppendsWorkflowInstructionsByState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		state     string
		want      string
		forbidden []string
	}{
		{
			name:      "todo",
			state:     "Todo",
			want:      "Prepare the research brief.",
			forbidden: []string{"Address review feedback.", "Run the merge checklist."},
		},
		{
			name:      "rework",
			state:     "Rework",
			want:      "Address review feedback.",
			forbidden: []string{"Prepare the research brief.", "Run the merge checklist."},
		},
		{
			name:      "merging",
			state:     "Merging",
			want:      "Run the merge checklist.",
			forbidden: []string{"Prepare the research brief.", "Address review feedback."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt, err := BuildPrompt(config.Workflow{
				Config: config.Config{
					Agent: config.Agent{
						InstructionsByState: map[string]string{
							"Todo":    "Prepare the research brief.",
							"Rework":  "Address review feedback.",
							"Merging": "Run the merge checklist.",
						},
					},
				},
				Prompt: "Base workflow prompt.",
			}, connector.Issue{
				Identifier: "digitaldrywood/detent#980",
				Title:      "Workflow instructions",
				State:      tt.state,
			}, PromptOptions{})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}

			for _, want := range []string{"## Workflow instructions", "### State: " + tt.state, tt.want} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt missing %q:\n%s", want, prompt)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("prompt contains %q:\n%s", forbidden, prompt)
				}
			}
		})
	}
}

func TestBuildPromptAppendsWorkflowInstructionsByTransitionBeforeDeliverableAndGate(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Deliverable: config.Deliverable{Kind: config.DeliverableArtifact},
			Agent: config.Agent{
				InstructionsByState: map[string]string{
					"In Progress": "Work from the implementation checklist.",
				},
				InstructionsByTransition: map[string]map[string]string{
					"Todo": {
						"In Progress": "Confirm dependencies before coding.",
					},
				},
			},
			Gate: gate.Config{Kind: gate.KindArtifact},
		},
		Prompt: "Base workflow prompt.",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#980",
		Title:      "Workflow instructions",
		State:      "In Progress",
	}, PromptOptions{
		DispatchSourceState: "Todo",
		DispatchTargetState: "In Progress",
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"## Workflow instructions",
		"### State: In Progress",
		"Work from the implementation checklist.",
		"### Transition: Todo -> In Progress",
		"Confirm dependencies before coding.",
		"## Deliverable",
		"## Validation gate",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	workflowInstructionsIndex := strings.Index(prompt, "## Workflow instructions")
	deliverableIndex := strings.Index(prompt, "## Deliverable")
	gateIndex := strings.Index(prompt, "## Validation gate")
	if workflowInstructionsIndex == -1 || deliverableIndex == -1 || gateIndex == -1 {
		t.Fatalf("prompt missing block markers:\n%s", prompt)
	}
	if workflowInstructionsIndex > deliverableIndex {
		t.Fatalf("workflow instructions appear after deliverable block:\n%s", prompt)
	}
	if workflowInstructionsIndex > gateIndex {
		t.Fatalf("workflow instructions appear after validation gate block:\n%s", prompt)
	}
}

func TestBuildPromptOmitsWorkflowInstructionsWhenUnconfigured(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{
		Prompt: "Base workflow prompt.",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#980",
		Title:      "Workflow instructions",
		State:      "Todo",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	if strings.Contains(prompt, "## Workflow instructions") {
		t.Fatalf("prompt contains workflow instructions when unconfigured:\n%s", prompt)
	}
	if !strings.HasPrefix(prompt, "Base workflow prompt.") {
		t.Fatalf("prompt prefix = %q, want base workflow prompt", prompt[:len("Base workflow prompt.")])
	}
}

func TestBuildValidatorPromptSeedsDiffContext(t *testing.T) {
	t.Parallel()

	patch := "diff --git a/README.md b/README.md\n+seeded validator diff\n"
	stat := workspace.DiffStat{Files: 1, Added: 1}

	tests := []struct {
		name      string
		opts      ValidatorPromptOptions
		want      []string
		forbidden []string
	}{
		{
			name: "inline diff under threshold",
			opts: ValidatorPromptOptions{
				DiffStat:           &stat,
				DiffPatch:          patch,
				MaxInlineDiffBytes: len(patch),
			},
			want: []string{
				"Diff context:",
				"Stat: 1 file changed, 1 insertion(+)",
				"Inline diff limit: " + strconv.Itoa(len(patch)) + " bytes (`gate.validator.max_inline_diff_bytes`).",
				"Inline diff (" + strconv.Itoa(len(patch)) + " bytes):",
				"+seeded validator diff",
			},
			forbidden: []string{"Full diff omitted because it exceeds"},
		},
		{
			name: "stat only above threshold",
			opts: ValidatorPromptOptions{
				DiffStat:           &stat,
				DiffPatch:          patch,
				MaxInlineDiffBytes: len(patch) - 1,
			},
			want: []string{
				"Diff context:",
				"Stat: 1 file changed, 1 insertion(+)",
				"Full diff omitted because it exceeds the inline diff limit.",
			},
			forbidden: []string{"+seeded validator diff"},
		},
		{
			name: "stat only when threshold disabled",
			opts: ValidatorPromptOptions{
				DiffStat:           &stat,
				DiffPatch:          patch,
				MaxInlineDiffBytes: 0,
			},
			want: []string{
				"Diff context:",
				"Inline diff limit: 0 bytes (`gate.validator.max_inline_diff_bytes`); full diff omitted.",
				"Full diff omitted because it exceeds the inline diff limit.",
			},
			forbidden: []string{"+seeded validator diff"},
		},
		{
			name: "stat only when provider truncated",
			opts: ValidatorPromptOptions{
				DiffStat:           &stat,
				DiffPatch:          "",
				DiffTruncated:      true,
				MaxInlineDiffBytes: len(patch),
			},
			want: []string{
				"Diff context:",
				"Stat: 1 file changed, 1 insertion(+)",
				"Full diff omitted because it exceeds the inline diff limit.",
			},
			forbidden: []string{"+seeded validator diff"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt := BuildValidatorPrompt(config.Workflow{}, connector.Issue{
				Identifier:  "digitaldrywood/detent#854",
				Title:       "Seed validator prompt",
				Description: "## Acceptance Criteria\n- Inline small diffs.",
			}, tt.opts)

			for _, want := range tt.want {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt missing %q:\n%s", want, prompt)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("prompt contains %q:\n%s", forbidden, prompt)
				}
			}
		})
	}
}

func TestBuildPromptPrependsWorkspaceIsolationBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		workspacePath string
		branch        string
	}{
		{
			name:          "project scoped issue branch",
			workspacePath: "/workspaces/detent-digitaldrywood_detent_527-74ece90926d1",
			branch:        "detent/detent-digitaldrywood_detent_527-74ece90926d1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt, err := BuildPrompt(config.Workflow{
				Prompt: "Issue prompt",
			}, connector.Issue{
				Identifier: "digitaldrywood/detent#527",
				Title:      "Prompt isolation",
			}, PromptOptions{
				WorkspacePath: tt.workspacePath,
				Branch:        tt.branch,
			})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}

			for _, want := range []string{
				"## Detent workspace isolation",
				"You are already isolated in a Detent-created git worktree at `" + tt.workspacePath + "` on branch `" + tt.branch + "`.",
				"The branch name format (`detent/<project>-<identifier>-<digest>`) is generated by Detent.",
				"Do not validate, compare, require, or block on branch-name format.",
				"Do not block on branch naming, workspace, or worktree prerequisites.",
				"Issue prompt",
			} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("prompt missing %q:\n%s", want, prompt)
				}
			}
			if !strings.HasPrefix(prompt, "## Detent workspace isolation") {
				t.Fatalf("prompt did not start with isolation block:\n%s", prompt)
			}
		})
	}
}

func TestBuildPromptUsesDefaultPromptDescriptionFallback(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{},
		Prompt: " \n",
	}, connector.Issue{
		Identifier: "MT-1",
		Title:      "Missing body",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"You are working on a Linear issue.",
		"Identifier: MT-1",
		"Title: Missing body",
		"No description provided.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("default prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptAppendsGitHubClosingReferenceInstruction(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{
		Prompt: "Base prompt",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#193",
		Title:      "Dedupe dispatch",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	if !strings.Contains(prompt, "Fixes #193") {
		t.Fatalf("prompt missing closing reference instruction:\n%s", prompt)
	}
}

func TestBuildPromptArtifactWorkflowOmitsPullRequestContract(t *testing.T) {
	t.Parallel()

	workspacePath := filepath.Join(t.TempDir(), "ad-1")
	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Workspace: config.Workspace{Kind: config.WorkspaceFilesystem},
			Deliverable: config.Deliverable{
				Kind:       config.DeliverableArtifact,
				OutputRoot: "/tmp/detent-renders",
				ReviewURL:  "http://127.0.0.1:8080/review/ad-1",
			},
			Gate: gate.Config{Kind: gate.KindArtifact},
		},
		Prompt: "Deliver {{ deliverable.kind }} from {{ workspace.kind }} status={{ issue.deliverable.validation_status }} store={{ issue.metadata.store }}",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#780",
		Title:      "Artifact prompt",
		Metadata:   map[string]string{"store": "creswood"},
		Deliverable: &connector.Deliverable{
			Kind:             "video_ad",
			Path:             "outputs/ad-1/manifest.json",
			ReviewURL:        "http://127.0.0.1:8080/review/ad-1",
			ValidationStatus: "pending",
			ExternalID:       "creative-101",
		},
	}, PromptOptions{
		WorkspacePath: workspacePath,
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"## Detent artifact workspace",
		"filesystem workspace at `" + workspacePath + "`",
		"Deliver artifact from filesystem status=pending store=creswood",
		"## Deliverable",
		"Produce artifact deliverables for this work item instead of a pull request.",
		"- configured output root: `/tmp/detent-renders`",
		"- work item artifact path: `outputs/ad-1/manifest.json`",
		"## Validation gate",
		"artifact status",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{"Fixes #780", "pull request in Human Review"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt contains %q, want omitted:\n%s", forbidden, prompt)
		}
	}
}

func TestBuildPromptRejectsUnknownTemplateVariables(t *testing.T) {
	t.Parallel()

	_, err := BuildPrompt(config.Workflow{
		Prompt: "Prompt {{ issue.missing }}",
	}, connector.Issue{Identifier: "MT-1"}, PromptOptions{})
	if err == nil {
		t.Fatal("BuildPrompt() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown template variable") {
		t.Fatalf("BuildPrompt() error = %v, want unknown variable", err)
	}
}

func TestBuildPromptRendersIssueFieldLookups(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name: "present field",
			fields: map[string]string{
				"Owner":  "team-a",
				"Status": "Ready",
			},
			want: "owner=team-a status=Ready",
		},
		{
			name: "empty field",
			fields: map[string]string{
				"Owner":  "team-b",
				"Status": "",
			},
			want: "owner=team-b missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt, err := BuildPrompt(config.Workflow{
				Prompt: "owner={{ issue.fields.Owner }} {% if issue.fields.Status %}status={{ issue.fields.Status }}{% else %}missing{% endif %}",
			}, connector.Issue{
				Identifier: "MT-1",
				Fields:     tt.fields,
			}, PromptOptions{})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}
			if !strings.HasPrefix(prompt, tt.want) {
				t.Fatalf("prompt = %q, want prefix %q", prompt, tt.want)
			}
		})
	}
}

func TestBuildPromptRendersNestedConditionals(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{
		Prompt: `{% if issue.description %}{{ issue.description }} {% if issue.title %}{{ issue.title }}{% endif %}{% else %}No body{% endif %}`,
	}, connector.Issue{
		Identifier: "MT-1",
		Title:      "Nested title",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	if !strings.HasPrefix(prompt, "No body") {
		t.Fatalf("prompt = %q, want No body prefix", prompt)
	}
	if strings.Contains(prompt, "{% endif %}") {
		t.Fatalf("prompt left template delimiter: %q", prompt)
	}
}

func TestBuildPromptIgnoresUnreadableLessons(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".detent", "lessons.md"), 0o755); err != nil {
		t.Fatalf("mkdir lessons path: %v", err)
	}

	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Agent: config.Agent{
				Lessons: config.Lessons{
					Enabled: true,
					Path:    ".detent/lessons.md",
					RecallN: 2,
				},
			},
		},
		Prompt: "Base prompt",
	}, connector.Issue{Identifier: "MT-1"}, PromptOptions{WorkspacePath: workspace})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	if strings.Contains(prompt, "## Lessons from prior runs") {
		t.Fatalf("prompt included unreadable lessons:\n%s", prompt)
	}
}
