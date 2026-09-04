package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

// A ready higher-priority project that never acquires must not strand real work.
// This previously required a full gate rebuild to recover: the phantom project
// held a priority reservation, so the stranded issue could not dispatch at all.
// Priority now orders contention only, so the issue dispatches immediately and
// the rebuild path below still recovers it.
func TestBootRegistryRebuildDispatchesStrandedActiveIssueDespiteReadyHigherPriorityProject(t *testing.T) {
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
	select {
	case request := <-strandedRunner.started:
		if request.Issue.ID != issue.ID {
			t.Fatalf("RunRequest.Issue = %#v, want stranded issue %#v", request.Issue, issue)
		}
	case <-time.After(5 * time.Second):
		strandedState := restartRecoveryState(t, stranded)
		t.Fatalf(
			"timed out waiting for dispatch while %q was merely ready; capacity was free. Running = %#v, RecentEvents = %#v",
			phantom.ID,
			strandedState.Running,
			strandedState.RecentEvents,
		)
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
