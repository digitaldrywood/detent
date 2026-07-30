package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerStopsSessionBeyondMaxTurns(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 30, 14, 0, 0, 0, time.UTC)
	backend := &turnCountingAgentBackend{updates: []AgentUpdate{
		{Type: AgentUpdateTurnStarted, ThreadID: "thread-1572", TurnID: "turn-1"},
		{Type: AgentUpdateTurnStarted, ThreadID: "thread-1572", TurnID: "turn-2"},
		{Type: AgentUpdateTurnStarted, ThreadID: "thread-1572", TurnID: "turn-3"},
		{Type: AgentUpdateTurnStarted, ThreadID: "thread-1572", TurnID: "turn-4"},
	}}
	sessionStore := &fakeSessionStore{sessionID: 1572}
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-turn-limit"},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{Agent: config.Agent{MaxTurns: 2}},
			Prompt: "Work",
		},
		Workspace:    workspaceBackend,
		AgentBackend: backend,
		Store:        sessionStore,
		Now:          func() time.Time { return startedAt.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, err := runner.Run(t.Context(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-turn-limit",
			Identifier: "digitaldrywood/detent#1572",
		},
		StartedAt: startedAt,
	})
	if !errors.Is(err, ErrSessionTurnLimitExceeded) {
		t.Fatalf("Run() error = %v, want ErrSessionTurnLimitExceeded", err)
	}
	var brake *SessionBrakeError
	if !errors.As(err, &brake) {
		t.Fatalf("Run() error = %T, want SessionBrakeError", err)
	}
	if brake.Reason != SessionBrakeReasonTurnLimit || brake.Turns != 3 || brake.MaxTurns != 2 {
		t.Fatalf("session brake = %#v, want turn 3 beyond limit 2", brake)
	}
	if brake.CauseFingerprint == "" {
		t.Fatal("session brake cause fingerprint is empty")
	}
	if backend.updatesHandled != 3 {
		t.Fatalf("backend updates handled = %d, want 3", backend.updatesHandled)
	}
	if result.FinalState != FinalStateTurnLimitExceeded {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateTurnLimitExceeded)
	}
	if sessionStore.finishCalls != 1 {
		t.Fatalf("FinishSession() calls = %d, want 1", sessionStore.finishCalls)
	}
	if !workspaceBackend.afterRun {
		t.Fatal("AfterRun() was not called")
	}
}

func TestRunnerNormalizesProviderTurnLimitBreach(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 30, 14, 30, 0, 0, time.UTC)
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{Agent: config.Agent{MaxTurns: 20}},
			Prompt: "Work",
		},
		Workspace: &fakeWorkspaceBackend{
			info: workspace.Info{Path: t.TempDir(), Key: "issue-provider-turn-limit"},
		},
		AgentBackend: providerTurnLimitAgentBackend{},
		Store:        &fakeSessionStore{sessionID: 1575},
		Now:          func() time.Time { return startedAt.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	result, runErr := runner.Run(t.Context(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-provider-turn-limit",
			Identifier: "digitaldrywood/detent#1572",
		},
		StartedAt: startedAt,
	})
	var brake *SessionBrakeError
	if !errors.As(runErr, &brake) {
		t.Fatalf("Run() error = %v, want SessionBrakeError", runErr)
	}
	if brake.Reason != SessionBrakeReasonTurnLimit || brake.Turns != 20 || brake.MaxTurns != 20 {
		t.Fatalf("session brake = %#v, want provider breach at limit 20", brake)
	}
	if result.FinalState != FinalStateTurnLimitExceeded {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateTurnLimitExceeded)
	}
}

func TestRunnerStopsSessionAfterNoProgressBeforeCompletion(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 30, 15, 0, 0, 0, time.UTC)
	timeout := 25 * time.Second
	tickerFactory := newControlledSessionTickerFactory()
	probeCalls := make(chan struct{}, 4)
	backend := &sessionBlockingAgentBackend{
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	sessionStore := &fakeSessionStore{sessionID: 1573}
	workspaceBackend := &fakeWorkspaceBackend{
		info: workspace.Info{Path: t.TempDir(), Key: "issue-no-progress"},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{Agent: config.Agent{
				MaxTurns:            20,
				NoProgressTimeoutMS: int(timeout / time.Millisecond),
			}},
			Prompt: "Work",
		},
		Workspace:      workspaceBackend,
		AgentBackend:   backend,
		Store:          sessionStore,
		Now:            func() time.Time { return startedAt },
		progressTicker: tickerFactory.New,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	resultCh := make(chan RunResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, runErr := runner.Run(t.Context(), RunRequest{
			Issue: connector.Issue{
				ID:         "issue-no-progress",
				Identifier: "digitaldrywood/detent#1572",
			},
			StartedAt: startedAt,
			ProgressProbe: func(context.Context) (string, error) {
				probeCalls <- struct{}{}
				return "unchanged-workpad", nil
			},
		})
		resultCh <- result
		errCh <- runErr
	}()

	waitSessionSignal(t, probeCalls, "initial progress probe")
	waitSessionSignal(t, backend.started, "agent backend start")
	ticker := tickerFactory.Wait(t)
	ticker.Tick(startedAt.Add(timeout))
	waitSessionSignal(t, probeCalls, "expiration progress probe")
	waitSessionSignal(t, backend.stopped, "agent backend cancellation")

	var result RunResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for no-progress result")
	}
	var runErr error
	select {
	case runErr = <-errCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for no-progress error")
	}
	if !errors.Is(runErr, ErrSessionNoProgress) {
		t.Fatalf("Run() error = %v, want ErrSessionNoProgress", runErr)
	}
	var brake *SessionBrakeError
	if !errors.As(runErr, &brake) {
		t.Fatalf("Run() error = %T, want SessionBrakeError", runErr)
	}
	if brake.Elapsed != timeout || brake.Turns != 1 || brake.Reason != SessionBrakeReasonNoProgress {
		t.Fatalf("session brake = %#v, want %s no-progress breach with one turn", brake, timeout)
	}
	if result.FinalState != FinalStateNoProgress {
		t.Fatalf("FinalState = %q, want %q", result.FinalState, FinalStateNoProgress)
	}
	if sessionStore.finishCalls != 1 {
		t.Fatalf("FinishSession() calls = %d, want 1", sessionStore.finishCalls)
	}
	if !workspaceBackend.afterRun {
		t.Fatal("AfterRun() was not called")
	}
}

