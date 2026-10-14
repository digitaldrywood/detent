package config

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/intake"
)

func TestParseWorkflowLoadsIntakeSources(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: github
  api_key: token
  github_status_source: label
  repository: example/repo
intake:
  sources:
    - name: production-errors
      kind: sentry
      secret: $SENTRY_WEBHOOK_SECRET
      match: level:error
      creates:
        status: Backlog
        labels: [bug]
        title: "[{source}] {summary}"
      dedupe_by: fingerprint
---
Work the issue.
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(workflow.Config.Intake.Sources) != 1 {
		t.Fatalf("Intake.Sources = %#v", workflow.Config.Intake.Sources)
	}
	source := workflow.Config.Intake.Sources[0]
	if source.Name != "production-errors" || source.Kind != intake.KindSentry || source.Creates.Status != "Backlog" {
		t.Fatalf("source = %#v", source)
	}
}

func TestValidateIntakeRequiresGitHubRepositoryTracker(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tracker.Kind = TrackerMemory
	cfg.Intake = intake.Config{Sources: []intake.Source{{
		Kind:   intake.KindWebhook,
		Secret: "secret",
	}}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "intake.sources requires tracker.kind github") {
		t.Fatalf("Validate() error = %v, want GitHub tracker requirement", err)
	}
}

func TestDefaultWorkflowHasNoIntakeBehavior(t *testing.T) {
	t.Parallel()

	if Default().Intake.Enabled() {
		t.Fatal("Default().Intake.Enabled() = true, want false")
	}
}
