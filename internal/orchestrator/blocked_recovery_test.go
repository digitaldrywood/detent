package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestRecoverBlockedIssuesSkipsPersistedReworkLimitBlockedIssue(t *testing.T) {
	t.Parallel()

	issue := dependencyAutoUnblockIssue("issue-recovery-rework-limit", "Blocked")
	prNumber := 418
	issue.PRNumber = &prNumber
	issue.PullRequest = &connector.PullRequest{
		Number:         prNumber,
		State:          "OPEN",
		URL:            "https://github.test/digitaldrywood/detent/pull/418",
		MergeableState: "behind",
		HeadSHA:        "abc123",
	}
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{issue},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    "Blocked",
		Reason:       "rework_limit",
		Status:       "entered",
		StartedAt:    time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC),
		MetadataJSON: "{}",
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.recoverBlockedIssues(context.Background(), &state, []connector.Issue{issue}, time.Date(2026, 7, 8, 15, 1, 0, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no blocked recovery transition", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want no blocked recovery comment", tracker.comments)
	}
}
