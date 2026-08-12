package config

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseWorkflowRoutines(t *testing.T) {
	t.Parallel()
	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
routines:
  - name: Dependency-Audit
    schedule: "0 3 * * 1"
    target_state: Backlog
    labels: [Feature, Requires-Human-Review, feature]
    max_findings_per_run: 2
    max_open_findings: 8
    prompt: |
      Follow the dependency criteria in WORKFLOW.md.
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if len(workflow.Config.Routines) != 1 {
		t.Fatalf("Routines = %#v", workflow.Config.Routines)
	}
	routine := workflow.Config.Routines[0]
	if routine.Name != "dependency-audit" || routine.Schedule != "0 3 * * 1" ||
		routine.Prompt != "Follow the dependency criteria in WORKFLOW.md." ||
		routine.TargetState != "Backlog" ||
		!reflect.DeepEqual(routine.Labels, []string{"feature", "requires-human-review"}) ||
		routine.MaxFindingsPerRun != 2 || routine.MaxOpenFindings != 8 {
		t.Fatalf("Routine = %#v", routine)
	}
}

func TestParseWorkflowRoutineLimitDefaults(t *testing.T) {
	t.Parallel()
	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
routines:
  - name: audit
    schedule: "0 * * * *"
    prompt: Inspect.
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	routine := workflow.Config.Routines[0]
	if routine.TargetState != DefaultRoutineTargetState || len(routine.Labels) != 0 {
		t.Fatalf("Routine target = %q, labels = %#v", routine.TargetState, routine.Labels)
	}
	if routine.MaxFindingsPerRun != DefaultRoutineMaxFindingsPerRun || routine.MaxOpenFindings != DefaultRoutineMaxOpenFindings {
		t.Fatalf("Routine limits = %d, %d", routine.MaxFindingsPerRun, routine.MaxOpenFindings)
	}
}

func TestValidateRoutinesRejectsNonPositiveLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		field string
		value int
	}{
		{name: "zero findings per run", field: "max_findings_per_run", value: 0},
		{name: "negative findings per run", field: "max_findings_per_run", value: -1},
		{name: "zero open findings", field: "max_open_findings", value: 0},
		{name: "negative open findings", field: "max_open_findings", value: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte("---\ntracker:\n  kind: memory\nroutines:\n  - name: audit\n    schedule: \"0 * * * *\"\n    prompt: Inspect.\n    " + tt.field + ": " + strconv.Itoa(tt.value) + "\n---\nPrompt\n")
			workflow, err := ParseWorkflow(raw)
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			err = workflow.Config.Validate()
			want := "routines[0]." + tt.field + " must be greater than 0"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Validate() error = %v, want %q", err, want)
			}
		})
	}
}

func TestValidateRoutinesRejectsMalformedBlocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		routines   []Routine
		wantDetail string
	}{
		{name: "missing name", routines: []Routine{{Schedule: "0 * * * *", Prompt: "Inspect."}}, wantDetail: "routines[0].name"},
		{name: "unsafe name", routines: []Routine{{Name: "Dependency Audit", Schedule: "0 * * * *", Prompt: "Inspect."}}, wantDetail: "routines[0].name"},
		{name: "duplicate name", routines: []Routine{{Name: "audit", Schedule: "0 * * * *", Prompt: "Inspect."}, {Name: "AUDIT", Schedule: "0 1 * * *", Prompt: "Inspect again."}}, wantDetail: "routines[1].name must be unique"},
		{name: "missing schedule", routines: []Routine{{Name: "audit", Prompt: "Inspect."}}, wantDetail: "routines[0].schedule is required"},
		{name: "six field schedule", routines: []Routine{{Name: "audit", Schedule: "0 0 3 * * 1", Prompt: "Inspect."}}, wantDetail: "five-field cron"},
		{name: "missing prompt", routines: []Routine{{Name: "audit", Schedule: "0 * * * *"}}, wantDetail: "routines[0].prompt is required"},
		{name: "unknown target state", routines: []Routine{{Name: "audit", Schedule: "0 * * * *", Prompt: "Inspect.", TargetState: "Icebox"}}, wantDetail: "routines[0].target_state must name a configured workflow state"},
		{name: "blank label", routines: []Routine{{Name: "audit", Schedule: "0 * * * *", Prompt: "Inspect.", Labels: []string{" "}}}, wantDetail: "routines[0].labels labels must not be blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Tracker.Kind = TrackerMemory
			cfg.Routines = tt.routines
			cfg.normalize()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantDetail)
			}
		})
	}
}

func TestValidateRoutinesRequiresSupportedTracker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		kind       string
		repository string
		wantDetail string
	}{
		{name: "unsupported tracker", kind: TrackerLocalSQLite, wantDetail: "routines requires tracker.kind github or memory"},
		{name: "github repository", kind: TrackerGitHub, wantDetail: "tracker.repository is required for routines"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Tracker.Kind = tt.kind
			cfg.Tracker.Repository = tt.repository
			cfg.Routines = []Routine{{Name: "audit", Schedule: "0 * * * *", Prompt: "Inspect."}}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.wantDetail)
			}
		})
	}
}
