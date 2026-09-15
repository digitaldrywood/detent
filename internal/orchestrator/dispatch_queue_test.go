package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type queuedPullRequestConnector struct {
	hydratingDispatchConnector
	hydrated bool
	input    connector.Issue
}

func (c *queuedPullRequestConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.hydrated = true
	c.input = cloneIssue(issue)
	if issue.PullRequest != nil {
		issue.PullRequest.HeadSHA = "current-head"
	}
	return issue, nil
}

func TestQueuedDispatchPreservesPullRequestEnrichment(t *testing.T) {
	for _, stateName := range []string{"Rework", ""} {
		t.Run(map[string]string{"Rework": "active lane", "": "lane removed"}[stateName], func(t *testing.T) {
			now := time.Now()
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "project"}, MaxConcurrentAgents: 1, ActiveStates: []string{"Rework"}, TerminalStates: []string{"Done"}})
			issue := retryTestIssue("queued", "digitaldrywood/detent#30")
			issue.State = "Rework"
			issue.BranchName = "detent/queued"
			issue.PullRequest = &connector.PullRequest{Number: 30, HeadSHA: "prior-head", State: "open"}
			fresh := cloneIssue(issue)
			fresh.PullRequest = nil
			fresh.BranchName = ""
			tracker := &queuedPullRequestConnector{hydratingDispatchConnector: hydratingDispatchConnector{issue: fresh}}
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
			held, ok, err := gate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: "holder"}, scheduler.SlotRequest{State: "Rework"}, now)
			if err != nil || !ok {
				t.Fatalf("initial grant = %t %v", ok, err)
			}
			o := Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: gate, globalDispatchReady: make(chan struct{}, 1), globalDispatchPending: make(map[string]pendingGlobalDispatch), supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			defer o.releaseRunningSlots(&state)
			defer o.cancelPendingGlobalDispatches()
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(o.globalDispatchPending) != 1 {
				t.Fatalf("request was not queued; decisions: %+v", state.SchedulerDecisions)
			}
			tracker.issue.State = stateName
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			o.dispatchGrantedRequests(t.Context(), &state, now.Add(time.Second))
			if !tracker.hydrated || tracker.input.PullRequest == nil || tracker.input.PullRequest.HeadSHA != "prior-head" || tracker.input.BranchName != issue.BranchName {
				t.Fatalf("PR identity lost before hydration: %+v", tracker.input)
			}
			if tracker.input.State != stateName {
				t.Fatalf("fresh state = %q, want %q", tracker.input.State, stateName)
			}
			running, started := state.Running[issue.ID]
			if started != (stateName != "") {
				t.Fatalf("started = %t for state %q", started, stateName)
			}
			if started && (running.Issue.PullRequest == nil || running.Issue.PullRequest.HeadSHA != "current-head") {
				t.Fatalf("run did not receive fresh PR: %+v", running.Issue)
			}
			if !started && gate.PoolSnapshot().Used != 0 {
				t.Fatal("unused grant leaked")
			}
		})
	}
}

func TestQueuedDispatchPreservesRetry(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "grant", true: "refresh before grant"}[refresh], func(t *testing.T) {
			now := time.Now()
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "higher", Priority: 1}, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := retryTestIssue("retry", "digitaldrywood/detent#20")
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
			held, ok, err := gate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: "holder"}, scheduler.SlotRequest{State: "Todo"}, now)
			if err != nil || !ok {
				t.Fatalf("initial slot = %t %v", ok, err)
			}
			o := Orchestrator{
				cfg: cfg, connector: hydratingDispatchConnector{issue: issue}, globalDispatchGate: gate,
				globalDispatchReady: make(chan struct{}, 1), globalDispatchPending: make(map[string]pendingGlobalDispatch),
				supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1),
			}
			state := newState(cfg)
			defer o.releaseRunningSlots(&state)
			defer o.cancelPendingGlobalDispatches()
			retry := Retry{Issue: issue, Attempt: 7, DueAt: now.Add(-time.Second), Error: "previous failure", RecoveryAttemptID: 42}
			state.Retry[issue.ID] = retry
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(o.globalDispatchPending) != 1 {
				t.Fatalf("queued requests = %d", len(o.globalDispatchPending))
			}
			if got := state.Retry[issue.ID]; got.Attempt != retry.Attempt || got.RecoveryAttemptID != retry.RecoveryAttemptID || !got.DueAt.Equal(retry.DueAt) {
				t.Fatalf("queued retry changed: %+v", got)
			}
			if refresh {
				o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Millisecond))
				if len(o.globalDispatchPending) != 1 {
					t.Fatalf("retry was lost on refresh: %+v", state.Retry)
				}
			}
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			o.dispatchGrantedRequests(t.Context(), &state, now.Add(time.Second))
			if got, running := state.Running[issue.ID]; !running || got.Attempt != retry.Attempt {
				t.Fatalf("retry did not resume with its attempt: %+v", state.Running)
			}
			if _, pending := state.Retry[issue.ID]; pending {
				t.Fatal("running retry retained pending record")
			}
		})
	}
}

