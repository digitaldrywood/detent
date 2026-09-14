package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestDispatchRecordedPullRequestBlocker(t *testing.T) {
	for _, tt := range []struct {
		name    string
		missing bool
		retry   bool
	}{
		{name: "open PR across ten ticks"},
		{name: "unverifiable reference", missing: true},
		{name: "due retry", retry: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := dispatchTestIssue("2470", "Todo")
			issue.Identifier = "digitaldrywood/detent#2470"
			issue.Fields = map[string]string{"Status": "Todo"}
			issue.DependencySource = connector.BlockedRefSourceNative
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{typedTestBlocker(workpad.Predicate{Type: workpad.PredicatePullRequestState, Identifier: "digitaldrywood/detent#2635", States: []string{"open"}})}}
			blocker := connector.Issue{ID: "2635", Identifier: "digitaldrywood/detent#2635", State: "In Progress", PullRequest: &connector.PullRequest{State: "open"}}
			tracker := &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{hydratedIssues: []connector.Issue{issue}, blockers: []connector.Issue{blocker}}}
			if tt.missing {
				tracker.blockers = nil
			}
			runner := newWorkerHostRunner()
			orch := Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, runner, cfg), runResults: make(chan runpkg.Completion)}
			state := newState(cfg)
			now := time.Now()
			if tt.retry {
				state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 3, DueAt: now.Add(-time.Minute)}
			}
			for tick := range 10 {
				orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Duration(tick)*time.Minute))
				if len(state.Running) != 0 {
					t.Fatalf("tick %d launched a worker while PR blocker remained unresolved", tick)
				}
				if len(state.Blocked) != 0 {
					t.Fatal("dependency wait created a park")
				}
			}
			blocker.PullRequest = &connector.PullRequest{State: "merged"}
			tracker.blockers = []connector.Issue{blocker}
			orch.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(11*time.Minute))
			if len(state.Running) != 1 {
				t.Fatal("cleared PR blocker did not release dispatch")
			}
		})
	}
}

type queuedRecordedBlockerConnector struct {
	*blockerEvidenceTestConnector
	fresh connector.Issue
}

func (c *queuedRecordedBlockerConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return []connector.Issue{c.fresh}, nil
}

func TestQueuedDispatchRechecksRecordedBlockers(t *testing.T) {
	for _, prState := range []string{"open", "merged"} {
		t.Run(prState, func(t *testing.T) {
			now := time.Now()
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "project"}, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := dispatchTestIssue("2470", "Todo")
			issue.Identifier = "digitaldrywood/detent#2470"
			issue.DependencySource = connector.BlockedRefSourceNative
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, Blockers: []workpad.Blocker{typedTestBlocker(workpad.Predicate{Type: workpad.PredicatePullRequestState, Identifier: "digitaldrywood/detent#2635", States: []string{"open"}})}}
			fresh := cloneIssue(issue)
			fresh.WorkpadSignal = nil
			blocker := connector.Issue{ID: "2635", Identifier: "digitaldrywood/detent#2635", PullRequest: &connector.PullRequest{State: "merged"}}
			tracker := &queuedRecordedBlockerConnector{blockerEvidenceTestConnector: &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{hydratedIssues: []connector.Issue{issue}, blockers: []connector.Issue{blocker}}}, fresh: fresh}
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
			held, ok, err := gate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: "holder"}, scheduler.SlotRequest{State: "Todo"}, now)
			if err != nil || !ok {
				t.Fatalf("hold slot: %t %v", ok, err)
			}
			o := Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: gate, globalDispatchReady: make(chan struct{}, 1), globalDispatchPending: make(map[string]pendingGlobalDispatch), supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			defer o.releaseRunningSlots(&state)
			defer o.cancelPendingGlobalDispatches()
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(o.globalDispatchPending) != 1 {
				t.Fatalf("not queued: %+v", state.SchedulerDecisions)
			}
			tracker.blockers[0].PullRequest = &connector.PullRequest{State: prState}
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			o.dispatchGrantedRequests(t.Context(), &state, now.Add(time.Second))
			wantRunning := prState == "merged"
			if (len(state.Running) == 1) != wantRunning {
				t.Fatalf("running = %d for PR %s", len(state.Running), prState)
			}
			if !wantRunning && gate.PoolSnapshot().Used != 0 {
				t.Fatal("blocked grant leaked capacity")
			}
		})
	}
}

type commentRecordedBlockerConnector struct {
	*blockerEvidenceTestConnector
	comments   []connector.IssueComment
	commentErr error
}

func (c *commentRecordedBlockerConnector) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	return c.comments, c.commentErr
}

func TestDispatchLoadsRecordedBlockerComments(t *testing.T) {
	for _, tt := range []struct {
		name      string
		readError bool
	}{
		{name: "Todo comments omitted by state read"},
		{name: "comment read unavailable", readError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := dispatchTestIssue("2470", "Todo")
			issue.Identifier = "digitaldrywood/detent#2470"
			issue.Fields = map[string]string{"Status": "Todo"}
			issue.DependencySource = connector.BlockedRefSourceNative
			blocker := connector.Issue{ID: "2635", Identifier: "digitaldrywood/detent#2635", PullRequest: &connector.PullRequest{State: "open"}}
			tracker := &commentRecordedBlockerConnector{blockerEvidenceTestConnector: &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{blockers: []connector.Issue{blocker}}}, comments: []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: digitaldrywood/detent#2635\n    reason: waiting for PR\n    owner: orchestrator\n    predicate:\n      type: pull_request_state\n      states: [open]\n    recheck_interval: tick\nhuman_action: null\n```"}}}
			if tt.readError {
				tracker.commentErr = errors.New("comment service unavailable")
			}
			o := Orchestrator{cfg: cfg, connector: tracker, supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			now := time.Now()
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(state.Running) != 0 {
				t.Fatal("dispatched without clear Workpad evidence")
			}
			tracker.commentErr = nil
			tracker.blockers[0].PullRequest = &connector.PullRequest{State: "merged"}
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Minute))
			if len(state.Running) != 1 {
				t.Fatal("cleared comments did not release dispatch")
			}
		})
	}
}
