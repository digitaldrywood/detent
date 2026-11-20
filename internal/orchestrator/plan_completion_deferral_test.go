package orchestrator

import (
	"testing"

	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"

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

func TestDeferredStandalonePlanSurvivesRestartAndFenceLoss(t *testing.T) {
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
	for range 2 {
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
		retained := restarted.deferredCompletions[issue.ID]
		if err != nil || retained.Result.Output != draft || retained.Running.Issue.State != "In Progress" || after.Status != store.WorkAttemptStatusActive {
			t.Fatalf("draft or dispatchable lane lost before replacement: draft=%q lane=%q old attempt=%s err=%v", retained.Result.Output, retained.Running.Issue.State, after.Status, err)
		}
		if _, claimed := restarted.Claimed[issue.ID]; claimed {
			t.Fatal("the old fence still owns the item")
		}
	}
}
