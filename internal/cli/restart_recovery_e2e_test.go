package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestBootRegistryRebuildDispatchesStrandedActiveIssueAfterPoisonedGate(t *testing.T) {
	t.Parallel()

	configuredProject := globalconfig.Project{ID: "gopher-ai", Weight: 1, Priority: 3}
	configuredCandidate := globalProjectCandidates([]globalconfig.Project{configuredProject})[0]
	global := globalconfig.Config{
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 1,
			Scheduling:          globalconfig.SchedulingStrict,
		},
		Projects: []globalconfig.Project{configuredProject},
	}
	issue := connector.NewIssue()
	issue.ID = "issue-stranded"
	issue.Identifier = "gopherguides/gopher-ai#294"
	issue.Title = "stranded active work"
	issue.State = "In Progress"
	tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}})

	poisonedGate, err := buildGlobalDispatchPools(global, nil)
	if err != nil {
		t.Fatalf("buildGlobalDispatchPools() error = %v", err)
	}
	phantom := scheduler.ProjectCandidate{ID: "detent", Weight: 1, Priority: 0}
	poisonedGate.MarkReady(phantom)
	strandedRunner := newRestartRecoveryRunner()
	stranded, stopStranded := runRestartRecoveryOrchestrator(t, tracker, strandedRunner, poisonedGate, configuredCandidate)
	strandedState := restartRecoveryState(t, stranded)
	if len(strandedState.Running) != 0 {
		t.Fatalf("Running = %#v, want no live work attempt while gate is poisoned", strandedState.Running)
	}
	if !restartRecoveryEventContains(
		strandedState,
		"dispatch_slot_wait",
		"reason="+scheduler.DispatchGateReasonReservedForHigherPriorityProject,
		"selected_project_id="+phantom.ID,
	) {
		t.Fatalf("RecentEvents = %#v, want poisoned priority reservation", strandedState.RecentEvents)
	}
	select {
	case request := <-strandedRunner.started:
		t.Fatalf("unexpected runner request while gate is poisoned = %#v", request)
	default:
	}
	stopStranded()

	restartedGate, err := buildGlobalDispatchPools(global, nil)
	if err != nil {
		t.Fatalf("buildGlobalDispatchPools() after restart error = %v", err)
	}
	restartedRunner := newRestartRecoveryRunner()
	restarted, stopRestarted := runRestartRecoveryOrchestrator(t, tracker, restartedRunner, restartedGate, configuredCandidate)
	defer stopRestarted()

	select {
	case request := <-restartedRunner.started:
		if request.Issue.ID != issue.ID || request.Issue.State != issue.State {
			t.Fatalf("RunRequest.Issue = %#v, want unchanged stranded issue %#v", request.Issue, issue)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stranded issue dispatch after registry rebuild")
	}
	restartedState := restartRecoveryState(t, restarted)
	if _, ok := restartedState.Running[issue.ID]; !ok {
		t.Fatalf("Running[%q] missing after automatic restart recovery", issue.ID)
	}
}

type restartRecoveryRunner struct {
	started chan orchestrator.RunRequest
}

func newRestartRecoveryRunner() *restartRecoveryRunner {
	return &restartRecoveryRunner{started: make(chan orchestrator.RunRequest, 1)}
}

func (r *restartRecoveryRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	select {
	case r.started <- request:
	case <-ctx.Done():
		return orchestrator.RunResult{}, ctx.Err()
	}
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}

func runRestartRecoveryOrchestrator(
	t *testing.T,
	tracker connector.Connector,
	runner orchestrator.Runner,
	gate scheduler.ProjectDispatchGate,
	project scheduler.ProjectCandidate,
) (*orchestrator.Orchestrator, func()) {
	t.Helper()

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		Project:             project,
		ActiveStates:        []string{"In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	}, orchestrator.Dependencies{
		Connector:          tracker,
		Runner:             runner,
		GlobalDispatchGate: gate,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("orchestrator.New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- orch.Run(ctx)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case runErr := <-done:
				if runErr != nil && !errors.Is(runErr, context.Canceled) {
					t.Fatalf("Orchestrator.Run() error = %v", runErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for orchestrator shutdown")
			}
		})
	}
	t.Cleanup(stop)
	return orch, stop
}

func restartRecoveryState(t *testing.T, orch *orchestrator.Orchestrator) orchestrator.State {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	state, err := orch.State(ctx)
	if err != nil {
		t.Fatalf("Orchestrator.State() error = %v", err)
	}
	return state
}

func restartRecoveryEventContains(state orchestrator.State, event string, fragments ...string) bool {
	for _, candidate := range state.RecentEvents {
		if candidate.Event != event {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(candidate.Message, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
