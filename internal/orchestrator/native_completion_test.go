package orchestrator

import (
	"context"
	"encoding/json"
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
	// reviewed is the change's review state when the completion is applied;
	// nil answers as a failed read.
	reviewed *bool
}

func (c *nativeWorkflowConnector) ChangeReviewed(context.Context, string, string, string) (bool, error) {
	if c.reviewed == nil {
		return false, errors.New("hub unavailable")
	}
	return *c.reviewed, nil
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
	landing := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "In Review", "Merging", "Done"}},
		{Name: "In Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "In Review", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	undispatched := append([]connector.WorkflowState(nil), landing...)
	undispatched[3].Dispatchable = false
	blockedLanding := append([]connector.WorkflowState(nil), landing...)
	blockedLanding[1].Transitions = append([]string{"Blocked"}, blockedLanding[1].Transitions...)
	blockedLanding = append(blockedLanding, connector.WorkflowState{Name: "Blocked", Transitions: []string{"In Progress"}})
	yes, no := true, false
	head := strings.Repeat("c", 40)
	opened := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, Files: 2}
	waiting := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Files: 2}
	accepted := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, HeadSHA: head, Files: 2}
	deliveryErr := &runpkg.DeliverableRecoveryError{Err: &runpkg.DeliverableCommandError{OperationClass: "pull_request", Message: "pull request publication failed"}}
	for _, test := range []struct {
		name          string
		finalState    string
		finalMessage  string
		runErr        error
		wantHuman     bool
		noUsage       bool
		change        *runpkg.NativeChange
		states        []connector.WorkflowState
		statesErr     error
		reviewed      *bool
		updateErr     error
		plain         bool
		quotaWait     bool
		humanReview   *bool
		wantState     string
		wantComment   string
		wantDeferred  bool
		wantContinue  bool
		wantAbandoned bool
	}{
		{name: "successful coding publishes during landing quota wait", change: accepted, states: landing, quotaWait: true, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "commits move to the configured review lane", change: waiting, states: workflow, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "a version that needs no reviewer goes straight to landing", change: accepted, states: landing, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "a version waiting for a reviewer goes to review", change: waiting, states: landing, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "disabled review sends unaccepted change to blocked", change: waiting, states: blockedLanding, humanReview: &no, wantState: "Blocked", wantComment: "opened Change Request change_1"},
		{name: "disabled review lands accepted change", change: accepted, states: blockedLanding, humanReview: &no, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "an accepted version without a landing move goes to review", change: accepted, states: workflow, wantState: "In Review", wantComment: "so it waits in In Review"},
		{name: "an approval that arrived after the publish lands", change: waiting, states: landing, reviewed: &yes, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "a run that published no version cannot succeed on an earlier reviewed one", change: opened, states: landing, reviewed: &yes, wantDeferred: true},
		{name: "unavailable version evidence cannot succeed", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionError: "publication unavailable"}, states: landing, wantDeferred: true},
		{name: "native publication authority failure is instance owned", states: workflow, runErr: errors.Join(runpkg.ErrExecutionAuthorityUnavailable, errors.New("final diff unavailable")), wantAbandoned: true},
		{name: "a version that lost its acceptance goes to review", change: accepted, states: landing, reviewed: &no, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "an accepted version never goes to a landing lane that does not dispatch", change: accepted, states: undispatched, wantState: "In Review", wantComment: "so it waits in In Review"},
		{name: "unchanged work with final approval question needs human", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "May I merge?", wantHuman: true},
		{name: "unchanged work with final structured blocker needs human", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the migration\n```", wantHuman: true},
		{name: "human attention without a produced change", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Choose the storage architecture", wantHuman: true},
		{name: "human attention without final text", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, wantHuman: true, noUsage: true},
		{name: "human attention with synthetic unchanged change", change: &runpkg.NativeChange{}, states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", wantHuman: true},
		{name: "human attention with failed producer and no change", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: deliveryErr, wantHuman: true},
		{name: "human attention retains instance workspace failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, runpkg.ErrWorkspacePreparation)},
		{name: "human attention retains checkpoint failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("checkpoint persistence failed")), wantContinue: true},
		{name: "human attention retains lease failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("native execution lease lost")), wantContinue: true},
		{name: "human attention retains session failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("session persistence failed")), wantContinue: true},
		{name: "human attention defers a refused lane write", states: workflow, finalMessage: "May I merge?", updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "no commits end the work", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, wantState: "Done", wantComment: "nothing to review"},
		{name: "an unopened change is handed off, not reviewed", change: &runpkg.NativeChange{Changed: true, Error: "hub unavailable", HeadSHA: head, Files: 1}, states: workflow, wantDeferred: true},
		{name: "a workflow without the review lane is handed off, never ended", change: waiting, states: hosted, wantDeferred: true},
		{name: "a workflow without a terminal move is handed off", change: &runpkg.NativeChange{}, states: []connector.WorkflowState{{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked"}}, {Name: "Blocked"}}, wantDeferred: true},
		{name: "an unreadable workflow is handed off", change: waiting, statesErr: errors.New("hub unavailable"), wantDeferred: true},
		{name: "a refused lane write is handed off", change: waiting, states: workflow, updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "missing native result stays with native completion", states: workflow, wantDeferred: true},
		{name: "non-native final question keeps the ordinary path", finalMessage: "May I merge?", plain: true, wantContinue: true},
		{name: "a connector without a workflow keeps the ordinary path", change: &runpkg.NativeChange{}, plain: true, wantContinue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			issue := completionTransitionIssue("In Progress", "")
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}, updateErr: test.updateErr}
			var tracker connector.Connector = &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: test.states, statesErr: test.statesErr, reviewed: test.reviewed}
			if test.plain {
				tracker = tick
			}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
			cfg.AutoPromote.SourceState = "In Review"
			cfg.AutoPromote.HumanReview = test.humanReview
			attempts := &recordingWorkAttemptStore{}
			scheduling := &hubSchedulingSource{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: scheduling}
			state := newState(cfg)
			now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
			if test.quotaWait {
				orch.setGitHubRESTCapacityOutage(&state, githubRESTBudgetEvidence{Consumer: "worker", CredentialIdentity: "runner", RateLimitKind: "primary_exhausted", ObservedAt: now, ResetAt: now.Add(time.Hour)}, now)
			}
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Generation: 7, SessionID: "native-session", Tokens: TokenTotals{TotalTokens: 42}, Mode: runpkg.RunModeImplement, DispatchSourceState: "In Progress", StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			finalState := test.finalState
			if finalState == "" {
				finalState = FinalStateCompleted
			}
			tokens := TokenTotals{TotalTokens: 42}
			if test.noUsage {
				tokens = TokenTotals{}
			}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now, Err: test.runErr,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement, WorkAttemptID: 42, Generation: 7},
				Result:  runpkg.RunResult{FinalState: finalState, FinalMessage: test.finalMessage, NativeChange: test.change, Tokens: tokens, DiffStats: DiffStats{Status: "clean", HeadSHA: head}},
			})
			if test.wantAbandoned {
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalAbandoned || !strings.Contains(attempts.completions[0].ErrorMessage, "final diff unavailable") {
					t.Fatalf("native authority failure = %#v", attempts.completions)
				}
				if len(tick.updates) != 0 || len(state.Blocked) != 0 || len(state.Completed) != 0 || len(state.deferredCompletions) != 0 || len(state.FailureBreaker.Failures) != 0 {
					t.Fatal("native authority failure changed the issue or repeated an obsolete completion")
				}
				return
			}
			retry, retried := state.Retry[issue.ID]
			_, deferred := state.deferredCompletions[issue.ID]
			if deferred != test.wantDeferred || test.wantDeferred && !retry.CompletionDeferred {
				t.Fatalf("deferred = %t (retry %#v), want %t", deferred, retry, test.wantDeferred)
			}
			if continued := retried && !retry.CompletionDeferred; continued != test.wantContinue {
				t.Fatalf("continuation scheduled = %t, want %t", continued, test.wantContinue)
			}
			if test.wantHuman {
				blocked, ok := state.Blocked[issue.ID]
				if !ok || blocked.Reason != permissionWaitReason || blocked.Recovery.Owner != blockedRecoveryOwnerHuman || blocked.Recovery.WorkAttemptID != 42 {
					t.Fatalf("human outcome = %#v, present = %t", blocked, ok)
				}
				if _, completed := state.Completed[issue.ID]; completed || len(tick.updates) != 1 || tick.updates[0].state != "Blocked" {
					t.Fatalf("human attention completed = %t; updates = %#v", completed, tick.updates)
				}
				if _, claimed := state.Claimed[issue.ID]; claimed || scheduling.releases < 1 {
					t.Fatalf("claim retained = %t, releases = %d", claimed, scheduling.releases)
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalNoProgress || attempts.completions[0].ErrorClass != permissionWaitReason {
					t.Fatalf("attempt completions = %#v", attempts.completions)
				}
				var metadata struct {
					PermissionWait permissionWaitRecord `json:"permission_wait"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
					t.Fatal(err)
				}
				if metadata.PermissionWait.WorkAttemptID != 42 || metadata.PermissionWait.SessionID != "native-session" || metadata.PermissionWait.Question == "" {
					t.Fatalf("human outcome provenance = %#v", metadata.PermissionWait)
				}
				if state.TokenTotals.TotalTokens != 42 || state.DiffStats[issue.ID].HeadSHA != head {
					t.Fatalf("telemetry lost: tokens = %#v, diff = %#v", state.TokenTotals, state.DiffStats[issue.ID])
				}
				if orch.recoverCauseBlockedIssue(t.Context(), &state, blocked.Issue, now.Add(24*time.Hour)) {
					t.Fatal("human outcome automatically recovered")
				}
				return
			}
			if test.runErr != nil && !test.wantHuman {
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalFailure || attempts.completions[0].ErrorClass == permissionWaitReason || !strings.Contains(attempts.completions[0].ErrorMessage, test.runErr.Error()) {
					t.Fatalf("failure outcome = %#v", attempts.completions)
				}
				if _, blocked := state.Blocked[issue.ID]; blocked {
					t.Fatal("mixed failure became a human park")
				}
				if _, completed := state.Completed[issue.ID]; completed {
					t.Fatal("mixed failure completed")
				}
				for _, update := range tick.updates {
					if update.state != "Todo" {
						t.Fatalf("failure transition = %#v", update)
					}
				}
				return
			}
			if test.wantContinue || test.wantDeferred {
				if test.wantDeferred && len(attempts.completions) != 0 {
					t.Fatalf("deferred native completion recorded a terminal outcome: %#v", attempts.completions)
				}
				for _, update := range tick.updates {
					if test.updateErr == nil {
						t.Fatalf("the item was moved: %#v", tick.updates)
					}
					if update.state != "In Review" && (test.finalMessage == "" || update.state != "Blocked") {
						t.Fatalf("refused write targeted %s", update.state)
					}
				}
				if test.wantDeferred && test.finalMessage == "" && len(tick.comments) != 0 {
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
		{name: "opened and needing no reviewer", change: runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, HeadSHA: "0123456789abcdef", Files: 3}, want: []string{"needs no further review", "runner lands it next"}},
		{name: "unchanged", change: runpkg.NativeChange{BaseSHA: "abc"}, want: []string{"against abc", "nothing to review"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			to := "In Review"
			if test.change.Reviewed {
				to = "Merging"
			}
			got := nativeCompletionComment(&test.change, "In Progress", to)
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("comment %q does not contain %q", got, want)
				}
			}
		})
	}
}
