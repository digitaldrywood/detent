package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

func TestProviderIdentityFailureDoesNotCancelTurn(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"implementation", "validator", "triage"} {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"cancelled", context.Canceled},
			{"timed out", context.DeadlineExceeded},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				ss := &failingProviderIdentityStore{fakeSessionStore: &fakeSessionStore{sessionID: 2626}, err: tc.err}
				backend := &fakeCodexClient{
					updates: []AgentUpdate{
						{Type: AgentUpdateTurnStarted, ThreadID: "thread", TurnID: "turn"},
						{Type: AgentUpdateMessageDelta, ThreadID: "thread", Delta: `{"verdict":"pass","score":1,"summary":"continued"}`},
						{Type: AgentUpdateTurnCompleted, ThreadID: "thread", TurnID: "turn"},
					},
					result: AgentTurnResult{ThreadID: "thread", TurnID: "turn"},
				}
				info := workspace.Info{Path: t.TempDir(), Key: "provider-identity"}
				var logs bytes.Buffer
				r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource,
					SecurityAuditRoot: t.TempDir(),
					Logger:            slog.New(slog.NewTextHandler(&logs, nil)),
					Workflow:          config.Workflow{Config: config.Config{Gate: gate.Config{Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Work"},
					Workspace:         &fakeWorkspaceBackend{info: info}, AgentBackend: backend, Store: ss,
				})
				if err != nil {
					t.Fatal(err)
				}
				issue := connector.Issue{ID: "2626", Identifier: "digitaldrywood/detent#2626"}
				switch role {
				case "triage":
					result, err := r.Run(t.Context(), RunRequest{Issue: issue, Mode: RunModeTriage})
					if err != nil {
						t.Fatalf("Run triage: %v", err)
					}
					if result.FinalState != FinalStateCompleted || !strings.Contains(result.Output, "continued") {
						t.Fatalf("result = %+v", result)
					}
				case "validator":
					result, err := r.Validate(t.Context(), testValidatorRequest(issue))
					if err != nil {
						t.Fatalf("Validate: %v", err)
					}
					if result.Summary != "continued" || result.Verdict != gate.ValidatorVerdictPass {
						t.Fatalf("result = %+v", result)
					}
				default:
					execution := r.runAgentTurn(t.Context(), backend, AgentTurnRequest{Workspace: info.Path}, RunRequest{Issue: issue}, info, workspace.Issue{ID: issue.ID, Identifier: issue.Identifier}, config.Agent{}, "", nil, time.Now(), 2626, agentidentity.Identity{}, nil, 0, "", "")
					if execution.err != nil {
						t.Fatalf("runAgentTurn: %v", execution.err)
					}
					if execution.cleanupErr != nil {
						t.Fatal(execution.cleanupErr)
					}
				}
				if !strings.Contains(logs.String(), "agent session provider identity persistence deferred") {
					t.Fatalf("missing persistence warning: %s", logs.String())
				}
				if ss.calls != 3 || len(ss.providerUpdates) != 2 {
					t.Fatalf("writes = %d, successful writes = %d; want 3 and 2", ss.calls, len(ss.providerUpdates))
				}
				if ss.providerUpdates[1].ThreadID != "thread" {
					t.Fatalf("persisted identity = %+v", ss.providerUpdates[1])
				}
				if !ss.bounded {
					t.Fatal("provider persistence context has no deadline")
				}
			})
		}
	}
}

type failingProviderIdentityStore struct {
	*fakeSessionStore
	err        error
	calls      int
	bounded    bool
	contextErr error
	writeDone  <-chan struct{}
}

func (s *failingProviderIdentityStore) UpdateSessionProviderIdentity(ctx context.Context, id int64, identity store.SessionProviderIdentity) error {
	s.calls++
	s.contextErr = ctx.Err()
	s.writeDone = ctx.Done()
	_, s.bounded = ctx.Deadline()
	if s.calls == 2 {
		return fmt.Errorf("updating codex session provider identity: %w", s.err)
	}
	return s.fakeSessionStore.UpdateSessionProviderIdentity(ctx, id, identity)
}

func TestProviderIdentityPersistenceDetachesCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ss := &failingProviderIdentityStore{fakeSessionStore: &fakeSessionStore{}}
	r := &Runner{store: ss}
	r.persistSessionProviderIdentity(ctx, 2626, AgentUpdate{ThreadID: "thread"})
	if !ss.bounded || len(ss.providerUpdates) != 1 {
		t.Fatal("identity was not persisted with a bounded context")
	}
	if ss.contextErr != nil {
		t.Fatalf("store inherited cancellation: %v", ss.contextErr)
	}
	select {
	case <-ss.writeDone:
	default:
		t.Fatal("write context was not released")
	}
}

// Provider writes retain their existing ten-second budget even when the caller
// supplies a telemetry-sized deadline.
func TestProviderIdentityWriteOutlivesReadDeadline(t *testing.T) {
	for _, delay := range []time.Duration{0, 600 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				ss := &delayedProviderIdentityStore{fakeSessionStore: &fakeSessionStore{}, delay: delay}
				r := &Runner{store: ss, logger: slog.Default()}
				r.persistSessionProviderIdentity(ctx, 6004, AgentUpdate{ThreadID: "thread"})
				if len(ss.providerUpdates) != 1 {
					t.Fatal("provider write inherited short read deadline")
				}
			})
		})
	}
}

type delayedProviderIdentityStore struct {
	*fakeSessionStore
	delay time.Duration
}

func (s *delayedProviderIdentityStore) UpdateSessionProviderIdentity(ctx context.Context, id int64, identity store.SessionProviderIdentity) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
		return s.fakeSessionStore.UpdateSessionProviderIdentity(ctx, id, identity)
	}
}

func TestResourceUsageDoesNotInspectLiveDiff(t *testing.T) {
	directory := initRunnerSourceRepo(t)
	attemptDiffFile(t, directory, "work.go", "package example\n")
	info := workspace.Info{Path: directory}
	source := &resourceUsageWorkspace{Backend: &fakeWorkspaceBackend{info: info}}
	backend := &fakeCodexClient{result: AgentTurnResult{ThreadID: "thread", TurnID: "turn"}}
	for sample := range 5 {
		backend.updates = append(backend.updates, AgentUpdate{Type: AgentUpdateResourceUsage, RSSBytes: uint64(sample + 1)})
	}
	backend.updates = append(backend.updates, AgentUpdate{Type: AgentUpdateTurnCompleted, ThreadID: "thread", TurnID: "turn", Status: "completed"})
	r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Store: &fakeSessionStore{sessionID: 42}, Workspace: source, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	r.now = func() time.Time {
		at = at.Add(3 * time.Second)
		return at
	}
	var registered []string
	ctx := procgroup.WithCommandOwner(t.Context(), func(_ context.Context, cmd *exec.Cmd, _ procgroup.Identity) (func(error, error) error, error) {
		registered = append(registered, strings.Join(cmd.Args, " "))
		return func(runErr, cleanupErr error) error { return errors.Join(runErr, cleanupErr) }, nil
	}, gitexec.PrepareLocalInspection)
	var gotSamples []uint64
	var terminalDiff DiffStats
	request := RunRequest{workspaceBackend: source, OnUsageUpdate: func(usage UsageUpdate) error {
		if usage.LastEvent == string(AgentUpdateTurnCompleted) {
			terminalDiff = usage.DiffStats
		} else {
			gotSamples = append(gotSamples, usage.RSSBytes)
			if source.samples != 0 || len(registered) != 0 {
				t.Errorf("RSS sample %d: %d diff refreshes and %d host command registrations", usage.RSSBytes, source.samples, len(registered))
			}
		}
		return nil
	}}
	result := r.runAgentTurn(ctx, backend, AgentTurnRequest{}, request, info, workspace.Issue{}, config.Agent{}, "", nil, at, 42, agentidentity.Identity{}, nil, 0, "example-model", "codex")
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !slices.Equal(gotSamples, []uint64{1, 2, 3, 4, 5}) || source.samples != 1 || terminalDiff.FilesChanged != 1 || terminalDiff.Fingerprint == "" || len(registered) != 0 {
		t.Fatalf("RSS samples=%v diff refreshes=%d terminal diff=%+v host commands=%v", gotSamples, source.samples, terminalDiff, registered)
	}
}

