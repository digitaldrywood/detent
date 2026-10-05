package orchestrator_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type intakeBarrierHandler struct {
	slog.Handler
	applied chan struct{}
	once    *sync.Once
}

func (h intakeBarrierHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "local intake policy applied" {
		h.once.Do(func() { close(h.applied) })
	}
	return h.Handler.Handle(ctx, record)
}

func TestLocalIntakeStopsRacingDispatchAndResumes(t *testing.T) {
	t.Parallel()
	issue := testIssue("pending-local", "local#1", "Merging")
	issue.PullRequest = &connector.PullRequest{Number: 1, State: "OPEN", MergeableState: "clean", CIStatus: "success"}
	issue.PRRepository = "example/local"
	tracker := &pendingDispatchConnector{fakeConnector: newFakeConnector(issue), started: make(chan struct{}), release: make(chan struct{})}
	tracker.stateIssues = []connector.Issue{issue}
	runner := newBlockingRunner()
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 2}))
	cfg := orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local", Weight: 1}, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Merging"}, ObservedStates: []string{"Merging"}, TerminalStates: []string{"Done"}, MergeFastPathEnabled: true}
	applied := make(chan struct{})
	orch, err := orchestrator.New(cfg, orchestrator.Dependencies{Connector: tracker, Runner: runner, GlobalDispatchGate: gate, Logger: slog.New(intakeBarrierHandler{Handler: slog.NewTextHandler(io.Discard, nil), applied: applied, once: &sync.Once{}})})
	if err != nil {
		t.Fatal(err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()
	select {
	case <-tracker.started:
	case <-time.After(time.Second):
		t.Fatal("candidate preparation barrier not reached")
	}
	cfg.LocalIntakeDisabled = true
	updated := make(chan error, 1)
	go func() { updated <- orch.UpdateConfig(t.Context(), cfg) }()
	select {
	case <-applied:
	case <-time.After(time.Second):
		t.Fatal("intake policy blocked behind the dispatch tick")
	}
	close(tracker.release)
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("config update did not finish")
	}
	state, err := orch.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Draining || len(state.Running) != 0 || len(state.Claimed) != 0 || state.LocalIntake.Enabled {
		t.Fatalf("intake-off state = %+v", state.LocalIntake)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("racing first admission: %s", request.Issue.ID)
	default:
	}
	slot, acquired, _, err := gate.TryAcquireWithDecision(t.Context(), scheduler.ProjectCandidate{ID: "cloud-runner", Weight: 1}, scheduler.SlotRequest{State: "Todo"}, time.Now())
	if err != nil || !acquired {
		t.Fatalf("local intake disabled Cloud capacity: acquired=%t err=%v", acquired, err)
	}
	if err := gate.Release(slot); err != nil {
		t.Fatal(err)
	}
	cfg.LocalIntakeDisabled = false
	if err := orch.UpdateConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.RequestRefresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := receiveRunRequest(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("resumed admission = %s", request.Issue.ID)
	}
}
