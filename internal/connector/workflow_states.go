package connector

import (
	"context"
	"strings"
)

// WorkflowState is one lane of a tracker that states its own workflow, as a
// hub-native project does: which lanes end the work, which ones dispatch it,
// and where each lane may move next.
type WorkflowState struct {
	Name         string
	Terminal     bool
	Dispatchable bool
	OperatorOnly bool
	Transitions  []string
}

// WorkflowStateReader is a connector whose tracker states its workflow.
type WorkflowStateReader interface {
	WorkflowStates(context.Context) ([]WorkflowState, error)
}

// CompletionLane is the lane a successful run moves an item to, chosen from
// the moves the workflow allows out of its current lane, in the order the
// workflow lists them. Operator-only lanes are never chosen.
//
// A run that changed something goes to the first lane that neither ends nor
// dispatches the work, which is where a change waits for review; a workflow
// without one ends the work instead. A run that changed nothing goes to the
// first lane that ends the work, and a workflow without one parks it in the
// first lane that does not dispatch it. It reports false when the workflow
// offers neither, and the item stays where it is.
func CompletionLane(states []WorkflowState, current string, changed bool) (string, bool) {
	byName := make(map[string]WorkflowState, len(states))
	for _, state := range states {
		byName[normalizeWorkflowState(state.Name)] = state
	}
	from, ok := byName[normalizeWorkflowState(current)]
	if !ok {
		return "", false
	}
	review, done := "", ""
	for _, name := range from.Transitions {
		target, ok := byName[normalizeWorkflowState(name)]
		if !ok || target.OperatorOnly || normalizeWorkflowState(target.Name) == normalizeWorkflowState(from.Name) {
			continue
		}
		switch {
		case target.Terminal && done == "":
			done = target.Name
		case !target.Terminal && !target.Dispatchable && review == "":
			review = target.Name
		}
	}
	first, second := review, done
	if !changed {
		first, second = done, review
	}
	if first != "" {
		return first, true
	}
	return second, second != ""
}

func normalizeWorkflowState(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
