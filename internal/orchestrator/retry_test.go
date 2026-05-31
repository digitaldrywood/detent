package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
	runpkg "github.com/digitaldrywood/symphony/internal/runner"
)

func TestDispatchReadyIssuesKeepsRetryAttemptWhenCapacityIsFull(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		MaxRetryBackoff:       time.Minute,
		FailureRetryBaseDelay: time.Second,
		ActiveStates:          []string{"Todo"},
		TerminalStates:        []string{"Done"},
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	now := time.Now()
	running := retryTestIssue("running", "digitaldrywood/symphony#20")
	retrying := retryTestIssue("retrying", "digitaldrywood/symphony#21")

	state.Running[running.ID] = Running{Issue: running, StartedAt: now}
	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:   retrying,
		Attempt: 2,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "previous failure",
	}

	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{retrying}, now)

	retry, ok := state.Retry[retrying.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing after capacity refusal", retrying.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", retrying.ID, retry.Attempt)
	}
	if retry.Error != "no available orchestrator slots" {
		t.Fatalf("Retry[%q].Error = %q, want no available orchestrator slots", retrying.ID, retry.Error)
	}
	if !retry.DueAt.After(now) {
		t.Fatalf("Retry[%q].DueAt = %s, want after %s", retrying.ID, retry.DueAt, now)
	}
}

func TestApplyRuntimeUpdateRefreshesSupervisorRetryConfig(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		PollInterval:           time.Hour,
		MaxConcurrentAgents:    1,
		MaxRetryBackoff:        time.Minute,
		FailureRetryBaseDelay:  10 * time.Second,
		ActiveStates:           []string{"Todo"},
		TerminalStates:         []string{"Done"},
		ContinuationRetryDelay: time.Second,
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
	}
	state := newState(cfg)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	orch.applyRuntimeUpdate(&state, RuntimeUpdate{
		Config: Config{
			PollInterval:          time.Hour,
			MaxConcurrentAgents:   1,
			MaxRetryBackoff:       2 * time.Second,
			FailureRetryBaseDelay: time.Second,
			ActiveStates:          []string{"Todo"},
			TerminalStates:        []string{"Done"},
		},
	}, ticker)

	if got := orch.supervisor.RetryDelay(4); got != 2*time.Second {
		t.Fatalf("RetryDelay(4) = %s, want reloaded 2s cap", got)
	}
}

func TestDispatchReadyIssuesRanksDueRetriesWithCandidates(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:     1,
		DispatchPriorityByState: []string{"Merging"},
		ActiveStates:            []string{"Todo", "Merging"},
		TerminalStates:          []string{"Done"},
	})
	orch := Orchestrator{
		cfg:        cfg,
		supervisor: newTestSupervisor(t, FakeRunner{}, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	retrying := retryTestIssue("retrying", "digitaldrywood/symphony#21")
	merging := retryTestIssue("merging", "digitaldrywood/symphony#22")
	merging.State = "Merging"
	priority := 4
	merging.Priority = &priority

	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:   retrying,
		Attempt: 2,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "previous failure",
	}

	issues := []connector.Issue{retrying, merging}
	sortIssuesForDispatch(issues, cfg.DispatchPriorityByState)
	orch.dispatchReadyIssues(context.Background(), &state, issues, now)

	if _, ok := state.Running[merging.ID]; !ok {
		t.Fatalf("Running[%q] missing", merging.ID)
	}
	if _, ok := state.Running[retrying.ID]; ok {
		t.Fatalf("Running[%q] present", retrying.ID)
	}
	if retry, ok := state.Retry[retrying.ID]; !ok {
		t.Fatalf("Retry[%q] missing", retrying.ID)
	} else if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", retrying.ID, retry.Attempt)
	}
}

func TestDispatchReadyIssuesKeepsAttemptWhenWorkerCapacityIsFull(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:        2,
		MaxRetryBackoff:            time.Minute,
		FailureRetryBaseDelay:      time.Second,
		ActiveStates:               []string{"Todo"},
		TerminalStates:             []string{"Done"},
		WorkerHosts:                []string{"worker-a"},
		MaxConcurrentAgentsPerHost: 1,
	})
	orch := Orchestrator{cfg: cfg}
	state := newState(cfg)
	now := time.Now()
	running := retryTestIssue("running", "digitaldrywood/symphony#20")
	retrying := retryTestIssue("retrying", "digitaldrywood/symphony#21")

	state.Running[running.ID] = Running{Issue: running, StartedAt: now, WorkerHost: "worker-a"}
	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:      retrying,
		Attempt:    2,
		DueAt:      now.Add(-time.Millisecond),
		Error:      "previous failure",
		WorkerHost: "worker-a",
	}

	orch.dispatchReadyIssues(context.Background(), &state, []connector.Issue{retrying}, now)

	retry, ok := state.Retry[retrying.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing after worker capacity refusal", retrying.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", retrying.ID, retry.Attempt)
	}
	if retry.WorkerHost != "worker-a" {
		t.Fatalf("Retry[%q].WorkerHost = %q, want worker-a", retrying.ID, retry.WorkerHost)
	}
}

func retryTestIssue(id, identifier string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = identifier
	issue.Title = "Retry issue"
	issue.State = "Todo"
	return issue
}
