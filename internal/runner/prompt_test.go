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
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeIssuePromptOwnership(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"native", "github_compatible", ""} {
		t.Run(profile, func(t *testing.T) {
			issue := connector.Issue{ID: "wi_example", Identifier: "prj_example#12", Metadata: map[string]string{"hub_profile": profile, "hub_project_id": "prj_example"}}
			prompt, err := BuildPrompt(config.Workflow{}, issue, PromptOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if profile == "native" {
				for _, want := range []string{"detent hub issue", "--project prj_example", "Detent-Work-Item: wi_example", "Native approval does not satisfy a required GitHub review"} {
					if !strings.Contains(prompt, want) {
						t.Errorf("missing %q", want)
					}
				}
				if strings.Contains(prompt, "Fixes #12") {
					t.Fatal("native number generated a GitHub closing reference")
				}
			} else if strings.Contains(prompt, "Native Detent issue authority") {
				t.Fatal("compatibility workflow changed")
			}
		})
	}
}

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
		"dependencies/blocked_by",
		"```detent-status",
		"status: blocked",
		"status: complete",
		"Blocked by: #123",
		"Narrative Workpad sentences are never read as blockers",
		"## Validation gate",
		"Run `make check` from the workspace root",
		"## Available skills",
		"- migrate",
		"## Skill creation loop",
		"Draft only reusable methods",
		"Rerun validation after drafting; PR review approves it.",
		"Skill draft: yes",
		"Skill draft: no",
		"Draft at most 1 candidate skill file under `.detent/skills/`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Add migrations.") || strings.Contains(prompt, "Issue mentions schema changes.") {
		t.Fatalf("prompt included skill description, want names only:\n%s", prompt)
	}
}

func TestBuildRoutinePromptUsesConfiguredCriteriaAndProposalContract(t *testing.T) {
	t.Parallel()
	prompt, err := BuildRoutinePrompt(config.Workflow{Config: config.Config{
		Tracker: config.Tracker{Kind: config.TrackerMemory},
	}}, connector.Issue{Identifier: "detent/routine/dependency-audit"}, RoutineRequest{
		Name: "dependency-audit", Schedule: "0 3 * * 1", Prompt: "Follow the dependency criteria in WORKFLOW.md.",
	}, PromptOptions{WorkspacePath: "/tmp/detent", Branch: "main"})
	if err != nil {
		t.Fatalf("BuildRoutinePrompt() error = %v", err)
	}
	for _, want := range []string{
		"Routine: dependency-audit",
		"Schedule: 0 3 * * 1",
		"Follow the dependency criteria in WORKFLOW.md.",
		"propose_maintenance_issue",
		`{"issues":[{"dedup_key":"stable-key"`,
		`{"issues":[]}`,
		"Do not create or edit tracker issues directly.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{"## Validation gate", "## Skill creation loop", "## Blocked handoff"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt contains issue-workflow section %q:\n%s", unwanted, prompt)
		}
	}
}

func TestBuildPromptSkillCreationUsesDefaultConfigForPullRequestsOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		planOnly bool
		want     bool
	}{
		{name: "pull request run", want: true},
		{name: "plan only run", planOnly: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt, err := BuildPrompt(config.Workflow{
				Config: config.Default(),
				Prompt: "Base prompt",
			}, connector.Issue{
				Identifier: "digitaldrywood/detent#1166",
				Title:      "Tune skill creation",
			}, PromptOptions{PlanOnly: tt.planOnly})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}

			if got := strings.Contains(prompt, "## Skill creation loop"); got != tt.want {
				t.Fatalf("skill creation block present = %v, want %v:\n%s", got, tt.want, prompt)
			}
		})
	}
}

