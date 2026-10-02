package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestQueuedCompletionsReleaseCapacityBeforeRefill(t *testing.T) {
	for _, tt := range []struct {
		name    string
		stopped bool
		fenced  bool
	}{
		{name: "completed"},
		{name: "multiple operator stops", stopped: true},
		{name: "completion fence", fenced: true},
		{name: "stops under completion fence", stopped: true, fenced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 2, Project: scheduler.ProjectCandidate{ID: "fixture", Weight: 1}, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 2}), cfg.Project)
			state := newState(cfg)
			first := dispatchTestIssue("first", "Todo")
			second := dispatchTestIssue("second", "Todo")
			next := dispatchTestIssue("next", "Todo")
			runner := newWorkerHostRunner()
			o := Orchestrator{done: make(chan struct{}), cfg: cfg, globalDispatchGate: gate, supervisor: newTestSupervisor(t, runner, cfg), runResults: make(chan runpkg.Completion, 3), pendingStops: map[string]*pendingStopRun{}, completedStops: map[string]StopRunResult{}, now: func() time.Time { return now }}
			defer o.releaseRunningSlots(&state)
			for _, issue := range []connector.Issue{first, second} {
				slot, acquired, _, err := gate.TryAcquireWithDecision(t.Context(), cfg.Project, scheduler.SlotRequest{State: "Todo"}, now)
				if err != nil || !acquired {
					t.Fatalf("acquire fixture slot: acquired=%t error=%v", acquired, err)
				}
				state.Running[issue.ID] = Running{Issue: issue, StartedAt: now, globalSlot: slot}
				state.Claimed[issue.ID] = Claimed{Issue: issue}
				if tt.stopped {
					o.pendingStops[issue.ID] = &pendingStopRun{reapDone: true, result: StopRunResult{ProjectID: cfg.Project.ID, IssueID: issue.ID, Destination: "Todo"}}
				}
			}
			o.publishState(&state)
			if tt.fenced {
				o.startCompletion(&state)
			}
			entered := make(chan struct{})
			unblock := make(chan struct{})
			fetches := 0
			candidates := []connector.Issue{next}
			if tt.stopped {
				candidates = []connector.Issue{first, second, next}
			}
			o.connector = completionRefillConnector{hydratingDispatchConnector: hydratingDispatchConnector{issue: next}, fetch: func(ctx context.Context) ([]connector.Issue, error) {
				fetches++
				if len(state.Running) != 0 || gate.PoolSnapshot().Used != 0 {
					t.Errorf("refill began before both completions released: running=%d pool=%#v", len(state.Running), gate.PoolSnapshot())
				}
				close(entered)
				select {
				case <-unblock:
					return candidates, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			result := func(issue connector.Issue) runpkg.Completion {
				return runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted}}
			}
			o.runResults <- result(second)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				o.handleQueuedRunResults(t.Context(), &state, result(first))
			}()
			release := sync.OnceFunc(func() { close(unblock) })
			defer func() { release(); <-finished }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("completion cohort did not reach refill")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			observed, err := o.State(ctx)
			cancel()
			if err != nil {
				t.Fatalf("State() during completion refill: %v", err)
			}
			wantOwned := 0
			if tt.fenced {
				wantOwned = 2
			}
			if len(observed.Running) != 0 || len(observed.Claimed) != wantOwned {
				t.Fatalf("ownership during refill: running=%d claimed=%d, want running=0 claimed=%d", len(observed.Running), len(observed.Claimed), wantOwned)
			}
			if o.refreshProgress.Load() != nil || observed.RefreshProgress.Stage != "" {
				t.Fatal("completion refill started a tracker refresh")
			}
			if !tt.fenced && !observed.RuntimeObservation.IsZero() {
				t.Fatal("unfenced refill acquired a completion observation")
			}
			release()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("completion cohort did not finish")
			}
			if fetches != 1 {
				t.Fatalf("fresh candidate reads=%d, want one", fetches)
			}
			request := receiveWorkerHostRunRequest(t, runner.started)
			if request.Issue.ID != next.ID {
				t.Fatalf("refilled %q, want %q; stopped issues must stay excluded", request.Issue.ID, next.ID)
			}
		})
	}
}

func TestQueuedCompletionKeepsRejectedGenerationOwnership(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
	issue := dispatchTestIssue("current", "Todo")
	state := newState(cfg)
	state.Running[issue.ID] = Running{Issue: issue, Generation: 2, StartedAt: now}
	fetches := 0
	tracker := completionRefillConnector{fetch: func(context.Context) ([]connector.Issue, error) { fetches++; return nil, nil }}
	o := Orchestrator{cfg: cfg, connector: tracker, runResults: make(chan runpkg.Completion, 1), now: func() time.Time { return now }}
	o.handleQueuedRunResults(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, Request: runpkg.RunRequest{Generation: 1}, CompletedAt: now})
	if fetches != 0 || len(state.Running) != 1 {
		t.Fatalf("nonreleased result refilled or lost ownership: reads=%d running=%d", fetches, len(state.Running))
	}
}

