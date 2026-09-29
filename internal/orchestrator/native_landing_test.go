package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

// TestNativeLandingRunCompletion drives a landing run's completion: a landed
// version is read back from the hub, which finished it, and a refused one
// goes back to the review lane with the reason. Both complete the attempt.
func TestNativeLandingRunCompletion(t *testing.T) {
	t.Parallel()
	workflow := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	head := strings.Repeat("c", 40)
	landed := &runpkg.NativeLanding{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Landed: true, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}
	refused := &runpkg.NativeLanding{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, RefusalKind: "base_protected", Refusal: "the base branch main refused the push: GH006. Allow the runner to push to main, or enable GitHub pull request mode for this project."}
	for _, test := range []struct {
		name         string
		landing      *runpkg.NativeLanding
		hubState     string
		states       []connector.WorkflowState
		statesErr    error
		updateErr    error
		plain        bool
		wantState    string
		wantMoves    int
		wantComment  string
		wantDeferred bool
		wantContinue bool
	}{
		{name: "a landed version is finished by the hub", landing: landed, hubState: "Done", states: workflow, wantState: "Done", wantComment: "Landed Change Request change_1", wantMoves: 0},
		{name: "a refused landing returns to review with the reason", landing: refused, hubState: "Merging", states: workflow, wantState: "Human Review", wantComment: "enable GitHub pull request mode", wantMoves: 1},
		{name: "a refused landing with no review lane is handed off", landing: refused, hubState: "Merging", states: []connector.WorkflowState{{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}, wantDeferred: true},
		{name: "an unreadable workflow is handed off", landing: refused, hubState: "Merging", statesErr: errors.New("hub unavailable"), wantDeferred: true},
		{name: "a refused lane write is handed off", landing: refused, hubState: "Merging", states: workflow, updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "no landing keeps the ordinary path", hubState: "Merging", states: workflow, wantContinue: true},
		{name: "a connector without a workflow keeps the ordinary path", landing: landed, hubState: "Done", plain: true, wantContinue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			issue := completionTransitionIssue("Merging", "")
			hubIssue := cloneIssue(issue)
			hubIssue.State = test.hubState
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{hubIssue}, updateErr: test.updateErr}
			var tracker connector.Connector = &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: test.states, statesErr: test.statesErr}
			if test.plain {
				tracker = tick
			}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Merging"}, TerminalStates: []string{"Done"}})
			cfg.AutoPromote.SourceState = "Human Review"
			attempts := &recordingWorkAttemptStore{}
			scheduling := &hubSchedulingSource{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: scheduling}
			state := newState(cfg)
			now := time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC)
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Mode: runpkg.RunModeMerge, DispatchSourceState: "Merging", StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			output := runpkg.RunOutputNativeLanded
			if test.landing != nil && !test.landing.Landed {
				output = runpkg.RunOutputNativeLandingRefused
			}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge},
				Result:  runpkg.RunResult{FinalState: FinalStateCompleted, Output: output, NativeLanding: test.landing},
			})
			retry, retried := state.Retry[issue.ID]
			_, deferred := state.deferredCompletions[issue.ID]
			if deferred != test.wantDeferred || test.wantDeferred && !retry.CompletionDeferred {
				t.Fatalf("deferred = %t (retry %#v), want %t", deferred, retry, test.wantDeferred)
			}
			if test.wantContinue {
				if _, completedByLanding := state.Completed[issue.ID]; completedByLanding && retried && !retry.CompletionDeferred {
					t.Fatalf("the landing path completed an ordinary run")
				}
				for _, comment := range tick.comments {
					if strings.Contains(comment.body, "Landed Change Request") {
						t.Fatalf("the landing path commented on an ordinary run: %#v", tick.comments)
					}
				}
				return
			}
			if test.wantDeferred {
				if len(tick.comments) != 0 {
					t.Fatalf("a handed-off item was commented on: %#v", tick.comments)
				}
				return
			}
			if len(tick.updates) != test.wantMoves || test.wantMoves == 1 && tick.updates[0].state != test.wantState {
				t.Fatalf("lane updates = %#v, want %d to %s", tick.updates, test.wantMoves, test.wantState)
			}
			if len(tick.comments) != 1 || !strings.Contains(tick.comments[0].body, test.wantComment) {
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

func TestNativeLandingComment(t *testing.T) {
	t.Parallel()
	landed := nativeLandingComment(&runpkg.NativeLanding{ChangeID: "change_1", HeadSHA: "0123456789abcdef", Landed: true, MergeSHA: "fedcba9876543210", BaseRef: "main", Method: "squash"}, "Merging", "Done")
	for _, want := range []string{"change_1", "head 0123456789ab", "on main as fedcba987654", "by squash", "Merging to Done"} {
		if !strings.Contains(landed, want) {
			t.Fatalf("landed comment %q does not contain %q", landed, want)
		}
	}
	refused := nativeLandingComment(&runpkg.NativeLanding{ChangeID: "change_1", HeadSHA: "0123456789abcdef", RefusalKind: "conflict", Refusal: "squashing conflicts."}, "Merging", "Human Review")
	for _, want := range []string{"was not landed: squashing conflicts.", "Merging to Human Review"} {
		if !strings.Contains(refused, want) {
			t.Fatalf("refused comment %q does not contain %q", refused, want)
		}
	}
}

func TestNativeCandidateStatesIncludeTheLandingLane(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
	tick := &autoPromoteTickConnector{}
	for _, test := range []struct {
		name      string
		connector connector.Connector
		want      []string
	}{
		{name: "native adds Merging", connector: &nativeWorkflowConnector{autoPromoteTickConnector: tick}, want: []string{"todo", "in progress", "merging"}},
		{name: "other trackers keep their configuration", connector: tick, want: []string{"todo", "in progress"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			orch := &Orchestrator{cfg: cfg, connector: test.connector}
			state := newState(cfg)
			got := orch.candidateFetchStatesForTick(&state)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("candidate states = %v, want %v", got, test.want)
			}
			issue := completionTransitionIssue("Merging", "")
			mode := orch.dispatchMode(t.Context(), &state, issue)
			wantMode := runpkg.RunModeImplement
			if test.name == "native adds Merging" {
				wantMode = runpkg.RunModeMerge
			}
			if mode != wantMode {
				t.Fatalf("dispatchMode(Merging) = %q, want %q", mode, wantMode)
			}
		})
	}
}

// TestNativeMergingLaneIsNotReconciledAsAStalePullRequest checks that a
// native item waiting to land is left to the landing run: the stale-Merging
// reconciliation reads pull request state, and a native item has none.
func TestNativeMergingLaneIsNotReconciledAsAStalePullRequest(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Merging"}, TerminalStates: []string{"Done"}})
	now := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		native    bool
		wantMoves int
	}{
		{name: "native stays for the landing run", native: true, wantMoves: 0},
		{name: "a tracker with pull requests still reconciles", native: false, wantMoves: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			issue := completionTransitionIssue("Merging", "")
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			var tracker connector.Connector = tick
			if test.native {
				tracker = &nativeWorkflowConnector{autoPromoteTickConnector: tick}
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			transitioned := orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(transitioned) != test.wantMoves || len(tick.updates) != test.wantMoves {
				t.Fatalf("transitioned = %#v, updates = %#v, want %d", transitioned, tick.updates, test.wantMoves)
			}
		})
	}
}