func TestQueuedDispatchConsumesImmediateHostAssignment(t *testing.T) {
	for _, hostCapacity := range []int{0, 1} {
		t.Run(map[int]string{0: "uncapped", 1: "host ceiling"}[hostCapacity], func(t *testing.T) {
			now := time.Now()
			cfg := normalizeConfig(Config{
				Project: scheduler.ProjectCandidate{ID: "project"}, MaxConcurrentAgents: 2,
				WorkerHosts: []string{"a", "b"}, MaxConcurrentAgentsPerHost: hostCapacity,
				ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"},
			})
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 2}))
			// A real grant can precede publication in the owner's running state.
			held, ok, err := gate.TryAcquire(t.Context(), cfg.Project, scheduler.SlotRequest{State: "Todo", Host: "a"}, now)
			if err != nil || !ok {
				t.Fatalf("initial grant = %t %v", ok, err)
			}
			defer func() {
				if err := gate.Release(held); err != nil {
					t.Error(err)
				}
			}()
			issue := retryTestIssue("next", "digitaldrywood/detent#30")
			o := Orchestrator{
				cfg: cfg, connector: hydratingDispatchConnector{issue: issue}, globalDispatchGate: gate,
				globalDispatchReady: make(chan struct{}, 1), globalDispatchPending: make(map[string]pendingGlobalDispatch),
				supervisor: newTestSupervisor(t, FakeRunner{}, cfg), runResults: make(chan runpkg.Completion, 1),
			}
			state := newState(cfg)
			defer o.releaseRunningSlots(&state)
			defer o.cancelPendingGlobalDispatches()
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			running, started := state.Running[issue.ID]
			if !started || running.WorkerHost != "b" || running.globalSlot.Host != "b" {
				t.Fatalf("immediate grant host lost: started %t, worker %q, slot %q", started, running.WorkerHost, running.globalSlot.Host)
			}
		})
	}
}

type standingGateWithoutCycleHooks struct {
	scheduler.ProjectDispatchGate
	scheduler.QueuedProjectDispatchGate
}

type standingDispatchConnector struct {
	hydratingDispatchConnector
	fail bool
}

func (c *standingDispatchConnector) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	if c.fail {
		return nil, errors.New("tracker unavailable")
	}
	return c.hydratingDispatchConnector.FetchIssueStatesByIDs(ctx, ids)
}

func TestStandingDispatchSurvivesProjectRefresh(t *testing.T) {
	for _, refresh := range []string{"mid-pass", "same candidate", "ineligible", "removed", "failed hydration", "gate without cycle hooks"} {
		t.Run(refresh, func(t *testing.T) {
			now := time.Now()
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "higher", Priority: 0}, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}})
			issue := retryTestIssue("standing", "digitaldrywood/detent#20")
			tracker := &standingDispatchConnector{hydratingDispatchConnector: hydratingDispatchConnector{issue: issue}}
			gate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
			held, ok, err := gate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: "holder"}, scheduler.SlotRequest{State: "Todo"}, now)
			if err != nil || !ok {
				t.Fatalf("initial slot = %t %v", ok, err)
			}
			o := Orchestrator{cfg: cfg, connector: tracker, globalDispatchGate: gate, globalDispatchReady: make(chan struct{}, 1), globalDispatchPending: make(map[string]pendingGlobalDispatch)}
			state := newState(cfg)
			defer o.cancelPendingGlobalDispatches()
			o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now)
			original, exists := o.globalDispatchPending[issue.ID]
			if !exists {
				t.Fatal("higher request not queued")
			}
			lower, cancel, _ := gate.Submit(t.Context(), scheduler.ProjectCandidate{ID: "lower", Priority: 3}, scheduler.SlotRequest{State: "Todo"}, now.Add(time.Minute), nil)
			defer cancel()
			if refresh == "gate without cycle hooks" {
				o.globalDispatchGate = standingGateWithoutCycleHooks{ProjectDispatchGate: gate, QueuedProjectDispatchGate: gate}
			}
			o.beginGlobalProjectCycle()
			defer o.endGlobalProjectCycle()
			switch refresh {
			case "same candidate":
				tracker.issue.Title = "updated title"
				o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{tracker.issue}, now.Add(2*time.Minute))
				pending := o.globalDispatchPending[issue.ID]
				if pending.result != original.result {
					t.Fatal("refresh recreated the request")
				}
				if pending.action.issue.Title != "updated title" {
					t.Fatal("refresh did not update the action")
				}
			case "removed":
				o.dispatchReadyIssues(t.Context(), &state, nil, now.Add(2*time.Minute))
				if len(o.globalDispatchPending) != 0 {
					t.Fatal("removed candidate retained")
				}
			case "failed hydration":
				tracker.fail = true
				o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(2*time.Minute))
				if pending := o.globalDispatchPending[issue.ID]; pending.result != original.result {
					t.Fatal("failed read discarded standing readiness")
				}
			case "ineligible":
				tracker.issue.State = "Done"
				o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{tracker.issue}, now.Add(2*time.Minute))
				if len(o.globalDispatchPending) != 0 {
					t.Fatal("ineligible request retained")
				}
			}
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			winner, loser := original.result, lower
			if refresh == "ineligible" || refresh == "removed" {
				winner, loser = lower, original.result
			}
			select {
			case grant := <-winner:
				if grant.Err != nil || grant.Slot == (scheduler.Slot{}) {
					t.Fatalf("grant = %+v", grant)
				}
				select {
				case unexpected := <-loser:
					t.Fatalf("loser granted before winner released: %+v", unexpected)
				default:
				}
				cancel()
				o.cancelPendingGlobalDispatches()
				if err := gate.Release(grant.Slot); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("best-ranked standing request did not receive released capacity")
			}
		})
	}
}
