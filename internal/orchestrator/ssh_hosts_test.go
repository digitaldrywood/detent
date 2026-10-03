package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestSSHQueuedPreferenceAndHostCaps(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "fill preferred", true: "spill at cap"}[full], func(t *testing.T) {
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "project"}, MaxConcurrentAgents: 3, WorkerHosts: []string{"air", "studio", "local"}, WorkerHostSelection: "preference", WorkerHostCaps: map[string]int{"air": 1, "studio": 2, "local": 1}, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := retryTestIssue("next", "digitaldrywood/detent#30")
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 3}))
			o := Orchestrator{cfg: cfg, connector: hydratingDispatchConnector{issue: issue}, globalDispatchGate: gate, globalDispatchReady: make(chan struct{}, 1), globalDispatchPending: make(map[string]pendingGlobalDispatch), supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			defer o.releaseRunningSlots(&state)
			defer o.cancelPendingGlobalDispatches()
			want := "air"
			if full {
				prior := retryTestIssue("prior", "digitaldrywood/detent#29")
				state.Running[prior.ID] = Running{Issue: prior, WorkerHost: "air"}
				want = "studio"
			}
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
			running, started := state.Running[issue.ID]
			if !started || running.WorkerHost != want || running.globalSlot.Host != want {
				t.Fatalf("dispatch = %+v, started %v; want %s", running, started, want)
			}
		})
	}
}

func TestSSHHostPreferenceAndCaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, selection, preferred string
		used                       map[string]int
		down                       string
		want                       string
		ok                         bool
	}{
		{"fill first", "preference", "", map[string]int{"air": 1}, "", "air", true},
		{"spill second", "preference", "", map[string]int{"air": 2, "studio": 1}, "", "studio", true},
		{"spill local", "preference", "", map[string]int{"air": 2, "studio": 3}, "", "local", true},
		{"all full", "preference", "", map[string]int{"air": 2, "studio": 3, "local": 1}, "", "", false},
		{"least loaded default", "", "", map[string]int{"air": 1}, "", "studio", true},
		{"least loaded retry affinity", "least_loaded", "air", map[string]int{"air": 1}, "", "air", true},
		{"preference beats retry affinity", "preference", "studio", nil, "", "air", true},
		{"skip unreachable", "preference", "air", nil, "air", "studio", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{WorkerHosts: []string{"air", "studio", "local"}, WorkerHostSelection: test.selection, WorkerHostCaps: map[string]int{"air": 2, "studio": 3}, MaxConcurrentAgentsPerHost: 1}
			state := newState(cfg)
			for host, count := range test.used {
				for i := range count {
					issue := dispatchTestIssue(host+string(rune('0'+i)), "Todo")
					state.Running[issue.ID] = Running{Issue: issue, WorkerHost: host}
				}
			}
			planner := newDispatchPlanner(cfg)
			planner.workerHostAvailable = func(host string) bool { return host != test.down }
			got, ok := planner.selectWorkerHost(&state, test.preferred)
			if got != test.want || ok != test.ok {
				t.Fatalf("selection = %q, %v; want %q, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestSSHHostLossClearsResumeOnSpillover(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic fresh fallback", true: "native preserves resume intent"}[native], func(t *testing.T) {
			cfg := Config{WorkerHosts: []string{"air", "local"}, WorkerHostSelection: "preference"}
			state := newState(cfg)
			planner := newDispatchPlanner(cfg)
			planner.nativeWorkflow = native
			planner.workerHostAvailable = func(host string) bool { return host == "local" }
			retry := Retry{RecoveryAttemptID: 42, WorkerHost: "air", RetryMode: runpkg.RetryModeResume, ResumeState: store.AgentResumeState{ProviderThreadID: "remote-thread"}}
			action, ok := planner.newDispatchAction(&state, dispatchTestIssue("issue", "Todo"), 1, "air", true, true, &retry)
			wantMode, wantThread := runpkg.RetryModeFresh, ""
			if native {
				wantMode, wantThread = runpkg.RetryModeResume, "remote-thread"
			}
			if !ok || action.workerHost != "local" || action.retryState.RetryMode != wantMode || action.retryState.ResumeState.ProviderThreadID != wantThread || action.retryState.RecoveryAttemptID != retry.RecoveryAttemptID {
				t.Fatalf("spillover changed requested continuation: %+v", action)
			}
			if retry.ResumeState.ProviderThreadID != "remote-thread" {
				t.Fatal("selection mutated persisted retry")
			}
		})
	}
}

func TestSSHHostLossUsesInstanceRetry(t *testing.T) {
	t.Parallel()
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "before turn", true: "in flight"}[started], func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
			issue := connector.Issue{ID: "lost", Identifier: "owner/repo#1", State: "In Progress"}
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
			orch := Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: 2}
			state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: 2}
			state.Running[issue.ID] = Running{Issue: issue, WorkerHost: "air", Mode: runpkg.RunModeImplement, Attempt: 1, StartedAt: time.Now()}
			err := backendcapacity.NewError(backendcapacity.Scope{BackendID: "ssh:air", BackendKind: "ssh", Provider: "local"}, backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload}, errors.New("SSH channel closed"))
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Err: err, Result: runpkg.RunResult{TurnStarted: started}, CompletedAt: time.Now(), Retryable: true, RetryAttempt: 1, RetryDelay: time.Second})
			if _, ok := state.Retry[issue.ID]; !ok {
				t.Fatal("host loss did not schedule retry")
			}
			if state.FailureBreaker.Active() || state.InstantFailures[issue.ID].Count != 2 || state.RepeatedFailures[issue.ID].Count != 2 {
				t.Fatal("host loss charged issue failures or drained the instance")
			}
		})
	}
}
