//go:build unix

package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestStopRunReapsDescendantThatSurvivesParentCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	descendantInput, releaseDescendant, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = descendantInput.Close()
		_ = releaseDescendant.Close()
	})
	issue := testIssue("issue-stop-process-group", "example-org/example-project#1453", "In Progress")
	tracker := newFakeConnector(issue)
	processStore := &operatorStopProcessStore{}
	runner := &operatorStopDescendantRunner{
		processStore:       processStore,
		sessionID:          1453,
		descendantInput:    descendantInput,
		started:            make(chan procgroup.Identity, 1),
		descendantSurvived: make(chan int, 1),
	}
	beforeTermination := make(chan int, 1)
	allowTermination := make(chan struct{})
	var allowTerminationOnce sync.Once
	releaseTermination := func() {
		allowTerminationOnce.Do(func() {
			close(allowTermination)
		})
	}
	defer releaseTermination()
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
					beforeTermination <- descendantPID
				case <-ctx.Done():
					return "", ctx.Err()
				}
				<-allowTermination
				reapCtx, cancel := context.WithCancel(ctx)
				cancel()
				return procgroup.Terminate(reapCtx, identity, grace)
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
	var state orchestrator.State
	testenv.WaitUntil(t, t.Context(), func() error {
		var err error
		state, err = orch.State(t.Context())
		if err != nil {
			return err
		}
		running := state.Running[issue.ID]
		if running.DetentSessionID != runner.sessionID || running.WorkerProcess.PID != identity.PID {
			return errors.New("worker process identity is not yet recorded")
		}
		return nil
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

	descendantPID := <-beforeTermination
	if !operatorStopProcessRunning(t.Context(), descendantPID) {
		t.Fatalf("descendant %d exited before process-group termination", descendantPID)
	}
	releaseTermination()
	testenv.WaitUntil(t, t.Context(), func() error {
		state, err := orch.State(t.Context())
		if err != nil {
			return err
		}
		if _, stillRunning := state.Running[issue.ID]; stillRunning {
			return errors.New("stopped worker is still running")
		}
		return nil
	})
	if operatorStopProcessRunning(t.Context(), descendantPID) {
		t.Fatalf("descendant %d is still alive after process-group termination", descendantPID)
	}
	reaped := processStore.reapedSnapshot()
	if len(reaped) != 1 || reaped[0].sessionID != runner.sessionID || reaped[0].reap.Outcome != store.WorkerProcessOutcomeKilled || reaped[0].reap.Reason != "operator_stopped" || reaped[0].reap.ReapedAt.IsZero() {
		t.Fatalf("reaped worker processes = %#v", reaped)
	}
}

func TestOperatorStopProcessStateRunning(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{name: "missing process"},
		{name: "sleeping process", state: "S", want: true},
		{name: "running foreground process", state: "R+", want: true},
		{name: "zombie process", state: "Z"},
		{name: "zombie process with flags", state: "Z+"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := operatorStopProcessStateRunning(tt.state); got != tt.want {
				t.Fatalf("operatorStopProcessStateRunning(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestStopRunRefusesStalePersistedProcessIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	procgroup.Configure(t.Context(), cmd)
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
	if !operatorStopProcessRunning(t.Context(), identity.PID) {
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
	synctest.Test(t, testStopRunRetriesWorkerReapAfterSessionCompletion)
}

func testStopRunRetriesWorkerReapAfterSessionCompletion(t *testing.T) {
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
	synctest.Wait()
	state, err = orch.State(t.Context())
	if err != nil {
		t.Fatalf("State() after session completion error = %v", err)
	}
	if _, stillRunning := state.Running[issue.ID]; !stillRunning {
		t.Fatal("running worker removed before reap outcome was persisted")
	}
	if blocked := state.Blocked[issue.ID]; !strings.Contains(blocked.Reason, "persist reap outcome") {
		t.Fatalf("Blocked[%q] = %#v, want reap outcome persistence failure", issue.ID, blocked)
	}
	if len(tracker.stateUpdateCalls()) != 0 {
		t.Fatalf("tracker state updates = %#v, want none before reap outcome persistence", tracker.stateUpdateCalls())
	}
	processStore.clearProcesses()

	result, err = orch.StopRun(t.Context(), request)
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
	synctest.Wait()
	state, err = orch.State(t.Context())
	if err != nil {
		t.Fatalf("State() after reap retry error = %v", err)
	}
	if _, stillRunning := state.Running[issue.ID]; stillRunning {
		t.Fatal("running worker retained after reap retry succeeded")
	}
}

type operatorStopDescendantRunner struct {
	processStore       *operatorStopProcessStore
	sessionID          int64
	descendantInput    *os.File
	started            chan procgroup.Identity
	descendantSurvived chan int
}

func (r *operatorStopDescendantRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return orchestrator.RunResult{}, err
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	script := `sh -c 'trap "" TERM; printf "%s\n" "$$"; read -r release <&3' & wait`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
	cmd.Stdout = readyWriter
	cmd.ExtraFiles = []*os.File{r.descendantInput}
	procgroup.Configure(ctx, cmd)
	if err := cmd.Start(); err != nil {
		return orchestrator.RunResult{}, err
	}
	_ = readyWriter.Close()
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		_ = procgroup.Cleanup(procgroup.GroupID(cmd))
		_ = cmd.Wait()
		return orchestrator.RunResult{}, err
	}
	var descendantPID int
	if _, err := fmt.Fscan(readyReader, &descendantPID); err != nil {
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
	if operatorStopProcessRunning(context.WithoutCancel(ctx), descendantPID) {
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

func operatorStopProcessRunning(ctx context.Context, pid int) bool {
	if pid <= 0 {
		return false
	}
	output, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		return false
	}
	return operatorStopProcessStateRunning(string(output))
}

func operatorStopProcessStateRunning(state string) bool {
	state = strings.TrimSpace(state)
	return state != "" && !strings.HasPrefix(state, "Z")
}
