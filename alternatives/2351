package orchestrator

import (
	"context"
	"testing"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

// Regression test: a successful plan completion must clear both failure
// circuit breakers. The reset used to live only in the implement completion
// path, after the plan branch had already returned, so a plan that failed
// repeatedly and then succeeded carried a stale count that could park the
// issue prematurely on a later unrelated failure.
func TestHandleRunResultPlanCompletionResetsFailureBreakers(t *testing.T) {
	t.Parallel()

	tracker := &dependencyAutoUnblockConnector{}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch := planReviewTestOrchestrator(tracker, metrics)
	state := newState(orch.cfg)

	issue := dependencyAutoUnblockIssue("issue-plan-reset", "Todo")
	now := time.Date(2026, 7, 9, 3, 0, 0, 0, time.UTC)
	state.Running[issue.ID] = Running{
		Issue:     issue,
		Attempt:   repeatedFailureThreshold,
		Mode:      runpkg.RunModePlan,
		StartedAt: now.Add(-3 * time.Minute),
	}
	state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: repeatedFailureThreshold - 1}
	state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: instantFailureThreshold - 1}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModePlan},
		Result:      runpkg.RunResult{Output: "plan artifact"},
		CompletedAt: now,
	})

	if _, ok := state.RepeatedFailures[issue.ID]; ok {
		t.Fatalf("RepeatedFailures[%q] present after successful plan completion", issue.ID)
	}
	if _, ok := state.InstantFailures[issue.ID]; ok {
		t.Fatalf("InstantFailures[%q] present after successful plan completion", issue.ID)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one plan artifact comment", tracker.comments)
	}
}
