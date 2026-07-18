//go:build unix

package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestStopRunReapsDescendantThatSurvivesParentCancellation(t *testing.T) {
	issue := testIssue("issue-stop-process-group", "digitaldrywood/detent#1453", "In Progress")
	tracker := newFakeConnector(issue)
	processStore := &operatorStopProcessStore{}
	runner := &operatorStopDescendantRunner{
		processStore:       processStore,
		sessionID:          1453,
		pidPath:            filepath.Join(t.TempDir(), "descendant.pid"),
		started:            make(chan procgroup.Identity, 1),
		descendantSurvived: make(chan int, 1),
	}
	observedDescendant := make(chan int, 1)
	orch, err := orchestrator.New(
		orchestrator.Config{
			PollInterval:        time.Hour,
			MaxConcurrentAgents: 1,
			Project:             scheduler.ProjectCandidate{ID: "detent"},
			ActiveStates:        []string{"In Progress"},
			ObservedStates:      []string{"Blocked"},
			TerminalStates:      []string{"Done"},
			StopRunTargetState:  "Blocked",
		},
		orchestrator.Dependencies{
			Connector:       tracker,
			Runner:          runner,
			WorkerProcesses: processStore,
			WorkerReapGrace: 20 * time.Millisecond,
			ReapWorkerProcess: func(ctx context.Context, identity procgroup.Identity, grace time.Duration) (procgroup.TerminationOutcome, error) {
				select {
				case descendantPID := <-runner.descendantSurvived:
					observedDescendant <- descendantPID
				case <-time.After(time.Second):
					return "", errors.New("parent cancellation did not leave a surviving descendant")
				}
				return procgroup.Terminate(ctx, identity, grace)
			},
		},
	)
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	t.Cleanup(stop)

	identity := <-runner.started
	t.Cleanup(func() {
		_ = procgroup.Cleanup(identity.GroupID)
	})
	state := waitForOperatorStopState(t, orch, func(state orchestrator.State) bool {
		running := state.Running[issue.ID]
		return running.DetentSessionID == runner.sessionID && running.WorkerProcess.PID == identity.PID
	})
	running := state.Running[issue.ID]
	result, err := orch.StopRun(t.Context(), orchestrator.StopRunRequest{
		ProjectID:       "detent",
		IssueID:         issue.ID,
		Attempt:         running.Attempt,
		DetentSessionID: running.DetentSessionID,
	})
	if err != nil || result.Outcome != "pending" {
		t.Fatalf("StopRun() = %#v, %v", result, err)
	}

	descendantPID := <-observedDescendant
	if !operatorStopProcessAlive(descendantPID) {
		t.Fatalf("descendant %d exited before process-group termination", descendantPID)
	}
	waitForOperatorStopState(t, orch, func(state orchestrator.State) bool {
		_, stillRunning := state.Running[issue.ID]
		return !stillRunning
	})
	waitForOperatorStopProcessExit(t, descendantPID)
	reaped := processStore.reapedSnapshot()
	if len(reaped) != 1 || reaped[0].sessionID != runner.sessionID || reaped[0].reap.Outcome != store.WorkerProcessOutcomeTerminated {
		t.Fatalf("reaped worker processes = %#v", reaped)
	}
}

