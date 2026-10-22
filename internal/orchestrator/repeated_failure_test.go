package orchestrator_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type tokenCeilingRunner struct {
	calls        atomic.Int64
	failureCount int64
}

type restBudgetHeadroomRunner struct {
	calls   atomic.Int64
	resetAt time.Time
}

func (r *restBudgetHeadroomRunner) Run(_ context.Context, _ orchestrator.RunRequest) (orchestrator.RunResult, error) {
	n := r.calls.Add(1)
	return orchestrator.RunResult{}, fmt.Errorf(
		"run agent turn: worker github REST budget reached reserved headroom: consumer=shared_pool credential_identity=github-rest:worker remaining=%d reserve=1250 reset_at=%s",
		950-n, r.resetAt.Format(time.RFC3339),
	)
}

func (r *tokenCeilingRunner) Run(_ context.Context, _ orchestrator.RunRequest) (orchestrator.RunResult, error) {
	n := r.calls.Add(1)
	if r.failureCount > 0 && n > r.failureCount {
		return orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted}, nil
	}
	return orchestrator.RunResult{TurnStarted: true}, fmt.Errorf(
		"run agent turn: claude update rejected: session token ceiling exceeded: total_tokens=%d ceiling_tokens=2000000 source=max_session_tokens",
		2000000+n,
	)
}

func TestRunRetriesRepeatedCostlyWorkerFailures(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-repeated-fail", "wi-011cd179bc7ecf36b7197e4b", "Todo")
	tracker := newFakeConnector(issue)
	runner := &tokenCeilingRunner{failureCount: 6}

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:           time.Millisecond,
		MaxConcurrentAgents:    1,
		MaxRetryBackoff:        time.Millisecond,
		FailureRetryBaseDelay:  time.Millisecond,
		ActiveStates:           []string{"Todo", "In Progress"},
		ObservedStates:         []string{"Blocked"},
		TerminalStates:         []string{"Done", "Cancelled", "Canceled", "Closed"},
		ContinuationRetryDelay: time.Second,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	var state orchestrator.State
	testenv.WaitUntil(t, t.Context(), func() error {
		var err error
		state, err = orch.State(t.Context())
		if err != nil {
			return err
		}
		if _, ok := state.Completed[issue.ID]; !ok {
			return fmt.Errorf("awaiting successful completion")
		}
		return nil
	})
	if runner.calls.Load() < 7 {
		t.Fatal("repeated failures stopped before success")
	}
	if len(state.Blocked) != 0 {
		t.Fatal("machine failure created a hold")
	}
	if retry, ok := state.Retry[issue.ID]; ok && retry.Attempt > 1 {
		t.Fatalf("successful completion retained failure backoff: %+v", retry)
	}
	for _, update := range tracker.stateUpdateCalls() {
		if update.state == "Blocked" {
			t.Fatal("machine failure wrote Blocked")
		}
	}
}

func TestRunDefersRESTBudgetFailureAsCapacityWithoutBreaker(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-rest-budget-park", "digitaldrywood/detent#1824", "Todo")
	tracker := newFakeConnector(issue)
	resetAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	runner := &restBudgetHeadroomRunner{resetAt: resetAt}
	metrics := &workflowMetricsRecorder{}

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:           time.Millisecond,
		MaxConcurrentAgents:    1,
		MaxRetryBackoff:        time.Millisecond,
		FailureRetryBaseDelay:  time.Millisecond,
		ActiveStates:           []string{"Todo", "In Progress"},
		ObservedStates:         []string{"Blocked"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Second,
	}, orchestrator.Dependencies{
		Connector:       tracker,
		Runner:          runner,
		WorkflowMetrics: metrics,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	state := waitForState(t, orch, func(state orchestrator.State) bool {
		_, ok := state.Retry[issue.ID]
		return ok
	})
	stop()

	retry := state.Retry[issue.ID]
	if want := resetAt.Add(5 * time.Second); !retry.DueAt.Equal(want) {
		t.Fatalf("retry DueAt = %s, want %s", retry.DueAt, want)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1 before reset", got)
	}
	if _, ok := state.Blocked[issue.ID]; ok {
		t.Fatalf("Blocked[%q] present after capacity deferral", issue.ID)
	}
	if state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want inactive", state.FailureBreaker)
	}
	for _, event := range metrics.snapshot() {
		if event.PhaseName == "Blocked" {
			t.Fatalf("workflow event = %#v, want no breaker transition", event)
		}
	}
	if comments := tracker.commentCalls(); len(comments) != 0 {
		t.Fatalf("comments = %#v, want no breaker explanation", comments)
	}
}

func TestRunDispatchTransitionsTemplateVocabularyIssueToProduction(t *testing.T) {
	t.Parallel()

	issue := testIssue("issue-production-transition", "wi-local-video-1", "Todo")
	tracker := newFakeConnector(issue)
	runner := &staticRunner{
		result: orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted},
	}

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:           time.Millisecond,
		MaxConcurrentAgents:    1,
		MaxRetryBackoff:        time.Hour,
		FailureRetryBaseDelay:  time.Hour,
		ActiveStates:           []string{"Todo", "Production", "Rework"},
		ObservedStates:         []string{"Backlog", "Review", "Blocked"},
		TerminalStates:         []string{"Ready for Pickup", "Done", "Cancelled"},
		ContinuationRetryDelay: time.Millisecond,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	waitForStateUpdate(t, tracker, stateUpdateCall{issueID: issue.ID, state: "Production"})
	updates := tracker.stateUpdateCalls()
	if len(updates) == 0 || updates[0] != (stateUpdateCall{issueID: issue.ID, state: "Production"}) {
		t.Fatalf("state updates = %#v, want Production as the dispatch-start transition", updates)
	}
}
