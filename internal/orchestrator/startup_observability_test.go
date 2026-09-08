package orchestrator_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStartupRefreshKeepsStateAndWorkerProgressObservable(t *testing.T) {
	for _, cancelRefresh := range []bool{false, true} {
		name := "complete refresh"
		if cancelRefresh {
			name = "cancel refresh"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				issue := testIssue("startup-worker", "digitaldrywood/detent#2270", "Todo")
				tracker := &pendingDispatchConnector{fakeConnector: newFakeConnector(issue, testIssue("startup-worker-2", "digitaldrywood/detent#2271", "Todo")), started: make(chan struct{}), release: make(chan struct{})}
				reaper := &startupStalledReaper{started: make(chan struct{}), release: make(chan struct{})}
				runner := newBlockingRunner()
				orch, err := orchestrator.New(orchestrator.Config{
					PollInterval: time.Hour, MaxConcurrentAgents: 2,
					ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"},
				}, orchestrator.Dependencies{Connector: tracker, Runner: runner, WorkspaceReaper: reaper})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- orch.Run(ctx) }()
				defer func() { cancel(); <-done }()
				<-tracker.started
				state := startupState(t, orch)
				if got := state.Snapshot(time.Now()).Refresh; !got.Initializing() || got.LastRefreshAt != nil {
					t.Fatalf("initial refresh = %#v", got)
				}
				initialProgress := state.Snapshot(time.Now().Add(2 * time.Minute)).Refresh.InFlight
				if initialProgress == nil || initialProgress.Stage != "tracker_fetch" || initialProgress.ElapsedSeconds != 120 || initialProgress.StageElapsedSeconds != 120 {
					t.Fatalf("initial progress = %#v", initialProgress)
				}
				close(tracker.release)
				requests := []orchestrator.RunRequest{<-runner.started, <-runner.started}
				<-reaper.started
				for _, request := range requests {
					for _, message := range []string{"workspace created", "tests running"} {
						err := request.OnUsageUpdate(runpkg.UsageUpdate{
							SessionID: request.Issue.ID + "-session", TurnCount: 1, LastEventAt: time.Now(), LastMessage: message,
							DispatchLoopStart: &runpkg.DispatchLoopStartSnapshot{WorkspaceDiffAvailable: true},
						})
						if err != nil {
							t.Fatal(err)
						}
						state = startupState(t, orch)
						progress := state.Snapshot(time.Now().Add(time.Minute)).Refresh.InFlight
						if progress == nil || progress.Stage != "workspace_cleanup" || progress.StageElapsedSeconds != 60 {
							t.Fatalf("cleanup progress = %#v", progress)
						}
						if got := state.Running[request.Issue.ID]; got.LastMessage != message || got.SessionID != request.Issue.ID+"-session" {
							t.Fatalf("running worker = %#v, want progress %q during stalled cleanup", got, message)
						}
						if got := state.Snapshot(time.Now()).Refresh; got.ReadinessStatus() != telemetry.RefreshStatusInitializing || got.LastRefreshAt != nil {
							t.Fatalf("in-flight refresh = %#v, want initializing", got)
						}
					}
				}
				if cancelRefresh {
					cancel()
				} else {
					close(reaper.release)
					synctest.Wait()
					if got := startupState(t, orch).Snapshot(time.Now()).Refresh; !got.Ready() || got.InFlight != nil {
						t.Fatalf("completed refresh = %#v", got)
					}
				}
			})
		})
	}
}

func startupState(t *testing.T, orch *orchestrator.Orchestrator) orchestrator.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	state, err := orch.State(ctx)
	if err != nil {
		t.Fatalf("State() during stalled startup: %v", err)
	}
	return state
}

type startupStalledReaper struct {
	started chan struct{}
	release chan struct{}
}

func (r *startupStalledReaper) ReapWorkspace(context.Context, connector.Issue) (orchestrator.WorkspaceReapResult, error) {
	return orchestrator.WorkspaceReapResult{}, nil
}

func (r *startupStalledReaper) ReconcileWorkspaces(ctx context.Context, _ []connector.Issue) (orchestrator.WorkspaceReconcileResult, error) {
	close(r.started)
	select {
	case <-ctx.Done():
		return orchestrator.WorkspaceReconcileResult{}, ctx.Err()
	case <-r.release:
		return orchestrator.WorkspaceReconcileResult{}, nil
	}
}

func TestCompletionFenceKeepsStateObservable(t *testing.T) {
	for _, cancelCompletion := range []bool{false, true} {
		name := "complete fence"
		if cancelCompletion {
			name = "cancel fence"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				issue := testIssue("completion-worker", "digitaldrywood/detent#2361", "In Progress")
				tracker := &completionStalledConnector{fakeConnector: newFakeConnector(issue), armed: make(chan struct{}), started: make(chan struct{}), release: make(chan struct{})}
				runner := newBlockingRunner()
				orch, err := orchestrator.New(orchestrator.Config{
					PollInterval: time.Hour, MaxConcurrentAgents: 1,
					ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"},
				}, orchestrator.Dependencies{Connector: tracker, Runner: runner})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- orch.Run(ctx) }()
				defer func() { cancel(); <-done }()
				request := <-runner.started
				synctest.Wait()
				if request.Generation == 0 {
					t.Fatal("worker lacks completion fence generation")
				}
				close(tracker.armed)
				close(runner.release)
				<-tracker.started
				state := startupState(t, orch)
				if state.RefreshProgress.Stage != "" {
					t.Fatalf("completion unexpectedly inside refresh: %#v", state.RefreshProgress)
				}
				if state.Running[issue.ID].Generation != request.Generation || state.Claimed[issue.ID].Issue.ID != issue.ID {
					t.Fatal("pending completion lost worker ownership")
				}
				observed := state.Snapshot(time.Now()).Runtime
				if observed.Source != telemetry.SnapshotSourceCached || observed.ObservedAt.IsZero() || !observed.Complete {
					t.Fatalf("pending runtime observation = %#v", observed)
				}
				time.Sleep(time.Second)
				delete(state.Running, issue.ID)
				delete(state.Claimed, issue.ID)
				state = startupState(t, orch)
				if len(state.Running) != 1 || len(state.Claimed) != 1 {
					t.Fatal("reader mutated published ownership")
				}
				if got := state.Snapshot(time.Now()).Runtime; got != observed {
					t.Fatalf("cached runtime freshness advanced: %#v, want %#v", got, observed)
				}
				readCtx, cancelRead := context.WithCancel(t.Context())
				cancelRead()
				if _, err := orch.State(readCtx); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled State() = %v", err)
				}
				if cancelCompletion {
					cancel()
					synctest.Wait()
					if _, err := orch.State(t.Context()); !errors.Is(err, orchestrator.ErrStopped) {
						t.Fatalf("stopped State() = %v", err)
					}
				} else {
					close(tracker.release)
					synctest.Wait()
					state = startupState(t, orch)
					if len(state.Running) != 0 {
						t.Fatal("completed worker remains running")
					}
					if !state.RuntimeObservation.IsZero() {
						t.Fatalf("completed fence retained cached observation: %#v", state.RuntimeObservation)
					}
				}
			})
		})
	}
}

type completionStalledConnector struct {
	*fakeConnector
	armed   chan struct{}
	started chan struct{}
	release chan struct{}
}

func (c *completionStalledConnector) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	select {
	case <-c.armed:
		select {
		case <-c.started:
		default:
			close(c.started)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.release:
		}
	default:
	}
	return c.fakeConnector.FetchIssueStatesByIDs(ctx, ids)
}
