package orchestrator_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestTargetedReconcileUpdatesBoardWithoutFullPoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		result      connector.ReconcileResult
		wantCount   int
		wantState   string
		laneFailure bool
	}{
		{
			name: "external label transition",
			result: connector.ReconcileResult{
				Found: true,
				Issue: targetedReconcileIssue("In Progress"),
			},
			wantCount: 1,
			wantState: "In Progress",
		},
		{
			name: "configured label removed",
			result: connector.ReconcileResult{
				Found: true,
				Issue: targetedReconcileIssue("Open"),
			},
			wantCount: 0,
		},
		{name: "canonical lane error preserves prior targeted state", result: connector.ReconcileResult{Found: true, Issue: targetedReconcileIssue("In Progress")}, wantCount: 1, wantState: "Backlog", laneFailure: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			initial := targetedReconcileIssue("Backlog")
			base := newFakeConnector()
			base.stateIssues = []connector.Issue{initial}
			tracker := &targetedConnector{fakeConnector: base, result: tt.result}
			var source connector.Connector = tracker
			if tt.laneFailure {
				source = &targetedLaneConnector{targetedConnector: tracker}
			}
			orch, err := orchestrator.New(orchestrator.Config{
				PollInterval:        time.Hour,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress"},
				ObservedStates:      []string{"Backlog"},
				TerminalStates:      []string{"Done"},
			}, orchestrator.Dependencies{Connector: source})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- orch.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				<-done
			})

			initialState := waitForTargetedState(t, orch, func(state orchestrator.State) bool {
				return state.DataSeq >= 1 && len(state.BoardIssues) == 1
			})
			initialFetches := base.fetchCounts()
			response, err := orch.RequestTargetedRefresh(context.Background(), connector.ReconcileTarget{
				Scope:          "digitaldrywood/detent",
				WorkItemNumber: 1133,
				Event:          "issues",
			})
			if err != nil {
				t.Fatalf("RequestTargetedRefresh() error = %v", err)
			}
			if !response.Queued || response.Coalesced {
				t.Fatalf("RequestTargetedRefresh() = %#v, want queued", response)
			}

			state := waitForTargetedState(t, orch, func(state orchestrator.State) bool {
				return state.DataSeq > initialState.DataSeq || tt.laneFailure && state.ManualRefresh.Status == telemetry.RefreshAttemptStatusFailed
			})
			if len(state.BoardIssues) != tt.wantCount {
				t.Fatalf("BoardIssues = %#v, want %d items", state.BoardIssues, tt.wantCount)
			}
			if tt.wantCount > 0 {
				if state.BoardIssues[0].State != tt.wantState {
					t.Fatalf("BoardIssues[0].State = %q, want %q", state.BoardIssues[0].State, tt.wantState)
				}
				if tt.laneFailure {
					if !reflect.DeepEqual(state.BoardIssues, initialState.BoardIssues) || state.DataSeq != initialState.DataSeq {
						t.Fatalf("failed targeted observation changed published ownership: before=%+v after=%+v", initialState.BoardIssues, state.BoardIssues)
					}
				} else if state.BoardIssues[0].StageUpdatedAt == nil || !state.BoardIssues[0].StageUpdatedAt.Equal(*tt.result.Issue.UpdatedAt) {
					t.Fatalf("StageUpdatedAt = %v, want external update time %v", state.BoardIssues[0].StageUpdatedAt, tt.result.Issue.UpdatedAt)
				}
			}
			if got := base.fetchCounts(); got != initialFetches {
				t.Fatalf("full fetch count = %d after targeted reconcile, want unchanged %d", got, initialFetches)
			}
			if got := tracker.reconcileCount(); got != 1 {
				t.Fatalf("ReconcileIssue() calls = %d, want 1", got)
			}
		})
	}
}

type targetedLaneConnector struct{ *targetedConnector }

func (c *targetedLaneConnector) CurrentLaneEntry(_ context.Context, issue connector.Issue) (connector.IssueStateTransition, bool, error) {
	if issue.State == "In Progress" {
		return connector.IssueStateTransition{}, false, errors.New("canonical lane history unavailable")
	}
	return connector.IssueStateTransition{}, false, nil
}

type targetedConnector struct {
	*fakeConnector
	mu         sync.Mutex
	result     connector.ReconcileResult
	reconciles int
}

func (c *targetedConnector) ReconcileIssue(context.Context, connector.ReconcileTarget) (connector.ReconcileResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconciles++
	return c.result, nil
}

func (c *targetedConnector) reconcileCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconciles
}

func (c *fakeConnector) fetchCounts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetchCandidateCount + c.fetchByStatesCount
}

func targetedReconcileIssue(state string) connector.Issue {
	updatedAt := time.Date(2026, 7, 10, 12, 30, 0, 0, time.UTC)
	issue := connector.NewIssue()
	issue.ID = "issue-1133"
	issue.Identifier = "digitaldrywood/detent#1133"
	issue.Title = "Board freshness"
	issue.State = state
	issue.URL = "https://github.com/digitaldrywood/detent/issues/1133"
	issue.UpdatedAt = &updatedAt
	return issue
}

func waitForTargetedState(t *testing.T, orch *orchestrator.Orchestrator, ready func(orchestrator.State) bool) orchestrator.State {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		state, err := orch.State(ctx)
		cancel()
		if err == nil && ready(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for targeted reconcile state")
	return orchestrator.State{}
}
