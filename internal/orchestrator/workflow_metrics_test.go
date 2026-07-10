package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestApplyAutoPromoteDecisionUpdatesSnapshotBeforePoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		trackerName string
	}{
		{name: "label", trackerName: "github_label"},
		{name: "ProjectV2", trackerName: "github_project_v2"},
		{name: "issue field", trackerName: "github_issue_field"},
		{name: "local", trackerName: "local"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			transitionAt := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
			previousStageAt := transitionAt.Add(-5 * time.Minute)
			issue := connector.Issue{
				ID:             "issue-promote",
				Identifier:     "digitaldrywood/detent#1131",
				State:          "In Progress",
				StageUpdatedAt: &previousStageAt,
			}
			tracker := &workflowMetricsConnector{name: tt.trackerName}
			orch := &Orchestrator{connector: tracker}
			state := newState(Config{})
			state.BoardIssues = []connector.Issue{cloneIssue(issue)}
			state.Pipeline = []connector.Issue{cloneIssue(issue)}

			applied := orch.applyAutoPromoteDecision(
				context.Background(),
				&state,
				issue,
				AutoPromoteSummary{},
				autoPromoteDecision(AutoPromoteActionPromote, AutoPromoteReasonReady),
				"Merging",
				transitionAt,
			)
			if !applied {
				t.Fatal("applyAutoPromoteDecision() = false, want true")
			}

			snapshot := state.Snapshot(transitionAt.Add(time.Second))
			if len(snapshot.BoardIssues) != 1 {
				t.Fatalf("snapshot BoardIssues len = %d, want 1", len(snapshot.BoardIssues))
			}
			if got := snapshot.BoardIssues[0].State; got != "Merging" {
				t.Fatalf("snapshot BoardIssues state = %q, want Merging", got)
			}
			if snapshot.BoardIssues[0].StageUpdatedAt == nil || !snapshot.BoardIssues[0].StageUpdatedAt.Equal(transitionAt) {
				t.Fatalf("snapshot BoardIssues StageUpdatedAt = %v, want %v", snapshot.BoardIssues[0].StageUpdatedAt, transitionAt)
			}
			if len(snapshot.Pipeline) != 1 {
				t.Fatalf("snapshot Pipeline len = %d, want 1", len(snapshot.Pipeline))
			}
			if got := snapshot.Pipeline[0].State; got != "Merging" {
				t.Fatalf("snapshot Pipeline state = %q, want Merging", got)
			}
			if tracker.fetches != 0 {
				t.Fatalf("tracker fetches = %d, want none", tracker.fetches)
			}
		})
	}
}

func TestUpdateIssueStateByIDSkipsWorkflowMetricsForBlockedUpdate(t *testing.T) {
	t.Parallel()

	recorder := &workflowMetricsRecorderSpy{}
	orch := &Orchestrator{
		connector: &workflowMetricsConnector{
			err: &connector.StateUpdateBlockedError{
				IssueID:      "issue-blocked",
				CurrentState: "Done",
				TargetState:  "Todo",
			},
		},
		workflowMetrics: recorder,
	}
	state := newState(Config{})
	state.BoardIssues = []connector.Issue{{ID: "issue-blocked", State: "Done"}}

	err := orch.updateIssueStateByID(
		context.Background(),
		&state,
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
	if got := state.BoardIssues[0].State; got != "Done" {
		t.Fatalf("snapshot BoardIssues state = %q, want Done", got)
	}
}

type workflowMetricsRecorderSpy struct {
	events []store.WorkflowPhaseEvent
}

func (r *workflowMetricsRecorderSpy) RecordWorkflowPhaseEvent(_ context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}

type workflowMetricsConnector struct {
	name    string
	err     error
	fetches int
}

func (c *workflowMetricsConnector) Name() string {
	return c.name
}

func (c *workflowMetricsConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.fetches++
	return nil, nil
}

func (c *workflowMetricsConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	c.fetches++
	return nil, nil
}

func (c *workflowMetricsConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.fetches++
	return nil, nil
}

func (c *workflowMetricsConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *workflowMetricsConnector) UpdateIssueState(context.Context, string, string) error {
	return c.err
}

func (c *workflowMetricsConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *workflowMetricsConnector) SetField(context.Context, string, string, string) error {
	return nil
}