func TestBuildPromptIncludesMergeMethod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		deliverable   config.Deliverable
		mergeFallback bool
		want          string
		wantBlock     bool
	}{
		{name: "omitted defaults to squash", want: "This project merges pull requests with: `squash`.", wantBlock: true},
		{name: "squash", deliverable: config.Deliverable{MergeMethod: config.MergeMethodSquash}, want: "This project merges pull requests with: `squash`.", wantBlock: true},
		{name: "merge", deliverable: config.Deliverable{MergeMethod: config.MergeMethodMerge}, want: "This project merges pull requests with: `merge`.", wantBlock: true},
		{name: "rebase", deliverable: config.Deliverable{MergeMethod: config.MergeMethodRebase}, want: "This project merges pull requests with: `rebase`.", wantBlock: true},
		{name: "merge fallback defaults to squash", mergeFallback: true, want: "This project merges pull requests with: `squash`.", wantBlock: true},
		{name: "merge fallback uses configured method", deliverable: config.Deliverable{MergeMethod: config.MergeMethodMerge}, mergeFallback: true, want: "This project merges pull requests with: `merge`.", wantBlock: true},
		{name: "artifact omits merge strategy", deliverable: config.Deliverable{Kind: config.DeliverableArtifact}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prompt, err := BuildPrompt(config.Workflow{
				Config: config.Config{Deliverable: tt.deliverable},
				Prompt: "Base prompt",
			}, connector.Issue{Identifier: "digitaldrywood/detent#1270"}, PromptOptions{MergeFallback: tt.mergeFallback})
			if err != nil {
				t.Fatalf("BuildPrompt() error = %v", err)
			}
			if got := strings.Contains(prompt, "## Merge strategy"); got != tt.wantBlock {
				t.Fatalf("merge strategy block present = %v, want %v:\n%s", got, tt.wantBlock, prompt)
			}
			if tt.want != "" && !strings.Contains(prompt, tt.want) {
				t.Fatalf("prompt missing %q:\n%s", tt.want, prompt)
			}
		})
	}
}

