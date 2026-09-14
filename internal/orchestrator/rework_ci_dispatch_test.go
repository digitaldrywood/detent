package orchestrator

import (
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestReworkCurrentHeadCIDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		pr   *connector.PullRequest
		wait bool
	}{
		{name: "no PR"},
		{name: "no checks", pr: &connector.PullRequest{State: "OPEN"}},
		{name: "pending", pr: &connector.PullRequest{State: "OPEN", CIStatus: "pending"}, wait: true},
		{name: "queued", pr: &connector.PullRequest{State: "OPEN", CIStatus: "queued"}, wait: true},
		{name: "running", pr: &connector.PullRequest{State: "OPEN", CIStatus: "running"}, wait: true},
		{name: "in progress", pr: &connector.PullRequest{State: "OPEN", CIStatus: "in_progress"}, wait: true},
		{name: "waiting", pr: &connector.PullRequest{State: "OPEN", CIStatus: "waiting"}, wait: true},
		{name: "success", pr: &connector.PullRequest{State: "OPEN", CIStatus: "success"}},
		{name: "failure", pr: &connector.PullRequest{State: "OPEN", CIStatus: "failure"}},
		{name: "cancelled", pr: &connector.PullRequest{State: "OPEN", CIStatus: "cancelled"}},
		{name: "skipped", pr: &connector.PullRequest{State: "OPEN", CIStatus: "skipped"}},
		{name: "failed with running check", pr: &connector.PullRequest{State: "OPEN", CIStatus: "failure", RunningChecks: []string{"Verify"}}, wait: true},
		{name: "queued check", pr: &connector.PullRequest{State: "OPEN", UnstartedChecks: []connector.PullRequestCheck{{Name: "Verify", Status: "queued"}}}, wait: true},
		{name: "required check running", pr: &connector.PullRequest{State: "OPEN", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "Verify", Status: "in_progress"}}}, wait: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, retry := range []bool{false, true} {
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Rework", "Todo"}, TerminalStates: []string{"Done"}, DispatchPriorityByState: []string{"Rework", "Todo"}})
				now := time.Now()
				state := newState(cfg)
				issue := dispatchTestIssue("rework", "Rework")
				issue.PullRequest = tt.pr
				next := dispatchTestIssue("next", "Todo")
				if retry {
					state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2, DueAt: now}
				}
				var reason string
				planner := newDispatchPlanner(cfg)
				plan := planner.plan(&state, []connector.Issue{issue, next}, now, dispatchPlanHooks{decision: func(d dispatchPlanDecision) {
					if d.Issue.ID == issue.ID {
						reason = d.SkipReason
					}
				}})
				want := issue.ID
				if tt.wait {
					want = next.ID
				}
				if !slices.Equal(plan.DispatchOrder(), []string{want}) {
					t.Fatalf("retry=%v dispatch order=%v, want %s", retry, plan.DispatchOrder(), want)
				}
				if tt.wait && reason != dispatchSkipCurrentHeadCIWait {
					t.Fatalf("retry=%v reason=%q", retry, reason)
				}
				if tt.wait {
					if retry && state.Retry[issue.ID].Attempt != 2 {
						t.Fatal("CI wait lost retry attempt")
					}
					issue.PullRequest = &connector.PullRequest{State: "OPEN", CIStatus: "success"}
					delete(state.Running, next.ID)
					delete(state.Claimed, next.ID)
					plan = planner.plan(&state, []connector.Issue{issue}, now.Add(time.Second), dispatchPlanHooks{})
					if !slices.Equal(plan.DispatchOrder(), []string{issue.ID}) {
						t.Fatalf("terminal CI did not release Rework: %v", plan.DispatchOrder())
					}
				}
			}
		})
	}
}

func TestReworkCurrentHeadCIConfiguredLane(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		lane string
		wait bool
	}{
		{"Repair", true}, {"Rework", false}, {"In Progress", false},
	} {
		t.Run(tt.lane, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Repair", "Rework", "In Progress"}, AutoPromote: AutoPromoteConfig{ReworkState: "Repair"}})
			state := newState(cfg)
			issue := dispatchTestIssue("issue", tt.lane)
			issue.PullRequest = &connector.PullRequest{State: "OPEN", CIStatus: "queued"}
			decision := newDispatchPlanner(cfg).dispatchableIssueDecision(issue, &state, false, time.Now(), "")
			if decision.dispatchable == tt.wait {
				t.Fatalf("dispatchable=%v, wait=%v, reason=%s", decision.dispatchable, tt.wait, decision.reason)
			}
		})
	}
}
