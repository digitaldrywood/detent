package orchestrator_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type observedQueueGate struct {
	scheduler.ProjectDispatchGate
	submitted chan string
}

func (g *observedQueueGate) Submit(ctx context.Context, project scheduler.ProjectCandidate, req scheduler.SlotRequest, now time.Time, wake chan<- struct{}) (<-chan scheduler.DispatchResult, func(), scheduler.DispatchGateDecision) {
	result, cancel, decision := g.ProjectDispatchGate.(scheduler.QueuedProjectDispatchGate).Submit(ctx, project, req, now, wake)
	g.submitted <- project.ID
	return result, cancel, decision
}

func (g *observedQueueGate) Update(result <-chan scheduler.DispatchResult, req scheduler.SlotRequest, now time.Time) {
	g.ProjectDispatchGate.(scheduler.QueuedProjectDispatchGate).Update(result, req, now)
}

func TestRunDispatchesQueuedRequestsWithoutPolling(t *testing.T) {
	for _, scenario := range []string{"priority", "candidate became terminal", "owner shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			registry, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "strict", Capacity: 1}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			gate := &observedQueueGate{ProjectDispatchGate: registry, submitted: make(chan string, 4)}
			held, ok, err := gate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: "running"}, scheduler.SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !ok {
				t.Fatalf("initial slot = %t, %v", ok, err)
			}
			higherIssue := testIssue("higher", "digitaldrywood/detent#10", "Todo")
			lowerIssue := testIssue("lower", "digitaldrywood/detent#11", "Todo")
			higherTracker, lowerTracker := newFakeConnector(higherIssue), newFakeConnector(lowerIssue)
			higherRunner, lowerRunner := newBlockingRunner(), newBlockingRunner()
			start := func(id string, priority int, tracker *fakeConnector, runner *blockingRunner) (*orchestrator.Orchestrator, func()) {
				t.Helper()
				o, err := orchestrator.New(orchestrator.Config{
					Project:      scheduler.ProjectCandidate{ID: id, Priority: priority},
					PollInterval: time.Hour, MaxConcurrentAgents: 1,
					ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"},
				}, orchestrator.Dependencies{Connector: tracker, Runner: runner, GlobalDispatchGate: gate})
				if err != nil {
					t.Fatal(err)
				}
				stop := sync.OnceFunc(runOrchestrator(t, o))
				select {
				case got := <-gate.submitted:
					if got != id {
						t.Fatalf("submitted = %q, want %q", got, id)
					}
				case <-time.After(slowCIIntegrationWaitTimeout):
					t.Fatal("project did not submit eligible request")
				}
				return o, stop
			}
			// Separate submissions and no forced mutex overlap reproduce independent polls.
			higher, stopHigher := start("higher", 1, higherTracker, higherRunner)
			defer stopHigher()
			lower, stopLower := start("lower", 4, lowerTracker, lowerRunner)
			defer stopLower()
			for _, owner := range []*orchestrator.Orchestrator{higher, lower} {
				ctx, cancel := context.WithTimeout(t.Context(), slowCIIntegrationWaitTimeout)
				_, err := owner.State(ctx)
				cancel()
				if err != nil {
					t.Fatalf("pending acquisition blocked owner state requests: %v", err)
				}
			}
			if scenario == "candidate became terminal" {
				if err := higherTracker.UpdateIssueState(t.Context(), higherIssue.ID, "Done"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "owner shutdown" {
				stopHigher()
			}
			if err := gate.Release(held); err != nil {
				t.Fatal(err)
			}
			if scenario == "priority" {
				request := receiveRunRequest(t, higherRunner.started)
				if request.Issue.ID != higherIssue.ID {
					t.Fatalf("first dispatched = %s", request.Issue.ID)
				}
				select {
				case <-lowerRunner.started:
					t.Fatal("lower ran before higher released capacity")
				default:
				}
				close(higherRunner.release)
			}
			request := receiveRunRequest(t, lowerRunner.started)
			if request.Issue.ID != lowerIssue.ID {
				t.Fatalf("next dispatched = %s", request.Issue.ID)
			}
			if scenario != "priority" {
				select {
				case <-higherRunner.started:
					t.Fatal("stale higher request dispatched")
				default:
				}
			}
			if got := lowerTracker.fetchCandidateCalls(); got != 1 {
				t.Fatalf("lower needed another poll: %d", got)
			}
			close(lowerRunner.release)
		})
	}
}

func TestRunDispatchesQueuedRequestsAcrossHostsWithoutPolling(t *testing.T) {
	for _, hostCapacity := range []int{1, 0} {
		t.Run(map[int]string{1: "per-host ceiling", 0: "uncapped hosts"}[hostCapacity], func(t *testing.T) {
			registry, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "strict", Capacity: 2}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			gate := &observedQueueGate{ProjectDispatchGate: registry, submitted: make(chan string, 4)}
			var held []scheduler.Slot
			for range 2 {
				slot, ok, err := gate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: "holder"}, scheduler.SlotRequest{State: "Todo"}, time.Now())
				if err != nil || !ok {
					t.Fatalf("initial slot = %t, %v", ok, err)
				}
				held = append(held, slot)
			}
			tracker := newFakeConnector(testIssue("first", "digitaldrywood/detent#10", "Todo"), testIssue("second", "digitaldrywood/detent#11", "Todo"))
			runner := newBlockingRunner()
			o, err := orchestrator.New(orchestrator.Config{
				Project: scheduler.ProjectCandidate{ID: "project"}, PollInterval: time.Hour,
				MaxConcurrentAgents: 2, MaxConcurrentAgentsPerHost: hostCapacity,
				WorkerHosts:  []string{"host-a", "host-b"},
				ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"},
			}, orchestrator.Dependencies{Connector: tracker, Runner: runner, GlobalDispatchGate: gate})
			if err != nil {
				t.Fatal(err)
			}
			defer runOrchestrator(t, o)()
			for range 2 {
				select {
				case <-gate.submitted:
				case <-time.After(slowCIIntegrationWaitTimeout):
					t.Fatal("candidate did not queue")
				}
			}
			for _, slot := range held {
				if err := gate.Release(slot); err != nil {
					t.Fatal(err)
				}
			}
			hosts := make(map[string]bool)
			issues := make(map[string]bool)
			for range 2 {
				request := receiveRunRequest(t, runner.started)
				hosts[request.WorkerHost] = true
				issues[request.Issue.ID] = true
			}
			if !hosts["host-a"] || !hosts["host-b"] || !issues["first"] || !issues["second"] {
				t.Fatalf("dispatches = hosts %v, issues %v; want both hosts and candidates", hosts, issues)
			}
			if got := tracker.fetchCandidateCalls(); got != 1 {
				t.Fatalf("dispatch required another poll: %d", got)
			}
			close(runner.release)
		})
	}
}
