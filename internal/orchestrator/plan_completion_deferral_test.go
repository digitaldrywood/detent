package orchestrator

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestDeferredStandalonePlanRetriesInsideClaim(t *testing.T) {
	t.Parallel()
	now := time.Now()
	issue := completionDeferralIssue("legacy-plan", "In Progress")
	cfg := completionDeferralConfig()
	cfg.ActiveStates = []string{"Todo", "In Progress"}
	cfg.AutoPromote = AutoPromoteConfig{SourceState: "Human Review"}
	tracker := &nativeWorkflowConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, states: []connector.WorkflowState{
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review"}},
		{Name: "Todo", Dispatchable: true}, {Name: "Human Review"},
	}}
	runtime := newRuntimeFixture()
	id := startCompletionDeferralAttempt(t, runtime, issue, now)
	orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: runtime, now: func() time.Time { return now }}
	state := newState(cfg)
	running := completionDeferralRunning(issue, id, now)
	running.Mode, running.Generation = runpkg.RunModePlan, 0
	event := completionDeferralEvent(issue, id, now)
	event.Request.Mode, event.Request.Generation = runpkg.RunModePlan, 0
	event.Result = runpkg.RunResult{Output: "Stored standalone plan", FinalState: runpkg.FinalStateCompleted, TurnStarted: true}
	record := newDeferredCompletion(event, running, nil, now)
	record.AuthorityRestored, record.Persisted = true, true
	state.deferredCompletions[issue.ID] = record
	if !orch.retryDeferredCompletion(t.Context(), &state, issue.ID, now) {
		t.Fatal("standalone completion was deferred again")
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Issue.State != "Todo" {
		t.Fatalf("standalone plan stranded after settlement: retry=%+v", state.Retry)
	}
}