type completionRefillConnector struct {
	hydratingDispatchConnector
	fetch func(context.Context) ([]connector.Issue, error)
}

func (c completionRefillConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	return c.fetch(ctx)
}

func TestEventAndTickDispatchEligibilityParity(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		prepare     func(*Config, *State, *connector.Issue)
		wantRunning bool
	}{
		{name: "fresh Todo without prior snapshot", wantRunning: true},
		{name: "newly recovered Rework", prepare: func(cfg *Config, _ *State, issue *connector.Issue) {
			cfg.AutoPromote.ReworkState = "Rework"
			issue.State = "Rework"
		}, wantRunning: true},
		{name: "lane changed", prepare: func(_ *Config, _ *State, issue *connector.Issue) { issue.State = "Done" }},
		{name: "dependency changed", prepare: func(_ *Config, _ *State, issue *connector.Issue) {
			issue.DependencySource = connector.BlockedRefSourceNative
			issue.BlockedBy = []connector.BlockedRef{{ID: "blocked", Identifier: "digitaldrywood/detent#99", State: "In Progress"}}
		}},
		{name: "authorization", prepare: func(cfg *Config, _ *State, _ *connector.Issue) {
			cfg.Authorization = selector.Selector{Labels: selector.Labels{Include: []string{"authorized"}}}
		}},
		{name: "claim lease", prepare: func(_ *Config, state *State, issue *connector.Issue) {
			state.Claimed[issue.ID] = Claimed{Issue: *issue}
		}},
		{name: "retry deferral", prepare: func(_ *Config, state *State, issue *connector.Issue) {
			state.Retry[issue.ID] = Retry{Issue: *issue, DueAt: now.Add(time.Hour)}
		}},
		{name: "current head checks", prepare: func(cfg *Config, _ *State, issue *connector.Issue) {
			cfg.AutoPromote.ReworkState = "Rework"
			issue.State = "Rework"
			issue.PullRequest = &connector.PullRequest{Number: 10, State: "open", CIStatus: "pending"}
		}},
		{name: "lifetime hold", prepare: func(_ *Config, state *State, issue *connector.Issue) {
			state.Blocked[issue.ID] = Blocked{Issue: *issue, Reason: lifetimeLimitReason, Source: BlockedSourceProjectStatus}
		}},
		{name: "recovery park", prepare: func(_ *Config, state *State, issue *connector.Issue) {
			state.Blocked[issue.ID] = Blocked{Issue: *issue, Reason: "recovery pending", Source: BlockedSourceProjectStatus}
		}},
		{name: "failure breaker", prepare: func(_ *Config, state *State, _ *connector.Issue) {
			state.FailureBreaker.Class = "runner_error"
			state.FailureBreaker.ResumeAt = now.Add(time.Hour)
		}},
		{name: "instance tracker outage", prepare: func(_ *Config, state *State, _ *connector.Issue) {
			state.TrackerUnavailable = &TrackerCondition{}
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "Rework"}, TerminalStates: []string{"Done"}})
			current := dispatchTestIssue("newly-ready", "Todo")
			current.Identifier = "digitaldrywood/detent#10"
			state := newState(cfg)
			if tt.prepare != nil {
				tt.prepare(&cfg, &state, &current)
			}
			state.MaxConcurrentAgents = cfg.MaxConcurrentAgents
			for _, event := range []bool{false, true} {
				name := "tick"
				if event {
					name = "event"
				}
				t.Run(name, func(t *testing.T) {
					tracker := hydratingDispatchConnector{issue: current}
					o := Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
					copy := state.clone()
					defer o.releaseRunningSlots(&copy)
					if event {
						o.refillProjectSlots(t.Context(), &copy, now)
					} else {
						o.dispatchReadyIssues(t.Context(), &copy, []connector.Issue{current}, now)
					}
					if got := len(copy.Running) == 1; got != tt.wantRunning {
						t.Fatalf("running = %t, want %t", got, tt.wantRunning)
					}
					if event && tt.wantRunning {
						generation := copy.Running[current.ID].Generation
						o.dispatchReadyIssues(t.Context(), &copy, []connector.Issue{current}, now.Add(time.Second))
						if len(copy.Running) != 1 || copy.Running[current.ID].Generation != generation {
							t.Fatal("tick after event dispatched the same issue twice")
						}
					}
				})
			}
		})
	}
}
