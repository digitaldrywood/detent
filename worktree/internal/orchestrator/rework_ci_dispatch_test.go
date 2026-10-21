package orchestrator

import (
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestReworkCurrentHeadCIDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		lane        string
		localStatus string
		pr          *connector.PullRequest
		wait        bool
	}{
		{name: "no PR"},
		{name: "Rework missing owned status", localStatus: "local-gate", pr: &connector.PullRequest{State: "OPEN", CIStatus: "success", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}}}},
		{name: "Todo missing owned status", lane: "Todo", localStatus: "local-gate", pr: &connector.PullRequest{State: "OPEN", CIStatus: "success", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}}}},
		{name: "Merging missing owned status", lane: "Merging", localStatus: "local-gate", pr: &connector.PullRequest{State: "OPEN", MergeableState: "blocked", CIStatus: "success", Number: 42, HeadSHA: "head", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}}}},
		{name: "missing external status still waits", localStatus: "local-gate", pr: &connector.PullRequest{State: "OPEN", CIStatus: "pending", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "Verify", Status: "missing", Conclusion: "missing"}}}, wait: true},
		{name: "missing status without ownership still waits", pr: &connector.PullRequest{State: "OPEN", CIStatus: "pending", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}}}, wait: true},
		{name: "missing local with running external check", localStatus: "local-gate", pr: &connector.PullRequest{State: "OPEN", CIStatus: "pending", RunningChecks: []string{"Verify"}, RequiredCheckFailures: []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}}}, wait: true},
		{name: "no checks", pr: &connector.PullRequest{State: "OPEN"}},
		{name: "closed pending status", pr: &connector.PullRequest{State: "CLOSED", CIStatus: "pending"}},
		{name: "closed unstarted check", pr: &connector.PullRequest{State: "CLOSED", UnstartedChecks: []connector.PullRequestCheck{{Name: "Verify", Status: "queued"}}}},
		{name: "closed pending required check", pr: &connector.PullRequest{State: "CLOSED", RequiredCheckFailures: []connector.PullRequestCheck{{Name: "Verify", Status: "in_progress"}}}},
		{name: "unknown state pending status", pr: &connector.PullRequest{CIStatus: "pending"}, wait: true},
		{name: "unrecognized state pending status", pr: &connector.PullRequest{State: "UNKNOWN", CIStatus: "pending"}, wait: true},
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
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Rework", "Todo", "Merging"}, AutoPromote: AutoPromoteConfig{Gate: gate.Config{LocalStatus: tt.localStatus}}, TerminalStates: []string{"Done"}, DispatchPriorityByState: []string{"Rework", "Todo"}})
				now := time.Now()
				state := newState(cfg)
				lane := tt.lane
				if lane == "" {
					lane = "Rework"
				}
				issue := dispatchTestIssue("rework", lane)
				issue.Priority = new(1)
				issue.PullRequest = tt.pr
				next := dispatchTestIssue("next", "Todo")
				next.Priority = new(2)
				if retry {
					state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2, DueAt: now}
				}

				if lane == "Todo" {
					tracker := &autoPromoteTickConnector{}
					orch := &Orchestrator{cfg: cfg, connector: tracker}
					if moved := orch.reconcileStaleLinkedPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, now); len(moved) != 0 || len(tracker.updates) != 0 {
						t.Fatal("unproduced local status incorrectly treated as stale Todo work")
					}
				}
				var reason string
				planner := newDispatchPlanner(cfg)
				candidates := []connector.Issue{issue, next}
				if tt.lane != "" {
					candidates = []connector.Issue{issue}
				}
				plan := planner.plan(&state, candidates, now, dispatchPlanHooks{decision: func(d dispatchPlanDecision) {
					if d.Issue.ID == issue.ID {
						reason = d.SkipReason
					}
				}})
				want := issue.ID
				if tt.wait {
					want = next.ID
				}
				if !slices.Equal(plan.DispatchOrder(), []string{want}) {
					t.Fatalf("retry=%v dispatch order=%v, want %s (reason=%s)", retry, plan.DispatchOrder(), want, reason)
				}
				if !tt.wait && reason == dispatchSkipCurrentHeadCIWait {
					t.Fatalf("retry=%v unexpectedly waited on CI", retry)
				}
				if lane == "Merging" {
					if missing := mergeWorkerMissingRequiredChecks(issue, tt.localStatus); len(missing) != 0 {
						t.Fatalf("owned status entered missing-check streak: %v", missing)
					}
					if mergeWorkerProgrammaticMergeWaiting(issue) {
						t.Fatal("owned status entered merge CI wait")
					}
					if mergeWorkerProgrammaticMergeReady(issue, cfg) {
						t.Fatal("unproduced local gate allowed merge before validation")
					}
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
