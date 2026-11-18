package orchestrator

import (
	"context"
	"testing"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestHandleRunResultPlanCompletionResetsRetry(t *testing.T) {
	t.Parallel()

	tracker := &dependencyAutoUnblockConnector{}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch := planReviewTestOrchestrator(tracker, metrics)
	state := newState(orch.cfg)

	issue := dependencyAutoUnblockIssue("issue-plan-reset", "Todo")
	now := time.Date(2026, 7, 9, 3, 0, 0, 0, time.UTC)
	state.Running[issue.ID] = Running{
		Issue:     issue,
		Attempt:   5,
		Mode:      runpkg.RunModePlan,
		StartedAt: now.Add(-3 * time.Minute),
	}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModePlan},
		Result:      runpkg.RunResult{Output: "plan artifact"},
		CompletedAt: now,
	})

	if retry, ok := state.Retry[issue.ID]; ok && retry.Attempt > 1 {
		t.Fatalf("plan success retained failure backoff: %+v", retry)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one plan artifact comment", tracker.comments)
	}
}
