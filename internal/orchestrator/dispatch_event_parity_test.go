package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestEventAndTickDispatchEligibilityParity(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		prepare     func(*Config, *State, *connector.Issue)
		wantRunning bool
	}{
		{name: "ready", wantRunning: true},
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
			prior := dispatchTestIssue("candidate", "Todo")
			prior.Identifier = "digitaldrywood/detent#10"
			current := cloneIssue(prior)
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
					o := Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1), lastDispatchCandidates: []connector.Issue{prior}}
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
