package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/config"
	"github.com/digitaldrywood/symphony/internal/connector"
	"github.com/digitaldrywood/symphony/internal/lessons"
	"github.com/digitaldrywood/symphony/internal/skills"
)

func TestBuildPromptRendersAssignsLessonsAndSkills(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	lessonsPath := filepath.Join(workspace, ".symphony", "lessons.md")
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
					Path:    ".symphony/lessons.md",
					RecallN: 1,
				},
			},
		},
		Prompt: "Prompt for {{ issue.identifier }} via {{ tracker.kind }} attempt={{ attempt }} auto={{ workspace.auto_branch }}",
	}, connector.Issue{
		ID:          "issue-21",
		Identifier:  "digitaldrywood/symphony#21",
		Title:       "Build prompt",
		Description: "Wire prompt builder",
		Labels:      []string{"enhancement", "stage:s3"},
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
		"Prompt for digitaldrywood/symphony#21 via memory attempt=2 auto=true",
		"## Lessons from prior runs",
		"Check generator aliases before editing.",
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
	if prompt != "No body" {
		t.Fatalf("prompt = %q, want No body", prompt)
	}
	if strings.Contains(prompt, "{% endif %}") {
		t.Fatalf("prompt left template delimiter: %q", prompt)
	}
}

func TestBuildPromptIgnoresUnreadableLessons(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".symphony", "lessons.md"), 0o755); err != nil {
		t.Fatalf("mkdir lessons path: %v", err)
	}

	prompt, err := BuildPrompt(config.Workflow{
		Config: config.Config{
			Agent: config.Agent{
				Lessons: config.Lessons{
					Enabled: true,
					Path:    ".symphony/lessons.md",
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
