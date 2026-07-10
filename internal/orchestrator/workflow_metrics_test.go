package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestTickPreservesOrchestratorTransitionSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	issue := connector.Issue{
		ID:           "issue-closed",
		Identifier:   "digitaldrywood/detent#1131",
		State:        "Human Review",
		Closed:       true,
		ClosedReason: "completed",
	}
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:      []string{"Blocked", "Done"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	tracker := &workflowMetricsConnector{stateIssues: []connector.Issue{cloneIssue(issue)}}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.BoardIssues = []connector.Issue{cloneIssue(issue)}
	state.Pipeline = []connector.Issue{cloneIssue(issue)}

	orch.tick(context.Background(), &state, now)

	snapshot := state.Snapshot(now.Add(time.Second))
	if len(snapshot.BoardIssues) != 1 {
		t.Fatalf("snapshot BoardIssues len = %d, want 1", len(snapshot.BoardIssues))
	}
	if got := snapshot.BoardIssues[0].State; got != "Done" {
		t.Fatalf("snapshot BoardIssues state = %q, want Done", got)
	}
	if len(snapshot.Pipeline) != 1 {
		t.Fatalf("snapshot Pipeline len = %d, want 1", len(snapshot.Pipeline))
	}
	if got := snapshot.Pipeline[0].State; got != "Done" {
		t.Fatalf("snapshot Pipeline state = %q, want Done", got)
	}

	external := cloneIssue(issue)
	external.Closed = false
	external.ClosedReason = ""
	tracker.stateIssues = []connector.Issue{external}
	orch.tick(context.Background(), &state, now.Add(time.Minute))

	snapshot = state.Snapshot(now.Add(time.Minute + time.Second))
	if len(snapshot.BoardIssues) != 1 || snapshot.BoardIssues[0].State != "Human Review" {
		t.Fatalf("snapshot BoardIssues = %#v, want external Human Review state", snapshot.BoardIssues)
	}
	if len(snapshot.Pipeline) != 1 || snapshot.Pipeline[0].State != "Human Review" {
		t.Fatalf("snapshot Pipeline = %#v, want external Human Review state", snapshot.Pipeline)
	}
}

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

func TestResolveCurrentLaneEnteredAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC)
	createdAt := now.Add(-4 * time.Hour)
	enteredAt := now.Add(-time.Hour)
	transitionAt := now.Add(-10 * time.Minute)
	updatedAt := now.Add(-5 * time.Minute)

	tests := []struct {
		name     string
		issue    connector.Issue
		previous time.Time
		events   []store.WorkflowPhaseEvent
		want     time.Time
	}{
		{
			name: "same-lane update keeps previous entry",
			issue: connector.Issue{
				State:     "In Progress",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			previous: enteredAt,
			want:     enteredAt,
		},
		{
			name: "lane change uses transition event",
			issue: connector.Issue{
				State:     "Merging",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "entered", StartedAt: enteredAt},
				{ID: 2, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "merging", Status: "ENTERED", StartedAt: transitionAt},
			},
			want: transitionAt,
		},
		{
			name: "leave and return uses latest durable entry",
			issue: connector.Issue{
				State:     "In Progress",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			previous: enteredAt,
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "entered", StartedAt: enteredAt},
				{ID: 2, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Merging", Status: "entered", StartedAt: transitionAt.Add(-time.Minute)},
				{ID: 3, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "entered", StartedAt: transitionAt},
			},
			want: transitionAt,
		},
		{
			name: "restart restores durable entry",
			issue: connector.Issue{
				State:     "In Progress",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "in progress", Status: "entered", StartedAt: enteredAt},
			},
			want: enteredAt,
		},
		{
			name: "missing phase events uses tracker fallback",
			issue: connector.Issue{
				State:     "Todo",
				CreatedAt: &createdAt,
				UpdatedAt: &enteredAt,
			},
			want: enteredAt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveCurrentLaneEnteredAt(tt.issue, tt.previous, tt.events)
			if !got.Equal(tt.want) {
				t.Fatalf("resolveCurrentLaneEnteredAt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefreshCurrentLaneEntriesSurvivesStoreRestart(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "detent.db")
	enteredAt := time.Date(2026, 7, 9, 17, 0, 0, 0, time.UTC)

	seed, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("store.Open(seed) error = %v", err)
	}
	if _, err := seed.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID: "detent",
		IssueID:   "issue-1130",
		PhaseType: store.WorkflowPhaseTypeLane,
		PhaseName: "In Progress",
		Status:    "entered",
		StartedAt: enteredAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed.Close() error = %v", err)
	}

	for index, updatedAt := range []time.Time{enteredAt.Add(30 * time.Minute), enteredAt.Add(time.Hour)} {
		backend, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: dbPath})
		if err != nil {
			t.Fatalf("store.Open(restart %d) error = %v", index+1, err)
		}

		state := newState(normalizeConfig(Config{}))
		state.BoardIssues = []connector.Issue{{
			ID:        "issue-1130",
			State:     "In Progress",
			UpdatedAt: &updatedAt,
		}}
		orch := &Orchestrator{workflowMetrics: backend}
		orch.refreshCurrentLaneEntries(ctx, &state)
		snapshot := state.Snapshot(updatedAt.Add(time.Minute))
		if snapshot.BoardIssues[0].CurrentLaneEnteredAt == nil || !snapshot.BoardIssues[0].CurrentLaneEnteredAt.Equal(enteredAt) {
			t.Errorf("restart %d CurrentLaneEnteredAt = %v, want %v", index+1, snapshot.BoardIssues[0].CurrentLaneEnteredAt, enteredAt)
		}
		if err := backend.Close(); err != nil {
			t.Fatalf("backend.Close(restart %d) error = %v", index+1, err)
		}
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
	name        string
	err         error
	fetches     int
	candidates  []connector.Issue
	stateIssues []connector.Issue
}

func (c *workflowMetricsConnector) Name() string {
	return c.name
}

func (c *workflowMetricsConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.fetches++
	return cloneIssues(c.candidates), nil
}

func (c *workflowMetricsConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.fetches++
	return issuesInStates(c.stateIssues, states), nil
}

func (c *workflowMetricsConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.fetches++
	return nil, nil
}

func (c *workflowMetricsConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *workflowMetricsConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	if c.err != nil {
		return c.err
	}
	for index := range c.candidates {
		if c.candidates[index].ID == issueID {
			c.candidates[index].State = state
		}
	}
	for index := range c.stateIssues {
		if c.stateIssues[index].ID == issueID {
			c.stateIssues[index].State = state
		}
	}
	return nil
}

func (c *workflowMetricsConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *workflowMetricsConnector) SetField(context.Context, string, string, string) error {
	return nil
}
