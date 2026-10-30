package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestValidationContext(t *testing.T) {
	const evidence = "https://github.com/o/r/pull/236#issuecomment-5852279403"
	original := connector.Issue{Title: "Qualify cleanup", Description: "Require an integrated exercise", Comments: []connector.IssueComment{{Body: "## Codex Workpad\nQualification evidence: " + evidence}}}
	for _, tt := range []struct {
		name      string
		change    func(*connector.Issue, *gate.Config)
		workflow  string
		different bool
	}{
		{name: "stable"},
		{name: "body relaxed", different: true, change: func(i *connector.Issue, _ *gate.Config) { i.Description = "Unproven cause is acceptable" }},
		{name: "body tightened", different: true, change: func(i *connector.Issue, _ *gate.Config) { i.Description += " on physical hardware" }},
		{name: "new qualification", different: true, change: func(i *connector.Issue, _ *gate.Config) {
			i.Comments = []connector.IssueComment{{Body: "## Codex Workpad\nQualification evidence: " + evidence + "-new"}}
		}},
		{name: "progress comment", change: func(i *connector.Issue, _ *gate.Config) {
			i.Comments = append(i.Comments, connector.IssueComment{Body: "Still waiting at " + evidence + "-chatter"})
		}},
		{name: "workpad progress", change: func(i *connector.Issue, _ *gate.Config) {
			i.Comments = []connector.IssueComment{{Body: original.Comments[0].Body + "\nTests pending. Session 42"}}
		}},
		{name: "timestamp", change: func(i *connector.Issue, _ *gate.Config) { i.UpdatedAt = new(time.Now()) }},
		{name: "line endings", change: func(i *connector.Issue, _ *gate.Config) { i.Description += "  \r\n" }},
		{name: "review score", different: true, change: func(_ *connector.Issue, c *gate.Config) { c.Validator.MinScore = .99 }},
		{name: "review severities", different: true, change: func(_ *connector.Issue, c *gate.Config) { c.Validator.BlockOn = []string{"p1", "p2"} }},
		{name: "criterion policy", different: true, change: func(_ *connector.Issue, c *gate.Config) {
			c.Validator.UnverifiedCriteria = gate.UnverifiedCriteriaDisclose
		}},
		{name: "workflow text", different: true, workflow: "Review rules: every change needs a regression test."},
		{name: "workflow whitespace", workflow: "  \r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue, policy := original, gate.Config{}
			if tt.change != nil {
				tt.change(&issue, &policy)
			}
			different := ValidationContextDigest(issue, policy, tt.workflow) != ValidationContextDigest(original, gate.Config{}, "")
			if different != tt.different {
				t.Fatalf("digest changed=%v, want %v", different, tt.different)
			}
		})
	}
	prompt := BuildValidatorPrompt(config.Workflow{}, original, ValidatorPromptOptions{})
	if !strings.Contains(prompt, evidence) {
		t.Fatal("qualification identity absent from actual prompt")
	}
	if strings.Contains(prompt, "Project WORKFLOW.md") {
		t.Fatal("empty workflow rendered a workflow section")
	}
	const rules = "## Review rules\n\nEvery behavior change needs a regression test."
	workflow := config.Workflow{SharedPrompt: rules, Prompt: "local overlay only"}
	for _, review := range []bool{false, true} {
		prompt := BuildValidatorPrompt(workflow, original, ValidatorPromptOptions{implementationReview: review})
		if !strings.Contains(prompt, "Project WORKFLOW.md (governs this review") || !strings.Contains(prompt, rules) || strings.Contains(prompt, "local overlay only") {
			t.Fatalf("implementationReview=%t prompt does not carry the shared workflow:\n%s", review, prompt)
		}
		if !strings.Contains(prompt, "Task context SHA-256: "+ValidationContextDigest(original, gate.Config{}, rules)) {
			t.Fatalf("implementationReview=%t prompt digest omits the workflow", review)
		}
	}
}
