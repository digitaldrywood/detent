package global

import (
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/intake"
)

func TestParseLoadsProjectIntakeOverride(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(fmt.Sprintf(`
apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 2
  scheduling: weighted
projects:
  - id: example
    workflow: WORKFLOW.md
    workflow_ref: origin/main
    workdir: %s
    weight: 1
    priority: 0
    intake:
      sources:
        - name: weekly-todos
          kind: schedule
          cron: "0 6 * * 1"
          scan: stale-todos
          creates:
            status: Backlog
            labels: [maintenance]
            title: "{summary}"
          dedupe_by: fingerprint
`, t.TempDir())), "/config/global.yaml", WithProjectPathLiterals())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	project := cfg.Projects[0]
	if !project.IntakeConfigured || len(project.Intake.Sources) != 1 {
		t.Fatalf("project intake = configured %t sources %#v", project.IntakeConfigured, project.Intake.Sources)
	}
	if project.Intake.Sources[0].Kind != intake.KindSchedule {
		t.Fatalf("source = %#v", project.Intake.Sources[0])
	}
}

func TestParseProjectIntakeEmptyListCanDisableWorkflowSources(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(fmt.Sprintf(`
apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 2
  scheduling: weighted
projects:
  - id: example
    workflow: WORKFLOW.md
    workflow_ref: origin/main
    workdir: %s
    weight: 1
    priority: 0
    intake:
      sources: []
`, t.TempDir())), "/config/global.yaml", WithProjectPathLiterals())
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Projects[0].IntakeConfigured || cfg.Projects[0].Intake.Enabled() {
		t.Fatalf("project intake = %#v, want configured disabled override", cfg.Projects[0])
	}
}