type resourceUsageWorkspace struct {
	workspace.Backend
	samples int
}

func (w *resourceUsageWorkspace) DiffStat(ctx context.Context, info workspace.Info, _ workspace.Issue) (workspace.DiffStat, error) {
	w.samples++
	return workspace.GitDiffStat(ctx, info.Path)
}

func TestCodexTurnCompletionSurvivesSlowRuntimeObservation(t *testing.T) {
	for _, blocked := range []string{"runtime", "artifact", "usage", "activity", "live diff"} {
		t.Run(blocked, func(t *testing.T) { testCodexTurnWhileObservationBlocked(t, blocked) })
	}
}

func testCodexTurnWhileObservationBlocked(t *testing.T, blocked string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		observing := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		execution := &slowTurnObservation{observing: observing, release: release, blocked: blocked}
		var peer *SSHPeer
		if blocked == "usage" || blocked == "activity" {
			left, right := net.Pipe()
			central := NewSSHPeer(ctx, left, left, func(ctx context.Context, method string, _ []json.RawMessage) (any, error) {
				if method == blocked {
					return nil, execution.block(ctx)
				}
				return nil, nil
			})
			peer = NewSSHPeer(ctx, right, right, nil)
			defer func() { central.Close(); peer.Close(); left.Close(); right.Close() }()
		}
		info := workspace.Info{Path: t.TempDir()}
		source := &completedExecutionWorkspace{retainedExecutionWorkspace: retainedExecutionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: info, recoveryStates: []workspace.RecoveryState{{HeadSHA: "completed-head"}}}}}
		backend := &completionWithPendingTool{observing: observing, blockAfter: AgentUpdateTurnStarted}
		switch blocked {
		case "artifact":
			backend.blockAfter = AgentUpdateMessageDelta
		case "live diff":
			backend.blockAfter = AgentUpdateTurnCompleted
		}
		var sessions SessionStore = &fakeSessionStore{sessionID: 42}

		r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Store: sessions, Workspace: source, AgentBackend: backend})
		if err != nil {
			t.Fatal(err)
		}
		toolStarted := make(chan struct{})
		done := make(chan agentTurnExecution, 1)
		go func() {
			request := RunRequest{
				Execution: execution, finalizeNativeWork: true, workspaceBackend: source, artifactSpool: &sourceArtifactSpool{},
				AgentTools: []AgentTool{{Name: "get_change"}},
				AgentToolHandler: func(ctx context.Context, _ AgentToolCall) (AgentToolResult, error) {
					close(toolStarted)
					<-ctx.Done()
					return AgentToolResult{}, ctx.Err()
				},
			}
			if blocked == "live diff" {
				request.workspaceBackend = &blockedLiveDiff{Backend: source, execution: execution}
				request.OnUsageUpdate = func(UsageUpdate) error { return nil }
			}
			if peer != nil {
				wire := SSHRunRequest{Request: request, Callbacks: []string{"usage", "activity"}}
				request = wire.Bind(ctx, peer)
			}
			issue := workspace.Issue{}
			result := r.runAgentTurn(ctx, backend, AgentTurnRequest{}, request, info, issue, config.Agent{}, "", nil, time.Now(), 42, agentidentity.Identity{}, nil, 0, "example-model", "codex")
			if result.err == nil {
				finalizeCtx, finish := context.WithTimeout(ctx, time.Minute)
				defer finish()
				result.err = r.afterExecution(finalizeCtx, request, source, info, issue, AgentResume{ThreadID: result.turnResult.ThreadID}, result.turnStarted)
			}
			done <- result
		}()
		<-observing
		synctest.Wait()
		select {
		case <-toolStarted:
		default:
			t.Fatal("dynamic read did not start")
		}
		select {
		case result := <-done:
			if result.err != nil || result.result.FinalMessage != "complete" || result.turnResult.TurnID != "turn" {
				t.Fatalf("terminal error=%v message=%q turn=%q", result.err, result.result.FinalMessage, result.turnResult.TurnID)
			}
			if source.finalizationCalls != 1 {
				t.Fatalf("source finalizations=%d", source.finalizationCalls)
			}
			if blocked == "artifact" && execution.expectedArtifactChunks != 1 {
				t.Fatalf("expected artifact chunks = %d", execution.expectedArtifactChunks)
			}
		default:
			t.Fatal("matching completion was held behind a runtime observation")
		}
	})
}

