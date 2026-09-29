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
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "In Review", "Done", "Todo"}},
		{Name: "Blocked", Transitions: []string{"In Progress"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}
	hosted := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	head := strings.Repeat("c", 40)
	opened := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, Files: 2}
	for _, test := range []struct {
		name         string
		change       *runpkg.NativeChange
		states       []connector.WorkflowState
		statesErr    error
		updateErr    error
		plain        bool
		wantState    string
		wantComment  string
		wantDeferred bool
		wantContinue bool
	}{
		{name: "commits move to the configured review lane", change: opened, states: workflow, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "no commits end the work", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, wantState: "Done", wantComment: "nothing to review"},
		{name: "an unopened change is handed off, not reviewed", change: &runpkg.NativeChange{Changed: true, Error: "hub unavailable", HeadSHA: head, Files: 1}, states: workflow, wantDeferred: true},
		{name: "a workflow without the review lane is handed off, never ended", change: opened, states: hosted, wantDeferred: true},
		{name: "a workflow without a terminal move is handed off", change: &runpkg.NativeChange{}, states: []connector.WorkflowState{{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked"}}, {Name: "Blocked"}}, wantDeferred: true},
		{name: "an unreadable workflow is handed off", change: opened, statesErr: errors.New("hub unavailable"), wantDeferred: true},
		{name: "a refused lane write is handed off", change: opened, states: workflow, updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "no native change keeps the ordinary path", states: workflow, wantContinue: true},
		{name: "a connector without a workflow keeps the ordinary path", change: &runpkg.NativeChange{}, plain: true, wantContinue: true},
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
			cfg.AutoPromote.SourceState = "In Review"
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
			retry, retried := state.Retry[issue.ID]
			_, deferred := state.deferredCompletions[issue.ID]
			if deferred != test.wantDeferred || test.wantDeferred && !retry.CompletionDeferred {
				t.Fatalf("deferred = %t (retry %#v), want %t", deferred, retry, test.wantDeferred)
			}
			if continued := retried && !retry.CompletionDeferred; continued != test.wantContinue {
				t.Fatalf("continuation scheduled = %t, want %t", continued, test.wantContinue)
			}
			if test.wantContinue || test.wantDeferred {
				for _, update := range tick.updates {
					if test.updateErr == nil {
						t.Fatalf("the item was moved: %#v", tick.updates)
					}
					if update.state != "In Review" {
						t.Fatalf("refused write targeted %s", update.state)
					}
				}
				if test.wantDeferred && len(tick.comments) != 0 {
					t.Fatalf("a handed-off item was commented on: %#v", tick.comments)
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
		{name: "opened", change: runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: "0123456789abcdef", Files: 3}, want: []string{"change_1", "3 files", "head 0123456789ab)", "In Progress to In Review"}},
		{name: "opened without a version", change: runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionError: "publish version: policy_mismatch", HeadSHA: "0123456789abcdef", Files: 3}, want: []string{"change_1", "No version was published for review: publish version: policy_mismatch", "next successful run publishes one"}},
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
