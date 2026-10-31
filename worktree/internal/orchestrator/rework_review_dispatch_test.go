package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestReworkLiveReviewGateDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*connector.PullRequest)
		wait   bool
	}{
		{name: "question waits without completion evidence", wait: true},
		{name: "current head review", mutate: func(pr *connector.PullRequest) { pr.CodexReviewState = "COMMENTED" }},
		{name: "no established review cycle", mutate: func(pr *connector.PullRequest) { pr.LatestCodexReviewState = "" }},
		{name: "conflicts", mutate: func(pr *connector.PullRequest) { pr.MergeableState = "dirty" }},
		{name: "unknown mergeability", mutate: func(pr *connector.PullRequest) { pr.MergeableState = "unknown" }},
		{name: "failed CI", mutate: func(pr *connector.PullRequest) { pr.CIStatus = "failure" }},
		{name: "failed required check", mutate: func(pr *connector.PullRequest) {
			pr.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "Verify", Conclusion: "failure"}}
		}},
		{name: "unresolved thread", mutate: func(pr *connector.PullRequest) { pr.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{}} }},
		{name: "draft", mutate: func(pr *connector.PullRequest) { pr.Draft = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}}})
			issue := dispatchTestIssueWithPullRequest("2747", "Rework", "OPEN")
			issue.PullRequest.HeadSHA = "current-head"
			issue.PullRequest.MergeableState = "clean"
			issue.PullRequest.CIStatus = "success"
			issue.PullRequest.LatestCodexReviewState = "COMMENTED"
			if tt.mutate != nil {
				tt.mutate(issue.PullRequest)
			}
			// Question-wait completion releases claims and retries without recording
			// Completed evidence. After the answer, every scheduler pass sees this state.
			planner := newDispatchPlanner(cfg)
			answered := time.Date(2026, 9, 16, 13, 58, 0, 0, time.UTC)
			for _, elapsed := range []time.Duration{0, 9 * time.Minute, 13 * time.Minute, 23 * time.Minute} {
				state := newState(cfg)
				var reason string
				plan := planner.plan(&state, []connector.Issue{issue}, answered.Add(elapsed), dispatchPlanHooks{decision: func(d dispatchPlanDecision) { reason = d.SkipReason }})
				if tt.wait {
					if len(plan.DispatchOrder()) != 0 || reason != dispatchSkipAwaitingGate {
						t.Fatalf("at %s: dispatch=%v reason=%q, want awaiting_gate", elapsed, plan.DispatchOrder(), reason)
					}
				} else if len(plan.DispatchOrder()) != 1 {
					t.Fatalf("dispatch=%v reason=%q, want eligible", plan.DispatchOrder(), reason)
				}
			}
		})
	}
}