type slowTurnObservation struct {
	finalizingTestExecution
	observing              chan struct{}
	release                <-chan struct{}
	blocked                string
	expectedArtifactChunks int
}

func (e *slowTurnObservation) ObserveRuntime(ctx context.Context, _ tracker.NativeRuntimeObservation) error {
	if e.blocked != "runtime" {
		return nil
	}
	return e.block(ctx)
}

func (e *slowTurnObservation) SpoolArtifactLog(ctx context.Context, _ string) error {
	if e.blocked != "artifact" {
		return nil
	}
	return e.block(ctx)
}

func (e *slowTurnObservation) FinalizeArtifactsWithSpoolCount(ctx context.Context, directory string, expected int) error {
	e.expectedArtifactChunks = expected
	return e.FinalizeArtifacts(ctx, directory)
}

func (e *slowTurnObservation) block(ctx context.Context) error {
	select {
	case <-e.observing:
	default:
		close(e.observing)
	}
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type blockedLiveDiff struct {
	workspace.Backend
	execution *slowTurnObservation
}

func (w *blockedLiveDiff) DiffStat(ctx context.Context, _ workspace.Info, _ workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, w.execution.block(ctx)
}

type completionWithPendingTool struct {
	fakeCodexClient
	observing  <-chan struct{}
	blockAfter AgentUpdateType
}

func (b *completionWithPendingTool) RunTurnWithTools(ctx context.Context, _ AgentTurnRequest, _ []AgentTool, call AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	toolCtx, stopTool := context.WithCancel(ctx)
	toolDone := make(chan struct{})
	go func() {
		defer close(toolDone)
		_, _ = call(toolCtx, AgentToolCall{Name: "get_change"})
	}()
	defer func() { stopTool(); <-toolDone }()
	for _, event := range []AgentUpdate{
		{Type: AgentUpdateProviderIdentity, ThreadID: "thread"},
		{Type: AgentUpdateRuntimeIdentity, ThreadID: "thread", RuntimeIdentity: agentidentity.RuntimeUpdate("example-model", "", "", "", time.Time{})},
		{Type: AgentUpdateTurnStarted, ThreadID: "thread", TurnID: "turn"},
		{Type: AgentUpdateMessageDelta, ThreadID: "thread", TurnID: "turn", ItemID: "final", Delta: "complete"},
		{Type: AgentUpdateTurnCompleted, ThreadID: "thread", TurnID: "turn", Status: "completed"},
	} {
		if err := update(event); err != nil {
			return AgentTurnResult{}, err
		}
		if event.Type == b.blockAfter {
			<-b.observing
		}
	}
	return AgentTurnResult{ThreadID: "thread", TurnID: "turn"}, nil
}
