package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestUpdateIssueStateByIDSkipsWorkflowMetricsForBlockedUpdate(t *testing.T) {
	t.Parallel()

	recorder := &workflowMetricsRecorderSpy{}
	orch := &Orchestrator{
		connector: workflowMetricsBlockedConnector{
			err: &connector.StateUpdateBlockedError{
				IssueID:      "issue-blocked",
				CurrentState: "Done",
				TargetState:  "Todo",
			},
		},
		workflowMetrics: recorder,
	}

	err := orch.updateIssueStateByID(
		context.Background(),
		"issue-blocked",
		connector.Issue{
			ID:         "issue-blocked",
			Identifier: "digitaldrywood/detent#100",
			State:      "Done",
		},
		"Todo",
		time.Now(),
		"test",
	)
	if err != nil {
		t.Fatalf("updateIssueStateByID() error = %v, want nil", err)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("workflow metric events = %#v, want none", recorder.events)
	}
}

type workflowMetricsRecorderSpy struct {
	events []store.WorkflowPhaseEvent
}

func (r *workflowMetricsRecorderSpy) RecordWorkflowPhaseEvent(_ context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}

type workflowMetricsBlockedConnector struct {
	err error
}

func (c workflowMetricsBlockedConnector) Name() string {
	return "workflow_metrics_blocked"
}

func (c workflowMetricsBlockedConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c workflowMetricsBlockedConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c workflowMetricsBlockedConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c workflowMetricsBlockedConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c workflowMetricsBlockedConnector) UpdateIssueState(context.Context, string, string) error {
	return c.err
}

func (c workflowMetricsBlockedConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c workflowMetricsBlockedConnector) SetField(context.Context, string, string, string) error {
	return nil
}