func TestStopRunRefusesStalePersistedProcessIdentity(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	procgroup.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("Inspect() error = %v", err)
	}
	t.Cleanup(func() {
		_ = procgroup.Cleanup(identity.GroupID)
		_ = cmd.Wait()
	})
	staleIdentity := identity
	staleIdentity.StartedAt = staleIdentity.StartedAt.Add(time.Second)

	issue := testIssue("issue-stop-stale-process", "digitaldrywood/detent#1453", "In Progress")
	tracker := newFakeConnector(issue)
	processStore := &operatorStopProcessStore{processes: []store.WorkerProcess{{
		SessionID:  2453,
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		WorkerProcessIdentity: store.WorkerProcessIdentity{
			PID:       staleIdentity.PID,
			GroupID:   staleIdentity.GroupID,
			StartedAt: staleIdentity.StartedAt,
		},
	}}}
	runner := &operatorStopIdentityRunner{
		identity:  identity,
		sessionID: 2453,
		started:   make(chan struct{}),
		returned:  make(chan struct{}),
	}
	orch, err := orchestrator.New(
		orchestrator.Config{
			PollInterval:        time.Hour,
			MaxConcurrentAgents: 1,
			Project:             scheduler.ProjectCandidate{ID: "detent"},
			ActiveStates:        []string{"In Progress"},
			ObservedStates:      []string{"Blocked"},
			TerminalStates:      []string{"Done"},
			StopRunTargetState:  "Blocked",
		},
		orchestrator.Dependencies{Connector: tracker, Runner: runner, WorkerProcesses: processStore, WorkerReapGrace: 20 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	t.Cleanup(stop)
	<-runner.started
	state := waitForOperatorStopState(t, orch, func(state orchestrator.State) bool {
		return state.Running[issue.ID].WorkerProcess.PID == identity.PID
	})
	running := state.Running[issue.ID]
	result, err := orch.StopRun(t.Context(), orchestrator.StopRunRequest{ProjectID: "detent", IssueID: issue.ID, Attempt: running.Attempt})
	if err != nil || result.Outcome != "pending" {
		t.Fatalf("StopRun() = %#v, %v", result, err)
	}
	<-runner.returned
	waitForOperatorStopState(t, orch, func(state orchestrator.State) bool {
		blocked := state.Blocked[issue.ID]
		_, stillRunning := state.Running[issue.ID]
		return stillRunning && strings.Contains(blocked.Reason, "persisted PID, PGID, or start time is stale")
	})
	if !operatorStopProcessAlive(identity.PID) {
		t.Fatal("stale identity refusal terminated the unrelated test process")
	}
	if len(processStore.reapedSnapshot()) != 0 {
		t.Fatalf("stale worker process was marked reaped: %#v", processStore.reapedSnapshot())
	}
	if len(tracker.stateUpdateCalls()) != 0 {
		t.Fatalf("tracker state updates = %#v, want none before process-group exit", tracker.stateUpdateCalls())
	}
}

func TestStopRunRetriesWorkerReapAfterSessionCompletion(t *testing.T) {
	identity := procgroup.Identity{PID: 41453, GroupID: 41453, StartedAt: time.Unix(1453, 0).UTC()}
	issue := testIssue("issue-stop-reap-retry", "digitaldrywood/detent#1453", "In Progress")
	tracker := newFakeConnector(issue)
	processStore := &operatorStopProcessStore{
		processes: []store.WorkerProcess{{
			SessionID:  3453,
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			WorkerProcessIdentity: store.WorkerProcessIdentity{
				PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt,
			},
		}},
		markFailures: 1,
	}
	runner := &operatorStopIdentityRunner{
		identity:  identity,
		sessionID: 3453,
		started:   make(chan struct{}),
		returned:  make(chan struct{}),
	}
	var reapMu sync.Mutex
	var reapedIdentities []procgroup.Identity
	orch, err := orchestrator.New(
		orchestrator.Config{
			PollInterval:        time.Hour,
			MaxConcurrentAgents: 1,
			Project:             scheduler.ProjectCandidate{ID: "detent"},
			ActiveStates:        []string{"In Progress"},
			ObservedStates:      []string{"Blocked"},
			TerminalStates:      []string{"Done"},
			StopRunTargetState:  "Blocked",
		},
		orchestrator.Dependencies{
			Connector:       tracker,
			Runner:          runner,
			WorkerProcesses: processStore,
			ReapWorkerProcess: func(_ context.Context, identity procgroup.Identity, _ time.Duration) (procgroup.TerminationOutcome, error) {
				reapMu.Lock()
				defer reapMu.Unlock()
				reapedIdentities = append(reapedIdentities, identity)
				if len(reapedIdentities) == 1 {
					return procgroup.TerminationOutcomeTerminated, nil
				}
				return procgroup.TerminationOutcomeAlreadyExited, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	t.Cleanup(stop)
	<-runner.started
	state := waitForOperatorStopState(t, orch, func(state orchestrator.State) bool {
		return state.Running[issue.ID].WorkerProcess.PID == identity.PID
	})
	running := state.Running[issue.ID]
	request := orchestrator.StopRunRequest{ProjectID: "detent", IssueID: issue.ID, Attempt: running.Attempt}
	result, err := orch.StopRun(t.Context(), request)
	if err != nil || result.Outcome != "pending" {
		t.Fatalf("StopRun() = %#v, %v", result, err)
	}
	<-runner.returned
	waitForOperatorStopState(t, orch, func(state orchestrator.State) bool {
		blocked := state.Blocked[issue.ID]
		_, stillRunning := state.Running[issue.ID]
		return stillRunning && strings.Contains(blocked.Reason, "persist reap outcome")
	})
	processStore.clearProcesses()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		result, err = orch.StopRun(t.Context(), request)
		if err == nil && result.Outcome == "succeeded" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil || result.Outcome != "succeeded" || !result.AlreadyStopped {
		t.Fatalf("retry StopRun() = %#v, %v", result, err)
	}
	reapMu.Lock()
	gotReapedIdentities := append([]procgroup.Identity(nil), reapedIdentities...)
	reapMu.Unlock()
	if len(gotReapedIdentities) != 2 || gotReapedIdentities[0] != identity || gotReapedIdentities[1] != identity {
		t.Fatalf("reaped identities = %#v, want persisted identity twice", gotReapedIdentities)
	}
	reaped := processStore.reapedSnapshot()
	if len(reaped) != 1 || reaped[0].sessionID != runner.sessionID || reaped[0].reap.Outcome != store.WorkerProcessOutcomeAlreadyExited {
		t.Fatalf("reaped worker processes = %#v", reaped)
	}
}

type operatorStopDescendantRunner struct {
	processStore       *operatorStopProcessStore
	sessionID          int64
	pidPath            string
	started            chan procgroup.Identity
	descendantSurvived chan int
}

func (r *operatorStopDescendantRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	script := "sleep 30 >/dev/null 2>&1 & printf '%s\\n' \"$!\" > " + operatorStopShellQuote(r.pidPath) + "; wait"
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
	procgroup.Configure(cmd)
	if err := cmd.Start(); err != nil {
		return orchestrator.RunResult{}, err
	}
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		_ = procgroup.Cleanup(procgroup.GroupID(cmd))
		_ = cmd.Wait()
		return orchestrator.RunResult{}, err
	}
	descendantPID, err := readOperatorStopPID(r.pidPath, time.Second)
	if err != nil {
		_ = procgroup.Cleanup(identity.GroupID)
		_ = cmd.Wait()
		return orchestrator.RunResult{}, err
	}
	r.processStore.setProcess(store.WorkerProcess{
		SessionID:  r.sessionID,
		IssueID:    request.Issue.ID,
		Identifier: request.Issue.Identifier,
		WorkerProcessIdentity: store.WorkerProcessIdentity{
			PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt,
		},
	})
	if request.OnUsageUpdate != nil {
		if err := request.OnUsageUpdate(orchestrator.UsageUpdate{DetentSessionID: r.sessionID, WorkerProcess: identity}); err != nil {
			_ = procgroup.Cleanup(identity.GroupID)
			_ = cmd.Wait()
			return orchestrator.RunResult{}, err
		}
	}
	r.started <- identity
	<-ctx.Done()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if operatorStopProcessAlive(descendantPID) {
		r.descendantSurvived <- descendantPID
	} else {
		r.descendantSurvived <- -descendantPID
	}
	return orchestrator.RunResult{}, ctx.Err()
}

type operatorStopIdentityRunner struct {
	identity  procgroup.Identity
	sessionID int64
	started   chan struct{}
	returned  chan struct{}
}

func (r *operatorStopIdentityRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	if request.OnUsageUpdate != nil {
		if err := request.OnUsageUpdate(orchestrator.UsageUpdate{DetentSessionID: r.sessionID, WorkerProcess: r.identity}); err != nil {
			return orchestrator.RunResult{}, err
		}
	}
	close(r.started)
	<-ctx.Done()
	close(r.returned)
	return orchestrator.RunResult{}, ctx.Err()
}

type operatorStopProcessStore struct {
	mu           sync.Mutex
	processes    []store.WorkerProcess
	reaped       []operatorStopProcessReap
	markFailures int
}

type operatorStopProcessReap struct {
	sessionID int64
	reap      store.WorkerProcessReap
}

func (s *operatorStopProcessStore) ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.WorkerProcess(nil), s.processes...), nil
}

func (s *operatorStopProcessStore) MarkSessionWorkerProcessReaped(_ context.Context, sessionID int64, reap store.WorkerProcessReap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markFailures > 0 {
		s.markFailures--
		return errors.New("worker reap outcome persistence failed")
	}
	s.reaped = append(s.reaped, operatorStopProcessReap{sessionID: sessionID, reap: reap})
	return nil
}

func (s *operatorStopProcessStore) setProcess(process store.WorkerProcess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processes = []store.WorkerProcess{process}
}

func (s *operatorStopProcessStore) clearProcesses() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processes = nil
}

func (s *operatorStopProcessStore) reapedSnapshot() []operatorStopProcessReap {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]operatorStopProcessReap(nil), s.reaped...)
}

func readOperatorStopPID(path string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(time.Millisecond)
	}
	return 0, fmt.Errorf("timed out waiting for descendant PID at %s", path)
}

func waitForOperatorStopProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !operatorStopProcessAlive(pid) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}

func operatorStopProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func operatorStopShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