func TestRunnerWorkpadProgressResetsNoProgressHeartbeat(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)
	timeout := 25 * time.Second
	tickerFactory := newControlledSessionTickerFactory()
	probeCalls := make(chan struct{}, 4)
	release := make(chan struct{})
	backend := &sessionBlockingAgentBackend{
		started: make(chan struct{}),
		stopped: make(chan struct{}),
		release: release,
	}
	var workpad atomic.Value
	workpad.Store("initial-workpad")
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{Agent: config.Agent{
				MaxTurns:            20,
				NoProgressTimeoutMS: int(timeout / time.Millisecond),
			}},
			Prompt: "Work",
		},
		Workspace: &fakeWorkspaceBackend{
			info: workspace.Info{Path: t.TempDir(), Key: "issue-workpad-progress"},
		},
		AgentBackend:   backend,
		Store:          &fakeSessionStore{sessionID: 1574},
		Now:            func() time.Time { return startedAt },
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		progressTicker: tickerFactory.New,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, runErr := runner.Run(t.Context(), RunRequest{
			Issue: connector.Issue{
				ID:         "issue-workpad-progress",
				Identifier: "digitaldrywood/detent#1572",
			},
			StartedAt: startedAt,
			ProgressProbe: func(context.Context) (string, error) {
				probeCalls <- struct{}{}
				return workpad.Load().(string), nil
			},
		})
		errCh <- runErr
	}()

	waitSessionSignal(t, probeCalls, "initial progress probe")
	waitSessionSignal(t, backend.started, "agent backend start")
	ticker := tickerFactory.Wait(t)
	workpad.Store("updated-workpad")
	ticker.Tick(startedAt.Add(timeout))
	waitSessionSignal(t, probeCalls, "updated progress probe")
	close(release)

	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("Run() error = %v, want normal completion", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for normal completion")
	}
}

type turnCountingAgentBackend struct {
	updates        []AgentUpdate
	updatesHandled int
}

func (b *turnCountingAgentBackend) RunTurn(_ context.Context, _ AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	for _, update := range b.updates {
		err := onUpdate(update)
		b.updatesHandled++
		if err != nil {
			return AgentTurnResult{}, err
		}
	}
	return AgentTurnResult{ThreadID: "thread-1572", TurnID: "turn-4", SessionID: "thread-1572-turn-4"}, nil
}

type sessionBlockingAgentBackend struct {
	started chan struct{}
	stopped chan struct{}
	release <-chan struct{}
}

func (b *sessionBlockingAgentBackend) RunTurn(ctx context.Context, _ AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	if err := onUpdate(AgentUpdate{
		Type:     AgentUpdateTurnStarted,
		ThreadID: "thread-1572",
		TurnID:   "turn-1",
	}); err != nil {
		return AgentTurnResult{}, err
	}
	close(b.started)
	if b.release != nil {
		select {
		case <-ctx.Done():
			close(b.stopped)
			return AgentTurnResult{}, ctx.Err()
		case <-b.release:
			return AgentTurnResult{ThreadID: "thread-1572", TurnID: "turn-1", SessionID: "thread-1572-turn-1"}, nil
		}
	}
	<-ctx.Done()
	close(b.stopped)
	return AgentTurnResult{}, ctx.Err()
}

type providerTurnLimitAgentBackend struct{}

func (providerTurnLimitAgentBackend) RunTurn(_ context.Context, req AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	if req.MaxTurns != 20 {
		return AgentTurnResult{}, errors.New("max turns not propagated")
	}
	if err := onUpdate(AgentUpdate{
		Type:     AgentUpdateTurnStarted,
		ThreadID: "thread-provider-limit",
		TurnID:   "turn-provider-limit",
	}); err != nil {
		return AgentTurnResult{}, err
	}
	return AgentTurnResult{
		ThreadID:  "thread-provider-limit",
		TurnID:    "turn-provider-limit",
		SessionID: "thread-provider-limit-turn-provider-limit",
	}, ErrSessionTurnLimitExceeded
}

type controlledSessionTickerFactory struct {
	created chan *controlledSessionTicker
}

func newControlledSessionTickerFactory() *controlledSessionTickerFactory {
	return &controlledSessionTickerFactory{created: make(chan *controlledSessionTicker, 1)}
}

func (f *controlledSessionTickerFactory) New(time.Duration) sessionProgressTicker {
	ticker := &controlledSessionTicker{ticks: make(chan time.Time, 4)}
	f.created <- ticker
	return ticker
}

func (f *controlledSessionTickerFactory) Wait(t *testing.T) *controlledSessionTicker {
	t.Helper()
	select {
	case ticker := <-f.created:
		return ticker
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for progress ticker")
		return nil
	}
}

type controlledSessionTicker struct {
	ticks chan time.Time
}

func (t *controlledSessionTicker) Channel() <-chan time.Time {
	return t.ticks
}

func (t *controlledSessionTicker) Stop() {}

func (t *controlledSessionTicker) Tick(at time.Time) {
	t.ticks <- at
}

func waitSessionSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