func TestSkillDraftProposed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "yes decision", output: "Tests pass.\n\nSkill draft: yes — `.detent/skills/debug.md` captures the workflow.", want: true},
		{name: "case and whitespace", output: "  SKILL DRAFT: YES  ", want: true},
		{name: "no decision", output: "Skill draft: no — routine edit."},
		{name: "discussion is not a proposal", output: "The prompt should require Skill draft: yes when appropriate."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := skillDraftProposed(tt.output); got != tt.want {
				t.Fatalf("skillDraftProposed(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestBuildPromptDocumentsWorkpadStatusContract(t *testing.T) {
	t.Parallel()
	for _, opts := range []PromptOptions{{}, {WorkAttemptID: 5715, Generation: 68}} {
		t.Run(strconv.FormatInt(opts.WorkAttemptID, 10), func(t *testing.T) {
			prompt := appendBlockedHandoffBlock("", opts)
			blocks := strings.Split(prompt, "```detent-status\n")[1:]
			if len(blocks) != 2 {
				t.Fatalf("got %d examples, want 2", len(blocks))
			}
			for i, block := range blocks {
				content := strings.SplitN(block, "```", 2)[0]
				for _, ref := range []string{"owner/repo#123", "#123"} {
					signal, err := workpad.ParseStatusBlock(strings.ReplaceAll(content, "owner/repo#123", ref), "owner/repo")
					if err != nil {
						t.Fatal(err)
					}
					if len(signal.UnknownKeys) != 0 {
						t.Fatalf("unknown fields: %v", signal.UnknownKeys)
					}
					if i == 0 && (signal.Status != workpad.StatusBlocked || signal.Blockers[0].Identifier != "owner/repo#123" || signal.Blockers[0].Unverifiable) {
						t.Fatalf("invalid dependency signal: %+v", signal)
					}
					if i == 1 && signal.Status != workpad.StatusComplete {
						t.Fatalf("invalid completion: %+v", signal)
					}
				}
			}
		})
	}
}

func TestPromptWrapperBytes(t *testing.T) {
	t.Parallel()
	loaded, err := skills.Load("../..", skills.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Skills) != skills.DefaultMaxSkillsInPrompt {
		t.Fatalf("fixture needs capped skills: %d", len(loaded.Skills))
	}
	prompt, err := BuildPrompt(config.Workflow{Prompt: "WORKFLOW", Config: config.Default()}, connector.Issue{Identifier: "digitaldrywood/detent#2662"}, PromptOptions{WorkspacePath: t.TempDir(), Branch: "detent/detent-digitaldrywood_detent_2662-4373c74c714b", AvailableSkills: loaded.Skills, WorkAttemptID: 5715, Generation: 68})
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range strings.Split(prompt, "\n## ") {
		t.Logf("section %s: %d", strings.SplitN(section, "\n", 2)[0], len(section))
	}
	size := len(prompt) - len("WORKFLOW")
	t.Logf("wrapper=%d bytes, handoff=%d bytes, skills=%d bytes", size, len(appendBlockedHandoffBlock("", PromptOptions{})), len(AvailableSkillsBlock(loaded.Skills)))
	if size >= 6000 {
		t.Errorf("wrapper is %d bytes, want under 6000", size)
	}
}

func TestBuildPromptBindsCompletionToCurrentAttempt(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{Prompt: "Base prompt"}, connector.Issue{
		Identifier: "digitaldrywood/detent#1906",
		Title:      "Fence completion",
	}, PromptOptions{WorkAttemptID: 3295, Generation: 7})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	for _, want := range []string{
		"The orchestrator is the only writer of tracker lane state.",
		"completion_work_attempt_id: \"3295\"",
		"completion_generation: \"7\"",
		"Never change lane labels or status fields",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
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
		"Verify prior notes",
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

func TestBuildPromptRequiresExplanationBeforeBreakerRetry(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{Prompt: "Base prompt"}, connector.Issue{
		Identifier: "gopherguides/gopher-ai#214",
	}, PromptOptions{PriorAttempt: PriorAttempt{
		Source:                  "spend_since_progress_circuit_breaker",
		Reason:                  "spend exceeded the configured limit without an accepted state change",
		ExplainBeforeRetry:      true,
		MissingSignal:           "lane transition or pull request signature change",
		ObservedTokens:          25_000_000,
		NoProgressTokenLimit:    20_000_000,
		ObservedSpendUSD:        6.75,
		NoProgressSpendLimitUSD: 5,
	}})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	for _, want := range []string{
		"### Explain before retry",
		"first tool action must update the Workpad",
		"Do not use any other tools",
		"lane transition or pull request signature change",
		"tokens since last accepted state change: 25000000",
		"configured token limit: 20000000",
		"notional USD since last accepted state change: $6.75",
		"configured notional USD limit: $5.00",
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
				Kind:           gate.KindHumanReview,
				ApprovalLabel:  "Approved-By-Human",
				CITriggerLabel: "CI:Ready",
			},
			Plan: gate.PlanConfig{
				Enabled:       true,
				ApprovalLabel: "Plan-Approved",
			},
		},
		Prompt: "Gate {{ gate.kind }} label={{ gate.approval_label }} trigger={{ gate.ci_trigger_label }} stagger={{ gate.ci_trigger_label_stagger_seconds }} run={{ gate.run }} ci={{ gate.ci_failure_action }} max={{ gate.validator.max_inline_diff_bytes }} plan={{ plan.approval_label }}",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#266",
		Title:      "Gate prompt",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"Gate human_review label=approved-by-human trigger=ci:ready stagger=15 run= ci=skip max=65536 plan=plan-approved",
		"## Validation gate",
		"Keep the pull request in Human Review until a human applies label `approved-by-human`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildPromptIncludesGitHubTrackerHostnameInCITriggerCommand(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{Config: config.Config{
		Tracker: config.Tracker{Kind: config.TrackerGitHub, Endpoint: "https://github.example.com/api/graphql"},
		Gate: gate.Config{
			Kind:           gate.KindCommand,
			CITriggerLabel: "CI Ready",
		},
	}}, connector.Issue{Identifier: "owner/repo#42", Title: "Enterprise label gate"}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	for _, want := range []string{"--label-base64 Y2kgcmVhZHk", "--hostname-base64 Z2l0aHViLmV4YW1wbGUuY29t", "--stagger-seconds 15"} {
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
				"Detent's worktree `" + tt.workspacePath + "` and branch `" + tt.branch + "` satisfy isolation.",
				"Never require different branch naming or workspace prerequisites.",
				"Use `TMPDIR`/`TMP`/`TEMP` for all scratch output",
				"Never use host-temp siblings",
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

func TestBuildPromptIncludesStrandedWorkspaceRecovery(t *testing.T) {
	t.Parallel()

	prompt, err := BuildPrompt(config.Workflow{Config: config.Config{
		Deliverable: config.Deliverable{Kind: config.DeliverablePullRequest},
	}}, connector.Issue{Identifier: "digitaldrywood/detent#1290"}, PromptOptions{
		WorkspacePath: "/tmp/detent-1290",
		Branch:        "detent/issue-1290",
		RecoveryState: &workspace.RecoveryState{
			UnpushedCommits: 1,
			DiffStat:        workspace.DiffStat{Files: 4, Added: 12, Removed: 3},
			TrackedPaths:    []string{"internal/worker.go"},
			UntrackedPaths:  []string{"worker.log"},
			UnpushedCommitRefs: []string{
				"abc123 fix: preserve work",
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}
	for _, want := range []string{
		"## Existing workspace recovery",
		"unpushed commits: 1",
		"diffstat: 4 files, +12/-3",
		"tracked paths: `internal/worker.go`",
		"untracked paths: `worker.log`",
		"`abc123 fix: preserve work`",
		"completion cannot be accepted",
		"commit and publish",
		"discard stray artifacts",
		"completion_cleanliness_resolution: committed",
		"completion_cleanliness_resolution: discarded",
		"under `fields:`",
		"`status: blocked`",
		"Do not repeat `status: complete`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
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

func TestBuildAdmissionPromptUsesCurrentDependencyEvidence(t *testing.T) {
	t.Parallel()
	for _, ready := range []bool{false, true} {
		t.Run(strconv.FormatBool(ready), func(t *testing.T) {
			observedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			request := AdmissionRequest{Candidates: []AdmissionCandidate{{
				ID: "dependent", Description: "Blocked by: #10\nA human must provide credentials.",
				Dependencies: &AdmissionDependencies{ObservedAt: observedAt, Readiness: "terminal_or_merged", Ready: ready, References: []AdmissionDependency{{Identifier: "owner/repo#10", Closed: ready, Ready: ready}}},
			}}}
			prompt, err := BuildAdmissionPrompt(connector.Issue{Identifier: "project"}, request, PromptOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`"observed_at":"2026-09-04T12:00:00Z"`, `"ready":` + strconv.FormatBool(ready),
				"A Depends on or Blocked by declaration alone is not an open blocker.",
				"A ready dependency satisfies the configured dependency rule even when its declaration remains in the body.",
				"Preserve independent human prerequisites", "A human must provide credentials.",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
		})
	}
}

func TestBuildPromptCapsFailedRunNotes(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1, 40, 41, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			path := filepath.Join(workspace, ".detent", "notes.md")
			var lines []string
			for i := range count {
				lines = append(lines, "output-line-"+strconv.Itoa(i))
			}
			entries := []notes.Entry{
				{Title: "Implementation handoff", Body: "preserve earlier context"},
				{Title: "Failed run output tail", Body: "obsolete failure"},
				{Title: "Implementation handoff", Body: "preserve intervening context"},
				{Title: "Failed run output tail", Body: failedRunNoteBody(RunResult{Output: strings.Join(lines, "\n")}, nil)},
				{Title: "Implementation handoff", Body: "preserve later context"},
			}
			for i, entry := range entries {
				if err := notes.Append(path, entry, notes.AppendOptions{Now: time.Date(2026, 9, 14, 0, i, 0, 0, time.UTC)}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := BuildPrompt(config.Workflow{Prompt: "Base prompt"}, connector.Issue{}, PromptOptions{WorkspacePath: workspace})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(prompt, " - Failed run output tail") != 1 || strings.Contains(prompt, "obsolete failure") {
				t.Fatal("prompt retained obsolete failure")
			}
			for _, text := range []string{"preserve earlier context", "preserve intervening context", "preserve later context", "- final_state: failed"} {
				if !strings.Contains(prompt, text) {
					t.Errorf("prompt missing %q", text)
				}
			}
			want := lines[max(0, len(lines)-40):]
			if strings.Count(prompt, "output-line-") != len(want) {
				t.Errorf("output line count = %d, want %d", strings.Count(prompt, "output-line-"), len(want))
			}
			if len(want) > 0 && !strings.Contains(prompt, "```text\n"+strings.Join(want, "\n")+"\n```") {
				t.Error("prompt missing fenced latest output tail")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("prompt rendering modified persisted notes")
			}
		})
	}
}

func TestBuildPromptIgnoresFencedNoteHeadings(t *testing.T) {
	t.Parallel()
	for _, title := range []string{"Implementation handoff", "Failed run output tail"} {
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			path := filepath.Join(workspace, ".detent", "notes.md")
			embedded := "## 2099-01-01T00:00:00Z - " + title
			oldOutput := "old-start\n" + embedded + "\n" + strings.Repeat("obsolete-output\n", 100)
			latestOutput := "latest-start\n" + embedded + "\n" + strings.Repeat("latest-output\n", 60)
			for i, output := range []string{oldOutput, latestOutput} {
				err := notes.Append(path, notes.Entry{Title: "Failed run output tail", Body: failedRunNoteBody(RunResult{Output: output}, nil)}, notes.AppendOptions{Now: time.Date(2026, 9, 14, 0, i, 0, 0, time.UTC)})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := notes.Append(path, notes.Entry{Title: "Implementation handoff", Body: "keep neighboring note"}, notes.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
			prompt, err := BuildPrompt(config.Workflow{Prompt: "Base prompt"}, connector.Issue{}, PromptOptions{WorkspacePath: workspace})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prompt, "obsolete-output") || strings.Contains(prompt, "latest-start") || strings.Contains(prompt, embedded) {
				t.Fatal("prompt retained output preceding the latest 40 lines")
			}
			if got := strings.Count(prompt, "latest-output"); got != 40 {
				t.Fatalf("latest output lines = %d, want 40", got)
			}
			if !strings.Contains(prompt, "keep neighboring note") {
				t.Fatal("lost neighboring note")
			}
		})
	}
}
