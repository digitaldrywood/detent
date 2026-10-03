package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// Replay Cloud retiring a lease on run.finished. The real runner must leave
// publication and the lane handoff to the orchestrator before that retirement.
func TestNativePlannerAutomaticHandoff(t *testing.T) {
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name    string
		abandon bool
		failure string
	}{
		{name: "automatic handoff"},
		{name: "abandon deferred planner and recover", abandon: true},
		{name: "provider failure settles before lease retirement", failure: "provider"},
		{name: "owned cleanup failure settles instance outcome", failure: "cleanup"},
		{name: "completed provider with exited process preserves staged finalization", failure: "exited"},
	} {
		t.Run(test.name, func(t *testing.T) { testNativePlannerHandoff(t, test.abandon, test.failure) })
	}
}

func testNativePlannerHandoff(t *testing.T, abandon bool, failure string) {
	t.Helper()
	h := newNativeChangeHubTransport(t, "Human Review", hubserverPlanStates(), true)
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Plan then implement", Body: "Update the README."})
	if err != nil {
		t.Fatal(err)
	}
	source := nativeChangeSourceRepo(t)
	nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
	nativeChangeGit(t, source, "config", "url."+source+".insteadOf", nativeChangeRepository)
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	plan := gate.PlanConfig{Enabled: failure == "", Review: gate.PlanReviewAutomated}
	provider := &nativePlanningAgent{failure: failure}
	agent, err := runner.NewRunner(runner.Dependencies{
		Workflow:  config.Workflow{Config: config.Config{Policy: h.descriptor, Plan: plan, Tracker: config.Tracker{Kind: config.TrackerHubNative}}, Prompt: "Complete the issue"},
		Workspace: backend, AgentBackend: provider,
		ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) {
			if failure == "cleanup" {
				return 0, errors.Join(errors.New("owned child cleanup deadline"), context.DeadlineExceeded)
			}
			return 0, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtimeStore.Close(); err != nil {
			t.Error(err)
		}
	})
	finished := make(chan nativePlanFinish, 4)
	transport := &nativePlanTransport{next: h.failChanges, native: h.native, finished: finished, blocked: make(chan struct{}, 1)}
	transport.failWorkflow.Store(abandon)
	h.native.client.httpClient.Transport = transport
	orchCfg := orchestrator.Config{
		Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, Plan: plan,
		PollInterval: 20 * time.Millisecond, MaxConcurrentAgents: 1,
		ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Human Review"}, TerminalStates: []string{"Done"},
	}
	if abandon {
		orchCfg.PollInterval = time.Hour
	}
	orch, err := orchestrator.New(orchCfg, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: agent, WorkAttempts: runtimeStore, LaneLedger: runtimeStore, WorkflowMetrics: runtimeStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	wantStates := []string{"In Progress", "Human Review"}
	if failure != "" {
		wantStates = []string{"Human Review"}
	}
	if abandon {
		select {
		case <-transport.blocked:
		case <-time.After(10 * time.Second):
			t.Fatal("planner did not reach the completion lane write")
		}
		attempts, err := runtimeStore.ListActiveWorkAttempts(t.Context(), store.WorkAttemptQuery{ProjectID: "local"})
		if err != nil || len(attempts) != 1 {
			t.Fatalf("active planner = %v, %v", attempts, err)
		}
		receipt, err := orch.WorkAttemptReceipt(t.Context(), "local", attempts[0].ID)
		if err != nil || receipt.Attempt.Phase != "completion_deferred" {
			t.Fatalf("planner deferral = %v, %v", receipt.Attempt.Phase, err)
		}
		// Hold intake through the operator's lane move and explicit retry intent.
		// Otherwise the next tick can start a worker before the retry request.
		heldConfig := orchCfg
		heldConfig.ActiveStates = []string{"Human Review"}
		heldConfig.ObservedStates = []string{"Todo", "In Progress"}
		if err := orch.UpdateRuntime(t.Context(), orchestrator.RuntimeUpdate{Config: heldConfig}); err != nil {
			t.Fatal(err)
		}
		// Replay Cloud's missing lease followed by supported local abandon.
		h.scheduler.mu.Lock()
		lease := h.scheduler.nativeClaims[issue.ID].lease
		h.scheduler.mu.Unlock()
		if err := h.native.Release(t.Context(), lease, "completed"); err != nil {
			t.Fatal(err)
		}
		response, err := orch.RecoverWorkAttempt(t.Context(), orchestrator.WorkAttemptRecoveryRequest{ProjectID: "local", AttemptID: attempts[0].ID, Action: orchestrator.WorkAttemptRecoveryAbandon, Confirm: true, Reason: "native planner succeeded but its lease ended", Operator: "ops"})
		if err != nil || response.Attempt.TerminalState != string(store.WorkAttemptTerminalAbandoned) {
			t.Fatalf("abandon = %v, %v", response.Status, err)
		}
		current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}); err != nil {
			t.Fatal(err)
		}
		transport.failWorkflow.Store(false)
		response, err = orch.RecoverWorkAttempt(t.Context(), orchestrator.WorkAttemptRecoveryRequest{ProjectID: "local", AttemptID: attempts[0].ID, Action: orchestrator.WorkAttemptRecoveryRetryFresh, Confirm: true, Reason: "operator starts implementation from the preserved planner workspace", Operator: "ops"})
		if err != nil || !response.Queued {
			t.Fatalf("fresh implementation handoff = %v, %v", response.Status, err)
		}
		orchCfg.PollInterval = 20 * time.Millisecond
		if err := orch.UpdateRuntime(t.Context(), orchestrator.RuntimeUpdate{Config: orchCfg}); err != nil {
			t.Fatal(err)
		}
		wantStates = []string{"Human Review"}
	}
	for _, want := range wantStates {
		select {
		case got := <-finished:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.state != want {
				t.Fatalf("native run finished in %s, want handoff to %s before lease retirement", got.state, want)
			}
		case <-time.After(10 * time.Second):
			state, _ := orch.State(t.Context())
			t.Fatalf("native handoff did not finish: running=%v retry=%v", len(state.Running), state.Retry)
		}
	}
	changes := h.changes(t, issue.ID)
	if failure == "provider" || failure == "cleanup" {
		if len(changes) != 0 || provider.calls.Load() != 1 {
			t.Fatalf("failed native run created a Change or repeated coding: changes=%v calls=%d", changes, provider.calls.Load())
		}
		if _, err := os.Stat(filepath.Join(provider.workspace, "CHANGE.md")); err != nil {
			t.Fatalf("failed native source not preserved: %v", err)
		}
		staged, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "diff", "--cached", "--name-only").Output()
		if err != nil || strings.TrimSpace(string(staged)) != "CHANGE.md" {
			t.Fatalf("failed native staged source = %q, error=%v", staged, err)
		}
		current, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(current.Attempts) != 1 || current.Attempts[0].Status != "failed" {
			t.Fatalf("native failed outcome = %+v, error=%v", current.Attempts, err)
		}
		return
	}
	if len(changes) != 1 || changes[0].CurrentVersion == "" {
		t.Fatalf("implementation did not publish Change Request: %+v", changes)
	}
	comments, err := h.connector.FetchIssueComments(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	plans := 0
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, "## Detent Plan\n") {
			plans++
		}
	}
	if want := 1; failure != "" {
		if plans != 0 {
			t.Fatalf("non-planning run published a plan")
		}
	} else if plans != want {
		t.Fatalf("plan publications = %d, want one", plans)
	}
}

func hubserverPlanStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"In Progress", "Merging", "Done"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
		{Name: "Done", Terminal: true},
	}
}

type nativePlanningAgent struct {
	failure   string
	calls     atomic.Int64
	workspace string
}

func (a *nativePlanningAgent) RunTurn(ctx context.Context, req runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	a.calls.Add(1)
	a.workspace = req.Workspace
	if a.failure == "cleanup" || a.failure == "exited" {
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 2081001, GroupID: 2081001, StartedAt: time.Now()}}); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	plan := strings.Contains(req.Prompt, "This dispatch is plan-only.")
	if plan {
		if err := update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: "Implement README changes and verify them.\n\n## Detent Plan Review\n\n- state: approved\n\nThe plan covers acceptance, tests and risks."}); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	result, err := (&committingAgent{staged: !plan}).RunTurn(ctx, req, update)
	if a.failure == "provider" {
		return result, errors.Join(err, errors.New("provider task failed"))
	}
	return result, err
}

type nativePlanFinish struct {
	state string
	err   error
}
type nativePlanTransport struct {
	next         http.RoundTripper
	native       *NativeClient
	finished     chan<- nativePlanFinish
	blocked      chan struct{}
	failWorkflow atomic.Bool
}

func (t *nativePlanTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/workflow") && t.failWorkflow.Load() {
		select {
		case t.blocked <- struct{}{}:
		default:
		}
		return nil, errors.New("native workflow publication unavailable")
	}
	response, err := t.next.RoundTrip(req)
	if err != nil || response.StatusCode >= 300 || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/events") {
		return response, err
	}
	body, err := req.GetBody()
	if err != nil {
		return response, err
	}
	defer body.Close()
	var event tracker.NativeRunEvent
	if err := json.NewDecoder(body).Decode(&event); err != nil {
		return response, err
	}
	if event.Type != "run.finished" {
		return response, nil
	}
	item := tracker.NativeWorkItemID(strings.Split(strings.TrimSuffix(req.URL.Path, "/events"), "/work-items/")[1])
	issue, err := t.native.Issue(req.Context(), item)
	if err == nil {
		err = t.native.Release(req.Context(), tracker.NativeLease{ID: event.Data.LeaseID, WorkItemID: item, FencingToken: event.Data.FencingToken}, "completed")
	}
	t.finished <- nativePlanFinish{state: issue.State, err: err}
	return response, nil
}
