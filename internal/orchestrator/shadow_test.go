package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/symphony/internal/connector"
)

func TestPlanDispatchReportsReadOnlyDispatchOrder(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		MaxConcurrentAgents:        2,
		ActiveStates:               []string{"Todo"},
		TerminalStates:             []string{"Done"},
		WorkerHosts:                []string{"worker-a", "worker-b"},
		MaxConcurrentAgentsPerHost: 1,
	}
	state := newState(normalizeConfig(cfg))
	running := shadowTestIssue("issue-running", "Todo", nil, now.Add(-3*time.Hour))
	state.Running[running.ID] = Running{Issue: running, WorkerHost: "worker-a"}

	highPriority := 1
	lowPriority := 4
	candidates := []connector.Issue{
		shadowTestIssue("issue-low", "Todo", &lowPriority, now.Add(-4*time.Hour)),
		shadowTestIssue("issue-high", "Todo", &highPriority, now.Add(-time.Hour)),
	}

	plan := PlanDispatch(cfg, state, candidates, now)

	if len(plan.Dispatches) != 1 {
		t.Fatalf("Dispatches length = %d, want 1", len(plan.Dispatches))
	}
	dispatch := plan.Dispatches[0]
	if dispatch.IssueID != "issue-high" {
		t.Fatalf("Dispatches[0].IssueID = %q, want issue-high", dispatch.IssueID)
	}
	if dispatch.WorkerHost != "worker-b" {
		t.Fatalf("Dispatches[0].WorkerHost = %q, want worker-b", dispatch.WorkerHost)
	}
	if _, ok := state.Running["issue-high"]; ok {
		t.Fatal("PlanDispatch mutated input state running set")
	}
	if len(plan.Claimed) != 1 || plan.Claimed[0] != "issue-high" {
		t.Fatalf("Claimed = %#v, want issue-high", plan.Claimed)
	}
}

func TestPlanDispatchRetriesDueIssueWithoutStartingRunner(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
	}
	state := newState(normalizeConfig(cfg))
	retryIssue := shadowTestIssue("issue-retry", "Todo", nil, now.Add(-time.Hour))
	state.Retry[retryIssue.ID] = Retry{
		Issue:   retryIssue,
		Attempt: 2,
		DueAt:   now.Add(-time.Minute),
		Error:   "previous failure",
	}
	state.Claimed[retryIssue.ID] = Claimed{Issue: retryIssue, ClaimedAt: now.Add(-2 * time.Minute)}

	plan := PlanDispatch(cfg, state, []connector.Issue{retryIssue}, now)

	if len(plan.Dispatches) != 1 {
		t.Fatalf("Dispatches length = %d, want 1", len(plan.Dispatches))
	}
	if plan.Dispatches[0].IssueID != retryIssue.ID || plan.Dispatches[0].Attempt != 2 {
		t.Fatalf("Dispatches[0] = %#v, want retry attempt 2", plan.Dispatches[0])
	}
	if len(plan.Retry) != 0 {
		t.Fatalf("Retry = %#v, want empty after retry dispatch", plan.Retry)
	}
}

func shadowTestIssue(id, state string, priority *int, createdAt time.Time) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/symphony#" + id
	issue.Title = "Shadow test issue"
	issue.State = state
	issue.Priority = priority
	issue.CreatedAt = &createdAt
	issue.AssignedToWorker = true
	return issue
}
