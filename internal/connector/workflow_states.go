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

// ChangeReviewReader is a connector whose tracker keeps Change Requests. A
// run's completion asks it whether the version the run published is still
// the change's current version and is reviewed when the completion is
// applied, since an approval or a check can arrive between the publish and
// the completion moving the item.
type ChangeReviewReader interface {
	ChangeReviewed(ctx context.Context, issueID, changeID, versionID string) (bool, error)
}

// CompletionLane is the lane a successful run moves an item to, chosen only
// from the moves the workflow allows out of its current lane and never an
// operator-only lane. The workflow states no review kind, so the review lane
// is the one the project configures as its review state, as it is for every
// other tracker.
//
// A run that changed something goes to that review lane, never to a lane
// that ends the work; a run that changed nothing goes to the first lane the
// workflow marks terminal. It reports false when the workflow allows no such
// move, and the item is not moved.
func CompletionLane(states []WorkflowState, current, review string, changed bool) (string, bool) {
	return completionLane(states, current, func(target WorkflowState) bool {
		if changed {
			return !target.Terminal && normalizeWorkflowState(target.Name) == normalizeWorkflowState(review)
		}
		return target.Terminal
	})
}

func LandingRefusalLane(states []WorkflowState, current, preferred string, rework bool) (string, bool) {
	eligible := func(target WorkflowState) bool {
		return !target.Terminal && target.Dispatchable == rework
	}
	if lane, ok := completionLane(states, current, func(target WorkflowState) bool {
		return eligible(target) && normalizeWorkflowState(target.Name) == normalizeWorkflowState(preferred)
	}); ok {
		return lane, true
	}
	return completionLane(states, current, eligible)
}

func completionLane(states []WorkflowState, current string, eligible func(WorkflowState) bool) (string, bool) {
	byName := make(map[string]WorkflowState, len(states))
	for _, state := range states {
		byName[normalizeWorkflowState(state.Name)] = state
	}
	from, ok := byName[normalizeWorkflowState(current)]
	if !ok {
		return "", false
	}
	for _, name := range from.Transitions {
		target, ok := byName[normalizeWorkflowState(name)]
		if !ok || target.OperatorOnly || normalizeWorkflowState(target.Name) == normalizeWorkflowState(from.Name) {
			continue
		}
		if eligible(target) {
			return target.Name, true
		}
	}
	return "", false
}

func normalizeWorkflowState(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
