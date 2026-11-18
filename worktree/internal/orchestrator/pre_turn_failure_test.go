package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPreTurnFailuresRetryWithoutPausingProject(t *testing.T) {
	t.Parallel()
	startup := backendcapacity.NewError(backendcapacity.Scope{}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Kind: backendcapacity.StartupTimeoutKind}, context.DeadlineExceeded)
	protocol := errors.New("codex turn/start: JSON-RPC -32600 invalid request")
	workspace := fmt.Errorf("%w: after_create exited 1", runpkg.ErrWorkspacePreparation)
	wrappedStartup := errors.Join(runpkg.NewCancellationCause(startup, "orchestrator.parent_context"), startup)
	wrappedMergeStartup := errors.Join(runpkg.NewCancellationCause(runpkg.ErrMergeWorkerStartupTimeout, "orchestrator.parent_context"), context.Canceled)
	inputRefusal := &codex.ResponseError{Request: "turn/start", Code: -32602, Message: "Input exceeds the maximum length of 1048576"}
	details, ok := codex.ClassifyCapacityError(inputRefusal, nil, time.Now())
	if !ok || details.Kind != backendcapacity.StartupFailureKind {
		t.Fatalf("input refusal classification = %+v, classified = %v", details, ok)
	}
	inputFailure := backendcapacity.NewError(backendcapacity.Scope{}, details, inputRefusal)
	for _, tt := range []struct {
		name     string
		failures []error
	}{
		{"protocol", []error{protocol, protocol, protocol}},
		{"local_checkpoint_changed", []error{fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired)}},
		{"policy_mismatch", []error{fmt.Errorf("%w: policy_mismatch", runpkg.ErrNativeRecoveryRequired)}},
		{"checkpoint_requires_recovery", []error{fmt.Errorf("%w: checkpoint_requires_recovery", runpkg.ErrNativeRecoveryRequired)}},
		{"checkpoint_unavailable", []error{fmt.Errorf("%w: checkpoint_unavailable", runpkg.ErrNativeRecoveryRequired)}},
		{"session_restart_required", []error{fmt.Errorf("%w: session_restart_required", runpkg.ErrNativeRecoveryRequired)}},
		{"workspace", []error{workspace, workspace, workspace}},
		{"startup", []error{startup, startup, startup}},
		{"turn input refusal", []error{inputFailure, inputFailure, inputFailure}},
		{"mixed", []error{protocol, startup, workspace}},
		{"supervisor startup deadline", []error{wrappedStartup, wrappedStartup, wrappedStartup}},
		{"supervisor merge startup timer", []error{wrappedMergeStartup, wrappedMergeStartup, wrappedMergeStartup}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, BlockedRecovery: BlockedRecoveryConfig{}})
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{}}
			o := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: &terminalRetryWorkAttemptStore{}}
			state := newState(cfg)
			for index, failure := range tt.failures {
				issue := terminalRetryTestIssue(strconv.Itoa(index))
				tracker.issues[issue.ID] = issue
				state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: int64(index + 1), DispatchSourceState: "Todo", DispatchTargetState: "In Progress", StartedAt: now.Add(-time.Second)}
				o.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Err: decidedTestFailure{failure, "instance"}, CompletedAt: now})
				retry, retried := state.Retry[issue.ID]
				if tracker.issues[issue.ID].State != "Todo" || len(state.Blocked) != 0 || len(tracker.comments) != 0 || !retried || retry.Attempt != 2 || !retry.DueAt.After(now) {
					t.Fatalf("pre-turn failure %d did not retry in its lane: state=%s blocked=%v comments=%v retry=%#v", index+1, tracker.issues[issue.ID].State, state.Blocked, tracker.comments, state.Retry)
				}
			}
		})
	}
}

