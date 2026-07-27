package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerEnforcesConfiguredDurationLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		agent           config.Agent
		want            error
		wantMaxDuration time.Duration
	}{
		{
			name:            "turn duration",
			agent:           config.Agent{MaxTurnDurationMS: 25},
			want:            ErrTurnDurationExceeded,
			wantMaxDuration: 25 * time.Millisecond,
		},
		{
			name:  "session duration",
			agent: config.Agent{MaxSessionDurationMS: 25},
			want:  ErrSessionDurationExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspaceBackend := &fakeWorkspaceBackend{
				info: workspace.Info{Path: t.TempDir(), Key: "issue-duration"},
			}
			agentBackend := &durationBlockingAgentBackend{}
			sessionStore := &fakeSessionStore{sessionID: 1496}
			runner, err := NewRunner(Dependencies{
				Workflow: config.Workflow{
					Config: config.Config{Agent: tt.agent},
					Prompt: "Work",
				},
				Workspace:    workspaceBackend,
				AgentBackend: agentBackend,
				Store:        sessionStore,
			})
			if err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}

			startedAt := time.Now()
			_, err = runner.Run(context.Background(), RunRequest{
				Issue: connector.Issue{
					ID:         "issue-duration",
					Identifier: "digitaldrywood/detent#1496",
				},
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want %v", err, tt.want)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run() error = %v, want context deadline exceeded", err)
			}
			if elapsed := time.Since(startedAt); elapsed > time.Second {
				t.Fatalf("Run() elapsed = %v, want configured duration limit", elapsed)
			}
			if !strings.Contains(err.Error(), "after 25ms") {
				t.Fatalf("Run() error = %v, want configured duration", err)
			}
			if agentBackend.request.TurnTimeout != 0 {
				t.Fatalf("AgentTurnRequest.TurnTimeout = %v, want backend liveness timeout unchanged", agentBackend.request.TurnTimeout)
			}
			if agentBackend.request.MaxDuration != tt.wantMaxDuration {
				t.Fatalf("AgentTurnRequest.MaxDuration = %v, want %v", agentBackend.request.MaxDuration, tt.wantMaxDuration)
			}
			if len(agentBackend.deadlines) != 4 {
				t.Fatalf("backend deadlines = %v, want one deadline before and after each update", agentBackend.deadlines)
			}
			for _, deadline := range agentBackend.deadlines[1:] {
				if !deadline.Equal(agentBackend.deadlines[0]) {
					t.Fatalf("backend deadlines = %v, want activity not to extend total duration", agentBackend.deadlines)
				}
			}
			if sessionStore.finishCalls != 1 {
				t.Fatalf("FinishSession() calls = %d, want 1", sessionStore.finishCalls)
			}
			if !workspaceBackend.afterRun {
				t.Fatal("AfterRun() was not called")
			}
			if workspaceBackend.afterRunErr != nil {
				t.Fatalf("AfterRun() context error = %v, want fresh cleanup context", workspaceBackend.afterRunErr)
			}
		})
	}
}

func TestRunnerSessionDurationSpansResumeFallback(t *testing.T) {
	t.Parallel()

	sessionDurationLimit := &controlledDurationLimit{}
	agentBackend := &durationResumeFallbackAgentBackend{
		expireSession: sessionDurationLimit.Expire,
	}
	sessionStore := &fakeSessionStore{
		sessionID: 1496,
		resumeState: store.AgentResumeState{
			DetentSessionID:   1495,
			ProviderThreadID:  "thread-1495",
			ProviderSessionID: "session-1495",
		},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{
					ExperimentalThreadResume: true,
					MaxSessionDurationMS:     25,
				},
				Agents: config.Agents{Routes: []config.AgentRoute{{
					Backend: config.DefaultAgentBackendID,
					Model:   "gpt-5-codex",
					Default: true,
				}}},
			},
			Prompt: "Work",
		},
		Workspace: &fakeWorkspaceBackend{
			info: workspace.Info{Path: t.TempDir(), Key: "issue-duration-resume"},
		},
		AgentBackend: agentBackend,
		Store:        sessionStore,
		sessionLimit: sessionDurationLimit.Context,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	_, err = runner.Run(context.Background(), RunRequest{
		Issue: connector.Issue{
			ID:         "issue-duration-resume",
			Identifier: "digitaldrywood/detent#1496",
		},
	})
	if !errors.Is(err, ErrSessionDurationExceeded) {
		t.Fatalf("Run() error = %v, want ErrSessionDurationExceeded", err)
	}
	if agentBackend.calls != 2 {
		t.Fatalf("RunTurn() calls = %d, want resume attempt and fresh fallback", agentBackend.calls)
	}
	if agentResumeEmpty(agentBackend.requests[0].Resume) {
		t.Fatal("first RunTurn() resume state is empty")
	}
	if !agentResumeEmpty(agentBackend.requests[1].Resume) {
		t.Fatalf("second RunTurn() resume state = %#v, want fresh fallback", agentBackend.requests[1].Resume)
	}
	if sessionDurationLimit.duration != 25*time.Millisecond {
		t.Fatalf("session duration = %v, want 25ms", sessionDurationLimit.duration)
	}
	if !errors.Is(sessionDurationLimit.limit, ErrSessionDurationExceeded) {
		t.Fatalf("session duration limit = %v, want ErrSessionDurationExceeded", sessionDurationLimit.limit)
	}
}

func TestRunAgentBackendTurnPreservesParentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err, cleanupErr := runAgentBackendTurn(ctx, &durationBlockingAgentBackend{}, AgentTurnRequest{
		MaxDuration: time.Hour,
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runAgentBackendTurn() error = %v, want context canceled", err)
	}
	if errors.Is(err, ErrTurnDurationExceeded) {
		t.Fatalf("runAgentBackendTurn() error = %v, want parent cancellation", err)
	}
	if cleanupErr != nil {
		t.Fatalf("runAgentBackendTurn() cleanup error = %v, want nil", cleanupErr)
	}
}

func TestRunAgentBackendTurnKeepsLivenessTimeoutSeparateFromMaxDuration(t *testing.T) {
	t.Parallel()

	backend := &durationBlockingAgentBackend{}
	_, err, cleanupErr := runAgentBackendTurn(context.Background(), backend, AgentTurnRequest{
		TurnTimeout: time.Hour,
		MaxDuration: 25 * time.Millisecond,
	}, nil)
	if !errors.Is(err, ErrTurnDurationExceeded) {
		t.Fatalf("runAgentBackendTurn() error = %v, want ErrTurnDurationExceeded", err)
	}
	if cleanupErr != nil {
		t.Fatalf("runAgentBackendTurn() cleanup error = %v, want nil", cleanupErr)
	}
	if backend.request.TurnTimeout != time.Hour {
		t.Fatalf("backend TurnTimeout = %v, want liveness timeout preserved", backend.request.TurnTimeout)
	}
	if backend.request.MaxDuration != 25*time.Millisecond {
		t.Fatalf("backend MaxDuration = %v, want total duration preserved", backend.request.MaxDuration)
	}
}

