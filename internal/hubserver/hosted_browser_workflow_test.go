package hubserver

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
)

func resolvedWorkflowPolicy(t *testing.T, states []policy.State, revision string, promotion bool) policy.Descriptor {
	t.Helper()
	if !slices.ContainsFunc(states, func(state policy.State) bool { return state.Name == "Blocked" }) {
		states = append(slices.Clone(states), policy.State{Name: "Blocked"})
	}
	lanes := []workflowconfig.Lane{}
	transitions := map[string][]string{}
	for _, state := range states {
		role := workflowconfig.LaneHolding
		if state.Terminal {
			role = workflowconfig.LaneTerminal
		} else if state.Dispatchable {
			role = workflowconfig.LaneActive
		}
		lanes = append(lanes, workflowconfig.Lane{Name: state.Name, Role: role})
		transitions[state.Name] = state.Transitions
	}
	raw, err := yaml.Marshal(map[string]any{
		"schema":  1,
		"tracker": map[string]any{"kind": "hub_native", "repository": "example-org/example-project", "lanes": lanes},
		"gate":    map[string]any{"run": "true"},
		"plan":    map[string]any{"enabled": false},
		"agent":   map[string]any{"auto_promote": map[string]any{"enabled": promotion}},
		"server":  map[string]any{"kanban": map[string]any{"allowed_transitions": transitions}},
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
		Workflow: []byte("Implement the assigned issue."), Config: raw, HasConfig: true, ConfigPath: "detent.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow.Definition.Revision = revision
	descriptor, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err = workflowconfig.ResolveSharedPolicy(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}