func TestPreTurnAttemptTaxonomy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		class   string
		metrics string
		want    bool
	}{
		{"runner before turn", workAttemptErrorRunner, `{"turns":0}`, true},
		{"runner after turn", workAttemptErrorRunner, `{"turns":1}`, false},
		{"runner used tokens", workAttemptErrorRunner, `{"turns":0,"total_tokens":5}`, false},
		{"unknown historical turns", workAttemptErrorRunner, `{}`, false},
		{"missing historical turns", workAttemptErrorRunner, "", false},
		{"malformed metrics", workAttemptErrorRunner, `{`, false},
		{"workspace hook", workAttemptErrorWorkspace, "", true},
		{"startup timeout", backendcapacity.StartupTimeoutErrorClass, "", true},
		{"startup exit", backendcapacity.StartupFailureErrorClass, "", true},
		{"provider capacity", backendcapacity.ErrorClass, `{"turns":0}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := preTurnAttempt(telemetry.WorkAttempt{ErrorClass: tt.class, MetricsJSON: tt.metrics}); got != tt.want {
				t.Fatalf("pre-turn = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreTurnRestoresDispatchLane(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Todo", "Rework", "Merging"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			issue := terminalRetryTestIssue("lane")
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "Rework", "Merging", "In Progress"}})
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			running := Running{Issue: issue, DispatchSourceState: lane, DispatchTargetState: "In Progress"}
			o.restorePreTurnIssue(t.Context(), &state, running, time.Now())
			if tracker.issues[issue.ID].State != lane || len(tracker.comments) != 0 {
				t.Fatalf("issue = %#v, comments = %v", tracker.issues[issue.ID], tracker.comments)
			}
		})
	}
}

type decidedTestFailure struct {
	error
	outcome string
}

func (e decidedTestFailure) Unwrap() error { return e.error }
func (e decidedTestFailure) FailureAttribution() tracker.FailureAttribution {
	return tracker.FailureAttribution{Outcome: e.outcome, Reason: "fake decision from failure evidence"}
}

func TestNativePreTurnRecoveryRetryOwner(t *testing.T) {
	t.Parallel()
	workflow := func(edit func([]connector.WorkflowState)) []connector.WorkflowState {
		states := []connector.WorkflowState{
			{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
			{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
			{Name: "In Review", Transitions: []string{"Rework", "In Progress"}},
			{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
			{Name: "Blocked", Transitions: []string{"In Progress"}},
			{Name: "Done", Terminal: true},
		}
		if edit != nil {
			edit(states)
		}
		return states
	}
	withoutTodo := []string{"Rework", "In Progress", "Merging"}
	withTodo := []string{"Todo", "Rework", "In Progress", "Merging"}
	recoveryErr := fmt.Errorf("%w: checkpoint_unavailable", runpkg.ErrNativeRecoveryRequired)
	const base, limit = 10 * time.Second, 40 * time.Second
	for _, tt := range []struct {
		name            string
		states          []connector.WorkflowState
		active          []string
		liveState       string
		failures        int
		releaseErr      error
		statesErr       error
		staleGeneration bool
		succeed         bool
		wantState       string
		wantUpdates     []string
		wantDelays      []time.Duration
		wantReleases    int
		wantDeferred    bool
	}{
		{name: "repeated failure without Todo lane grows backoff in place", states: workflow(nil), active: withoutTodo, failures: 4, wantState: "In Progress", wantDelays: []time.Duration{base, 2 * base, limit, limit}, wantReleases: 4},
		{name: "unreachable Todo lane retries in place", states: workflow(nil), active: withTodo, failures: 2, wantState: "In Progress", wantDelays: []time.Duration{base, 2 * base}, wantReleases: 2},
		{name: "reachable Todo lane keeps the Todo retry", states: workflow(func(states []connector.WorkflowState) { states[1].Transitions = append(states[1].Transitions, "Todo") }), active: withTodo, failures: 1, wantState: "Todo", wantUpdates: []string{"Todo"}, wantDelays: []time.Duration{base}, wantReleases: 1},
		{name: "lost release response keeps one retry owner", states: workflow(nil), active: withoutTodo, failures: 1, releaseErr: errors.New("release response lost"), wantState: "In Progress", wantDelays: []time.Duration{base}, wantReleases: 1},
		{name: "unreadable workflow replays into one retry owner", states: workflow(nil), active: withoutTodo, failures: 1, statesErr: errors.New("workflow unavailable"), wantState: "In Progress", wantDelays: []time.Duration{base}, wantReleases: 1},
		{name: "eventual success settles normally", states: workflow(nil), active: withoutTodo, failures: 2, succeed: true, wantState: "In Review", wantUpdates: []string{"In Review"}, wantDelays: []time.Duration{base, 2 * base}, wantReleases: 3},
		{name: "human stop lane is not retried", states: workflow(nil), active: withoutTodo, liveState: "Blocked", failures: 1, wantState: "Blocked", wantReleases: 1},
		{name: "non-dispatchable lane is not retried", states: workflow(func(states []connector.WorkflowState) { states[1].Dispatchable = false }), active: withoutTodo, failures: 1, wantState: "In Progress", wantDeferred: true},
		{name: "operator-only lane is not retried", states: workflow(func(states []connector.WorkflowState) { states[1].OperatorOnly = true }), active: withoutTodo, failures: 1, wantState: "In Progress", wantDeferred: true},
		{name: "changed ownership is not retried", states: workflow(nil), active: withoutTodo, failures: 1, staleGeneration: true, wantState: "In Progress"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := completionTransitionIssue("In Progress", "")
			live := issue
			live.State = firstNonBlank(tt.liveState, issue.State)
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{live}}
			native := &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: tt.states, statesErr: tt.statesErr, reviewed: new(false)}
			cfg := normalizeConfig(Config{ActiveStates: tt.active, TerminalStates: []string{"Done"}, FailureRetryBaseDelay: base, MaxRetryBackoff: limit})
			cfg.AutoPromote.SourceState = "In Review"
			cfg.AutoPromote.HumanReview = new(true)
			scheduling := &nativeCompletionScheduling{hubSchedulingSource: &hubSchedulingSource{releaseError: tt.releaseErr}}
			o := &Orchestrator{cfg: cfg, connector: native, scheduling: scheduling}
			state := newState(cfg)
			now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
			o.now = func() time.Time { return now }
			retryOwned := true
			scheduling.release = func() {
				retry, queued := state.Retry[issue.ID]
				retryOwned = retryOwned && queued && !retry.CompletionDeferred && retry.DueAt.After(now)
			}
			if len(tt.wantDelays) == 0 {
				scheduling.release = nil
			}
			dispatch := func(attempt int) Running {
				running := Running{Issue: issue, Attempt: attempt, Generation: 7, Mode: runpkg.RunModeImplement, StartedAt: now.Add(-time.Second)}
				delete(state.Retry, issue.ID)
				state.Running[issue.ID] = running
				state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: running.StartedAt}
				return running
			}
			attempt := 0
			for failure := range tt.failures {
				dispatch(attempt)
				event := runpkg.Completion{IssueID: issue.ID, Err: recoveryErr, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement, Generation: 7}}
				if tt.staleGeneration {
					event.Request.Generation = 8
				}
				o.handleRunResult(t.Context(), &state, event)
				if tt.statesErr != nil {
					deferred := state.Retry[issue.ID]
					if _, held := state.deferredCompletions[issue.ID]; !held || !deferred.CompletionDeferred || scheduling.releases != 0 {
						t.Fatalf("unreadable workflow did not hold the completion: deferred=%v releases=%d", deferred.CompletionDeferred, scheduling.releases)
					}
					native.statesErr = nil
					o.retryDeferredCompletions(t.Context(), &state, deferred.DueAt)
				}
				if tt.releaseErr != nil {
					o.handleRunResult(t.Context(), &state, event)
				}
				retry, queued := state.Retry[issue.ID]
				if len(tt.wantDelays) == 0 {
					if queued != tt.wantDeferred || retry.CompletionDeferred != tt.wantDeferred {
						t.Fatalf("pre-turn failure scheduled a dispatchable retry: queued=%v deferred=%v attempt=%d", queued, retry.CompletionDeferred, retry.Attempt)
					}
					continue
				}
				want := tt.wantDelays[failure]
				delay := retry.DueAt.Sub(now)
				if !queued || retry.CompletionDeferred || retry.TrackerUnavailable || retry.Attempt != attempt+1 || delay > want || delay < want-want/8 || retry.Issue.State != tick.stateIssues[0].State || !strings.Contains(retry.Error, "checkpoint_unavailable") {
					t.Fatalf("failure %d retry queued=%v deferred=%v attempt=%d lane=%s delay=%s error=%q; want attempt=%d delay=%s", failure+1, queued, retry.CompletionDeferred, retry.Attempt, retry.Issue.State, delay, retry.Error, attempt+1, want)
				}
				if len(state.deferredCompletions) != 0 || len(state.Blocked) != 0 || len(state.Claimed) != 0 || len(state.Running) != 0 {
					t.Fatalf("failure %d left a second owner: deferred=%d blocked=%d claimed=%d running=%d", failure+1, len(state.deferredCompletions), len(state.Blocked), len(state.Claimed), len(state.Running))
				}
				for _, at := range []time.Time{now, retry.DueAt.Add(-time.Nanosecond)} {
					if reason := knownDispatchWaitReason(retry.Issue, &state, dueRetriesByIssue(&state, at), cfg.TerminalStates); reason != dispatchSkipRetryPending {
						t.Fatalf("failure %d released claim reclaimable %s before backoff: reason=%q", failure+1, retry.DueAt.Sub(at), reason)
					}
				}
				if reason := knownDispatchWaitReason(retry.Issue, &state, dueRetriesByIssue(&state, retry.DueAt), cfg.TerminalStates); reason != "" {
					t.Fatalf("failure %d due retry still waits: %q", failure+1, reason)
				}
				attempt = retry.Attempt
				now = retry.DueAt
			}
			if tt.succeed {
				scheduling.release = nil
				dispatch(attempt)
				head := strings.Repeat("c", 40)
				result := runpkg.RunResult{FinalState: FinalStateCompleted, TurnStarted: true, Tokens: TokenTotals{TotalTokens: 42}, DiffStats: DiffStats{Status: "clean", HeadSHA: head}}
				result.FinalMessage = "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
				result.NativeChange = &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Files: 1}
				o.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement, Generation: 7}, Result: result})
				if len(state.Retry) != 0 || len(state.deferredCompletions) != 0 || len(state.Blocked) != 0 || len(state.Claimed) != 0 {
					t.Fatalf("success after retries did not settle: retry=%d deferred=%d blocked=%d claimed=%d", len(state.Retry), len(state.deferredCompletions), len(state.Blocked), len(state.Claimed))
				}
			}
			var updates []string
			for _, update := range tick.updates {
				updates = append(updates, update.state)
			}
			if !slices.Equal(updates, tt.wantUpdates) || tick.stateIssues[0].State != tt.wantState || scheduling.releases != tt.wantReleases || !retryOwned || !tt.succeed && len(tick.comments) != 0 {
				t.Fatalf("lane=%s updates=%v releases=%d retry recorded before release=%v; want lane=%s updates=%v releases=%d", tick.stateIssues[0].State, updates, scheduling.releases, retryOwned, tt.wantState, tt.wantUpdates, tt.wantReleases)
			}
		})
	}
}
