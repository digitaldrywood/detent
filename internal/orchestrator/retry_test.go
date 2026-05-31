package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/connector"
)

func TestDispatchDueRetriesKeepsAttemptWhenCapacityIsFull(t *testing.T) {
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
	running := retryTestIssue("running", "digitaldrywood/symphony-go#20")
	retrying := retryTestIssue("retrying", "digitaldrywood/symphony-go#21")

	state.Running[running.ID] = Running{Issue: running, StartedAt: now}
	state.Claimed[retrying.ID] = Claimed{Issue: retrying, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[retrying.ID] = Retry{
		Issue:   retrying,
		Attempt: 2,
		DueAt:   now.Add(-time.Millisecond),
		Error:   "previous failure",
	}

	orch.dispatchDueRetries(context.Background(), &state, []connector.Issue{retrying}, now)

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

func retryTestIssue(id, identifier string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = identifier
	issue.Title = "Retry issue"
	issue.State = "Todo"
	return issue
}
