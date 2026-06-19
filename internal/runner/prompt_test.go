package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/lessons"
	"github.com/digitaldrywood/detent/internal/skills"
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
		"## Validation gate",
		"Run `make check` from the workspace root",
		"In Merging, run a focused rebase/smoke gate after a clean rebase when the PR already passed current-head validation",
		"## Available skills",
		"- migrate — Issue mentions schema changes.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Add migrations.") {
		t.Fatalf("prompt included skill description, want only when_to_use:\n%s", prompt)
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
		},
		Prompt: "Gate {{ gate.kind }} label={{ gate.approval_label }} run={{ gate.run }} ci={{ gate.ci_failure_action }}",
	}, connector.Issue{
		Identifier: "digitaldrywood/detent#266",
		Title:      "Gate prompt",
	}, PromptOptions{})
	if err != nil {
		t.Fatalf("BuildPrompt() error = %v", err)
	}

	for _, want := range []string{
		"Gate human_review label=approved-by-human run= ci=skip",
		"## Validation gate",
		"Keep the pull request in Human Review until a human applies label `approved-by-human`",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
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
