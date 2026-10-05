package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const (
	sessionBrakeTestWaitTimeout        = 10 * time.Second
	sessionBrakeCompletionGuardTimeout = time.Minute
)

func TestRunnerStopsSessionBeyondMaxTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

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
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

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

func TestRunnerGateWaitOutlivesFormerNoProgressLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()

	startedAt := time.Now()
	timeout := time.Second
	releaseGate := make(chan struct{})
	gateStarted := make(chan struct{})
	backend := &sessionBlockingAgentBackend{
		started:     make(chan struct{}),
		stopped:     make(chan struct{}),
		release:     releaseGate,
		gateStarted: gateStarted,
	}
	sessionStore := &fakeSessionStore{sessionID: 1573}
	workspaceBackend := &fakeWorkspaceBackend{
		info:           workspace.Info{Path: t.TempDir(), Key: "issue-no-progress"},
		recoveryStates: []workspace.RecoveryState{{HeadSHA: "f834f32450227c1b693eadcf92bdccccb6066e4a"}},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{Agent: config.Agent{
				MaxTurns:            20,
				NoProgressTimeoutMS: int(timeout / time.Millisecond),
			}},
			Prompt: "Work",
		},
		Workspace:    workspaceBackend,
		AgentBackend: backend,
		Store:        sessionStore,
		Now:          time.Now,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	completionCh := make(chan sessionRunCompletion, 1)
	go func() {
		result, runErr := runner.Run(t.Context(), RunRequest{
			Issue: connector.Issue{
				ID:          "issue-no-progress",
				Identifier:  "digitaldrywood/detent#2976",
				PullRequest: &connector.PullRequest{Number: 3052, HeadSHA: "f834f32450227c1b693eadcf92bdccccb6066e4a"},
			},
			StartedAt:     startedAt,
			ProgressProbe: func(context.Context) (string, error) { return "unchanged-workpad", nil },
		})
		completionCh <- sessionRunCompletion{result: result, err: runErr}
	}()

	waitSessionSignal(t, backend.started, "agent backend start")
	// The gate holder keeps this committed-head worker first in line beyond the
	// former inactivity threshold. It may start validation when the holder exits.
	time.Sleep(3 * timeout)
	select {
	case <-backend.stopped:
		t.Fatal("live gate wait was canceled")
	default:
	}
	close(releaseGate)
	waitSessionSignal(t, gateStarted, "validation gate start")

	var completion sessionRunCompletion
	select {
	case completion = <-completionCh:
	case <-time.After(sessionBrakeCompletionGuardTimeout):
		t.Fatal("timed out waiting for gate completion")
	}
	result, runErr := completion.result, completion.err
	if runErr != nil || result.FinalState != FinalStateCompleted {
		t.Fatalf("gate completion = (%q, %v), want completed without error", result.FinalState, runErr)
	}
	if sessionStore.finishCalls != 1 {
		t.Fatalf("FinishSession() calls = %d, want 1", sessionStore.finishCalls)
	}
	if !workspaceBackend.afterRun {
		t.Fatal("AfterRun() was not called")
	}
}

func TestRunnerGateFailureReportsCommand(t *testing.T) {
	t.Parallel()
	releaseGate := make(chan struct{})
	backend := &sessionBlockingAgentBackend{
		started:   make(chan struct{}),
		stopped:   make(chan struct{}),
		release:   releaseGate,
		resultErr: errors.New("make check-fast: exit status 17"),
	}
	runner, err := NewRunner(Dependencies{
		Workflow:     config.Workflow{Config: config.Config{Agent: config.Agent{MaxTurns: 20}}, Prompt: "Work"},
		Workspace:    &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir(), Key: "gate-failure"}},
		AgentBackend: backend,
		Store:        &fakeSessionStore{sessionID: 1574},
	})
	if err != nil {
		t.Fatal(err)
	}
	completion := make(chan error, 1)
	go func() {
		_, runErr := runner.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "gate-failure", Identifier: "digitaldrywood/detent#2976"}})
		completion <- runErr
	}()
	waitSessionSignal(t, backend.started, "gate queue entry")
	close(releaseGate)
	select {
	case runErr := <-completion:
		if runErr == nil || !strings.Contains(runErr.Error(), "make check-fast: exit status 17") {
			t.Fatalf("gate failure = %v, want failing command", runErr)
		}
	case <-time.After(sessionBrakeTestWaitTimeout):
		t.Fatal("timed out waiting for gate failure")
	}
}

type sessionRunCompletion struct {
	result RunResult
	err    error
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
	started     chan struct{}
	stopped     chan struct{}
	release     <-chan struct{}
	gateStarted chan struct{}
	resultErr   error
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
	if err := onUpdate(AgentUpdate{Type: AgentUpdateToolOutput, ItemID: "gate", Delta: "validation gate waiting: position=1\n"}); err != nil {
		return AgentTurnResult{}, err
	}
	if b.release != nil {
		select {
		case <-ctx.Done():
			close(b.stopped)
			return AgentTurnResult{}, ctx.Err()
		case <-b.release:
			if b.gateStarted != nil {
				close(b.gateStarted)
			}
			if err := onUpdate(AgentUpdate{Type: AgentUpdateToolOutput, ItemID: "gate", Delta: "validation gate running: owner_pid=10\n"}); err != nil {
				return AgentTurnResult{}, err
			}
			return AgentTurnResult{ThreadID: "thread-1572", TurnID: "turn-1", SessionID: "thread-1572-turn-1"}, b.resultErr
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

func waitSessionSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(sessionBrakeTestWaitTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}