func TestRunAgentBackendTurnDoesNotTreatLivenessTimeoutAsTotalDuration(t *testing.T) {
	t.Parallel()

	backend := &deadlineObservingAgentBackend{}
	_, err, cleanupErr := runAgentBackendTurn(context.Background(), backend, AgentTurnRequest{
		TurnTimeout: 25 * time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("runAgentBackendTurn() error = %v", err)
	}
	if cleanupErr != nil {
		t.Fatalf("runAgentBackendTurn() cleanup error = %v", cleanupErr)
	}
	if backend.hasDeadline {
		t.Fatal("backend context has a total deadline from the liveness timeout")
	}
	if backend.request.TurnTimeout != 25*time.Millisecond {
		t.Fatalf("backend TurnTimeout = %v, want liveness timeout preserved", backend.request.TurnTimeout)
	}
}

func TestRunAgentBackendTurnLeavesDurationDisabledWithoutDeadline(t *testing.T) {
	t.Parallel()

	backend := &deadlineObservingAgentBackend{}
	_, err, cleanupErr := runAgentBackendTurn(context.Background(), backend, AgentTurnRequest{}, nil)
	if err != nil {
		t.Fatalf("runAgentBackendTurn() error = %v", err)
	}
	if cleanupErr != nil {
		t.Fatalf("runAgentBackendTurn() cleanup error = %v", cleanupErr)
	}
	if backend.hasDeadline {
		t.Fatal("backend context has a deadline with duration limit disabled")
	}
}

func TestRunAgentBackendTurnPropagatesDurationContextToUpdates(t *testing.T) {
	t.Parallel()

	hasDeadline := false
	_, err, cleanupErr := runAgentBackendTurnWithTools(
		context.Background(),
		&durationUpdateAgentBackend{},
		AgentTurnRequest{MaxDuration: 25 * time.Millisecond},
		nil,
		nil,
		func(ctx context.Context, _ AgentUpdate) error {
			_, hasDeadline = ctx.Deadline()
			<-ctx.Done()
			return ctx.Err()
		},
	)
	if !errors.Is(err, ErrTurnDurationExceeded) {
		t.Fatalf("runAgentBackendTurnWithTools() error = %v, want ErrTurnDurationExceeded", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runAgentBackendTurnWithTools() error = %v, want context deadline exceeded", err)
	}
	if cleanupErr != nil {
		t.Fatalf("runAgentBackendTurnWithTools() cleanup error = %v, want nil", cleanupErr)
	}
	if !hasDeadline {
		t.Fatal("update context has no duration deadline")
	}
}

func TestRunnerValidatorUpdatePersistenceUsesSessionDurationContext(t *testing.T) {
	t.Parallel()

	sessionStore := &durationBlockingSessionStore{
		fakeSessionStore: &fakeSessionStore{sessionID: 1496},
	}
	runner, err := NewRunner(Dependencies{
		Workflow: config.Workflow{
			Config: config.Config{
				Agent: config.Agent{MaxSessionDurationMS: 25},
				Gate:  gate.Config{Validator: gate.ValidatorConfig{Enabled: true}},
			},
			Prompt: "Work",
		},
		Workspace: &fakeWorkspaceBackend{
			info: workspace.Info{Path: t.TempDir(), Key: "issue-validator-duration"},
		},
		AgentBackend: &durationUpdateAgentBackend{},
		Store:        sessionStore,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	startedAt := time.Now()
	_, err = runner.Validate(ctx, ValidatorRequest{
		Issue: connector.Issue{
			ID:         "issue-validator-duration",
			Identifier: "digitaldrywood/detent#1496",
		},
	})
	if !errors.Is(err, ErrSessionDurationExceeded) {
		t.Fatalf("Validate() error = %v, want ErrSessionDurationExceeded", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Validate() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("Validate() elapsed = %v, want configured session duration", elapsed)
	}
	if !sessionStore.hasDeadline {
		t.Fatal("UpdateSessionWorkerProcess() context has no session deadline")
	}
}

type durationBlockingAgentBackend struct {
	request   AgentTurnRequest
	deadlines []time.Time
}

func (b *durationBlockingAgentBackend) RunTurn(ctx context.Context, request AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	b.request = request
	b.recordDeadline(ctx)
	for range 3 {
		if onUpdate != nil {
			if err := onUpdate(AgentUpdate{
				Type:   AgentUpdateMessageDelta,
				ItemID: "message",
				Delta:  "working",
			}); err != nil {
				return AgentTurnResult{}, err
			}
		}
		b.recordDeadline(ctx)
	}
	<-ctx.Done()
	return AgentTurnResult{}, ctx.Err()
}

func (b *durationBlockingAgentBackend) recordDeadline(ctx context.Context) {
	if deadline, ok := ctx.Deadline(); ok {
		b.deadlines = append(b.deadlines, deadline)
	}
}

type durationUpdateAgentBackend struct{}

func (b *durationUpdateAgentBackend) RunTurn(_ context.Context, _ AgentTurnRequest, onUpdate AgentUpdateHandler) (AgentTurnResult, error) {
	if onUpdate == nil {
		return AgentTurnResult{}, nil
	}
	return AgentTurnResult{}, onUpdate(AgentUpdate{
		Type: AgentUpdateProcessStarted,
		WorkerProcess: procgroup.Identity{
			PID:       1496,
			GroupID:   1496,
			StartedAt: time.Now(),
		},
	})
}

type durationResumeFallbackAgentBackend struct {
	calls         int
	requests      []AgentTurnRequest
	expireSession func()
}

func (b *durationResumeFallbackAgentBackend) RunTurn(ctx context.Context, request AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	b.calls++
	b.requests = append(b.requests, request)
	if !agentResumeEmpty(request.Resume) {
		return AgentTurnResult{}, errors.New("resume failed")
	}
	b.expireSession()
	<-ctx.Done()
	return AgentTurnResult{}, ctx.Err()
}

type controlledDurationLimit struct {
	cancel   context.CancelCauseFunc
	duration time.Duration
	limit    error
}

func (l *controlledDurationLimit) Context(ctx context.Context, duration time.Duration, limit error) (context.Context, context.CancelFunc) {
	ctx, l.cancel = context.WithCancelCause(ctx)
	l.duration = duration
	l.limit = limit
	return ctx, func() {
		l.cancel(context.Canceled)
	}
}

func (l *controlledDurationLimit) Expire() {
	l.cancel(&agentDurationLimitError{
		limit:    l.limit,
		duration: l.duration,
	})
}

type deadlineObservingAgentBackend struct {
	hasDeadline bool
	request     AgentTurnRequest
}

func (b *deadlineObservingAgentBackend) RunTurn(ctx context.Context, request AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	_, b.hasDeadline = ctx.Deadline()
	b.request = request
	return AgentTurnResult{}, nil
}

type durationBlockingSessionStore struct {
	*fakeSessionStore
	hasDeadline bool
}

func (s *durationBlockingSessionStore) UpdateSessionWorkerProcess(ctx context.Context, _ int64, _ store.WorkerProcessIdentity) error {
	_, s.hasDeadline = ctx.Deadline()
	<-ctx.Done()
	return ctx.Err()
}
