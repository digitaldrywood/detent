package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
)

func TestRepositoryNativeWorkflowProjection(t *testing.T) {
	t.Parallel()
	definition := "tracker:\n  kind: memory\n  active_states: [Todo, In Progress, Rework, Merging]\n  observed_states: [Backlog, Blocked, Human Review]\n  terminal_states: [Done, Cancelled]\nplan:\n  enabled: true\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Cancelled]\n"
	var resolved []policy.State
	for _, test := range []struct {
		name, workflow, config, source string
	}{
		{"legacy", "---\n" + definition + "---\nImplement the issue.\n", "", "WORKFLOW.md"},
		{"split", "Implement the issue.\n", "schema: 1\n" + definition, "detent.yaml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := ParseProjectDefinition(ProjectDefinitionSources{WorkflowPath: "WORKFLOW.md", Workflow: []byte(test.workflow), ConfigPath: "detent.yaml", Config: []byte(test.config), HasConfig: test.config != ""})
			if err != nil {
				t.Fatal(err)
			}
			workflow.Config = workflow.Config.ForNativeTracker()
			descriptor, err := ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.Workflow == nil || descriptor.Workflow.Source != test.source || len(descriptor.Workflow.States) != 10 {
				t.Fatalf("resolved workflow = %#v", descriptor.Workflow)
			}
			want := map[string][2]bool{"Backlog": {}, "Todo": {false, true}, "Plan Review": {}, "In Progress": {false, true}, "Blocked": {}, "Human Review": {}, "Rework": {false, true}, "Merging": {false, true}, "Done": {true, false}, "Cancelled": {true, false}}
			for _, state := range descriptor.Workflow.States {
				flags, exists := want[state.Name]
				if !exists || state.Terminal != flags[0] || state.Dispatchable != flags[1] || !reflect.DeepEqual(state.Transitions, workflow.Config.KanbanAllowedTransitionTargets(state.Name)) {
					t.Fatalf("state = %#v, expected flags %v", state, flags)
				}
				delete(want, state.Name)
			}
			if len(want) != 0 {
				t.Fatalf("missing states = %v", want)
			}
			if resolved != nil && !reflect.DeepEqual(resolved, descriptor.Workflow.States) {
				t.Fatal("split and legacy contracts resolved different states")
			}
			resolved = descriptor.Workflow.States
			if !workflow.Config.KanbanTransitionAllowed("In Progress", "Merging") || !workflow.Config.KanbanTransitionAllowed("Merging", "Done") {
				t.Fatal("resolved native workflow cannot advance runner completion and landing")
			}
		})
	}
	t.Run("custom state and transition", func(t *testing.T) {
		workflow, err := ParseProjectDefinition(ProjectDefinitionSources{WorkflowPath: "WORKFLOW.md", Workflow: []byte("---\n" + strings.ReplaceAll(definition, "Rework", "Repair") + "---\nWork.\n")})
		if err != nil {
			t.Fatal(err)
		}
		workflow.Config = workflow.Config.ForNativeTracker()
		descriptor, err := ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range descriptor.Workflow.States {
			if state.Name == "Repair" && state.Dispatchable {
				return
			}
		}
		t.Fatal("custom dispatchable state missing")
	})
	for _, test := range []struct{ name, extra string }{
		{"unknown source", "server:\n  kanban:\n    allowed_transitions:\n      Missing: [Todo]\n"},
		{"unknown target", "server:\n  kanban:\n    allowed_transitions:\n      Todo: [Missing]\n"},
		{"terminal dispatch", "tracker:\n  kind: hub_native\n  active_states: [Todo, Done]\n  terminal_states: [Done]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := "tracker:\n  kind: hub_native\n" + test.extra
			if strings.HasPrefix(test.extra, "tracker:") {
				definition = test.extra
			}
			workflow, err := ParseProjectDefinition(ProjectDefinitionSources{WorkflowPath: "WORKFLOW.md", Workflow: []byte("---\n" + definition + "---\nWork\n")})
			if err == nil {
				_, err = ResolvePolicy(workflow)
			}
			if err == nil {
				t.Fatal("invalid supplied workflow was accepted")
			}
		})
	}
}
