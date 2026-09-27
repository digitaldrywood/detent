package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

type nativeWorkflowConnector struct {
	*autoPromoteTickConnector
	states    []connector.WorkflowState
	statesErr error
}

func (c *nativeWorkflowConnector) WorkflowStates(context.Context) ([]connector.WorkflowState, error) {
	return c.states, c.statesErr
}

// TestNativeChangeRunCompletion drives a successful native run's completion
// through the success path: the orchestrator moves the item along the hub
// workflow through the lane ledger and releases the claim instead of
// continuing the item in its active lane.
func TestNativeChangeRunCompletion(t *testing.T) {
	t.Parallel()
	workflow := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}
	head := strings.Repeat("c", 40)
	for _, test := range []struct {
		name         string
		change       *runpkg.NativeChange
		states       []connector.WorkflowState
		statesErr    error
		updateErr    error
		plain        bool
		wantState    string
		wantComment  string
		wantContinue bool
	}{
		{name: "commits move to review", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, Files: 2}, states: workflow,
			wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "no commits end the work", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow,
			wantState: "Done", wantComment: "nothing to review"},
		{name: "a change that could not be opened still leaves dispatch", change: &runpkg.NativeChange{Changed: true, Error: "hub unavailable", HeadSHA: head, Files: 1}, states: workflow,
			wantState: "In Review", wantComment: "could not be opened: hub unavailable"},
		{name: "no native change keeps the ordinary path", states: workflow, wantContinue: true},
		{name: "a connector without a workflow keeps the ordinary path", change: &runpkg.NativeChange{}, plain: true, wantContinue: true},
		{name: "an unreadable workflow keeps the ordinary path", change: &runpkg.NativeChange{}, statesErr: errors.New("hub unavailable"), wantContinue: true},
		{name: "a workflow with no way out keeps the ordinary path", change: &runpkg.NativeChange{}, states: []connector.WorkflowState{{Name: "In Progress", Dispatchable: true}}, wantContinue: true},
		{name: "a refused lane write keeps the ordinary path", change: &runpkg.NativeChange{}, states: workflow, updateErr: errors.New("stale fencing token"), wantContinue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			issue := completionTransitionIssue("In Progress", "")
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}, updateErr: test.updateErr}
			var tracker connector.Connector = &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: test.states, statesErr: test.statesErr}
			if test.plain {
				tracker = tick
			}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
			attempts := &recordingWorkAttemptStore{}
			scheduling := &hubSchedulingSource{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: scheduling}
			state := newState(cfg)
			now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Mode: runpkg.RunModeImplement, DispatchSourceState: "In Progress", StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement},
				Result:  runpkg.RunResult{FinalState: FinalStateCompleted, NativeChange: test.change},
			})
			_, continued := state.Retry[issue.ID]
			if continued != test.wantContinue {
				t.Fatalf("continuation scheduled = %t, want %t", continued, test.wantContinue)
			}
			if test.wantContinue {
				for _, update := range tick.updates {
					if update.state != "In Progress" && test.updateErr == nil {
						t.Fatalf("the ordinary path moved the item: %#v", tick.updates)
					}
				}
				return
			}
			if len(tick.updates) != 1 || tick.updates[0].state != test.wantState {
				t.Fatalf("lane updates = %#v, want one to %s", tick.updates, test.wantState)
			}
			if len(tick.comments) != 1 || !strings.Contains(strings.ToLower(tick.comments[0].body), strings.ToLower(test.wantComment)) {
				t.Fatalf("comments = %#v, want one containing %q", tick.comments, test.wantComment)
			}
			if _, claimed := state.Claimed[issue.ID]; claimed || scheduling.releases != 1 {
				t.Fatalf("claim retained = %t, releases = %d", claimed, scheduling.releases)
			}
			completed, ok := state.Completed[issue.ID]
			if !ok || completed.Issue.State != test.wantState {
				t.Fatalf("completed = %#v, present = %t", completed, ok)
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("attempt completions = %#v", attempts.completions)
			}
		})
	}
}

func TestNativeCompletionComment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change runpkg.NativeChange
		want   []string
	}{
		{name: "opened", change: runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: "0123456789abcdef", Files: 3}, want: []string{"change_1", "3 files", "head 0123456789ab)", "In Progress to In Review"}},
		{name: "refused", change: runpkg.NativeChange{Changed: true, Error: "boom", Files: 1}, want: []string{"could not be opened: boom", "head its base"}},
		{name: "unchanged", change: runpkg.NativeChange{BaseSHA: "abc"}, want: []string{"against abc", "nothing to review"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := nativeCompletionComment(&test.change, "In Progress", "In Review")
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("comment %q does not contain %q", got, want)
				}
			}
		})
	}
}
