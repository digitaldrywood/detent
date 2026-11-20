package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestRetireDeferredStandalonePlanAfterRestart(t *testing.T) {
	t.Parallel()
	now := time.Now()
	issue := completionDeferralIssue("legacy-draft", "In Progress")
	cfg := completionDeferralConfig()
	cfg.Plan.Enabled = true
	cfg.ActiveStates = []string{"Todo", "In Progress"}
	conn := &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, states: []connector.WorkflowState{
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true},
	}}
	runtime := newRuntimeFixture()
	id := startCompletionDeferralAttempt(t, runtime, issue, now)
	running := completionDeferralRunning(issue, id, now)
	running.Mode = runpkg.RunModePlan
	event := completionDeferralEvent(issue, id, now)
	event.Request.Mode = runpkg.RunModePlan
	const draft = "Keep  repeated spaces.\n\nCover the empty input."
	event.Result = runpkg.RunResult{Output: draft, FinalState: runpkg.FinalStateCompleted, TurnStarted: true}
	record := newDeferredCompletion(event, running, nil, now)
	record.FenceRetryAt = now
	scheduling := &completionRestartScheduling{hubSchedulingSource: &hubSchedulingSource{}, restoreError: runpkg.ErrExecutionAuthorityUnavailable}
	orch := Orchestrator{cfg: cfg, connector: conn, workAttempts: runtime, scheduling: scheduling, now: func() time.Time { return now }}
	before := newState(cfg)
	if !orch.persistDeferredCompletion(t.Context(), &before, record) {
		t.Fatal("could not persist the standalone result")
	}
	attempt, err := runtime.WorkAttempt(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newState(cfg)
	orch.recoverDeferredCompletions(t.Context(), &restarted, []store.WorkAttempt{attempt}, now)
	if !orch.retryDeferredCompletion(t.Context(), &restarted, issue.ID, now) {
		t.Fatal("the old lease still holds the item")
	}
	after, err := runtime.WorkAttempt(t.Context(), id)
	if err != nil || after.Status != store.WorkAttemptStatusTerminal || after.TerminalState != store.WorkAttemptTerminalAbandoned {
		t.Fatalf("standalone attempt not retired: %+v, %v", after, err)
	}
	if _, retained := restarted.deferredCompletions[issue.ID]; retained {
		t.Fatal("legacy draft retained after retirement")
	}
	if _, claimed := restarted.Claimed[issue.ID]; claimed {
		t.Fatal("the old fence still owns the item")
	}
}
