package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/markdownfence"
)

type nativePlanWorkflow struct {
	states []connector.WorkflowState
	stop   string
}

// Resolve the existing native workflow before starting a planner. Projects
// without a dedicated plan lane use their configured review destination; an
// approved automated result goes directly to implementation under the same lease.
func (o *Orchestrator) nativePlanLanes(ctx context.Context, issue connector.Issue) (nativePlanWorkflow, error) {
	states, err := o.connector.(connector.WorkflowStateReader).WorkflowStates(ctx) //nolint:errcheck // nativeWorkflow verifies this capability before native planning.
	if err != nil {
		return nativePlanWorkflow{}, err
	}
	cfg := gate.EffectivePlan(o.cfg.Plan)
	stop := cfg.Stop
	if !nativePlanStateExists(states, stop) {
		stop = normalizeAutoPromoteConfig(o.cfg.AutoPromote).reviewTargetState()
	}
	w := nativePlanWorkflow{states: states, stop: stop}
	if _, ok := w.path(issue.State, planImplementationState); !ok || !dispatchableState(states, planImplementationState) {
		return w, fmt.Errorf("native plan workflow has no implementation handoff from %s to %s", issue.State, planImplementationState)
	}
	if _, ok := w.path(issue.State, stop); !ok || dispatchableState(states, stop) {
		return w, fmt.Errorf("native plan workflow has no review handoff from %s to %s (configured plan stop %s)", issue.State, stop, cfg.Stop)
	}
	return w, nil
}

func nativePlanStateExists(states []connector.WorkflowState, name string) bool {
	for _, state := range states {
		if normalizeState(state.Name) == normalizeState(name) && !state.Terminal && !state.OperatorOnly {
			return true
		}
	}
	return false
}

func (w nativePlanWorkflow) path(from, to string) ([]string, bool) {
	if normalizeState(from) == normalizeState(to) {
		return nil, true
	}
	if target, ok := connector.CompletionLane(w.states, from, to, true); ok {
		return []string{target}, true
	}
	// Hosted Todo moves through In Progress to the existing review lane.
	if first, ok := connector.CompletionLane(w.states, from, planImplementationState, true); ok {
		if last, ok := connector.CompletionLane(w.states, first, to, true); ok {
			return []string{first, last}, true
		}
	}
	return nil, false
}

func nativePlanReviewOutput(output string) string {
	lines := strings.Split(output, "\n")
	var fence markdownfence.Fence
	for i, line := range lines {
		if fence.Consume(line) || fence != "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line), "## Detent Plan Review") {
			end := i + 1
			for end < len(lines) {
				if !fence.Consume(lines[end]) && fence == "" && strings.HasPrefix(strings.TrimSpace(lines[end]), "## ") {
					break
				}
				end++
			}
			return strings.TrimSpace(strings.Join(lines[i:end], "\n"))
		}
	}
	return ""
}

func (o *Orchestrator) nativePlanTarget(ctx context.Context, issue connector.Issue, output string) ([]string, error) {
	w, err := o.nativePlanLanes(ctx, issue)
	if err != nil {
		return nil, err
	}
	review := nativePlanReviewOutput(output)
	evaluation := planReviewEvaluationFromComments([]connector.IssueComment{{Body: review}})
	decision := gate.EvaluatePlan(o.cfg.Plan, issue.Labels, evaluation.Summary)
	target := planReviewTargetState(decision.Action)
	if target == "" || !nativePlanStateExists(w.states, target) {
		target = w.stop
	}
	path, ok := w.path(issue.State, target)
	if !ok {
		return nil, fmt.Errorf("native plan workflow has no move from %s to %s", issue.State, target)
	}
	return path, nil
}
