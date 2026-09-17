package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestFirstHumanBlockerCompletionReachesBlocked(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		linkedPR bool
		action   string
	}{
		{name: "no PR human action", action: "Approve the hardware check."},
		{name: "linked PR human action", linkedPR: true, action: "Approve the hardware check."},
		{name: "no PR human owner"},
		{name: "linked PR human owner", linkedPR: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 16, 0, 25, 0, 0, time.UTC)
			issue := implementProgressIssueWithoutPR()
			if tt.linkedPR {
				issue = implementProgressIssue("head")
			}
			issue.State = "Rework"
			issue.AssignedToWorker = true
			current := cloneIssue(issue)
			body := implementProgressWorkpadComment("", tt.action)
			if tt.action == "" {
				body = "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - reason: Approve the hardware check.\n    owner: human\nhuman_action: null\n```"
			}
			current.Comments = []connector.IssueComment{{Body: body}}
			tracker := &implementProgressConnector{refreshed: current, hydrated: current}
			attempts := &implementProgressAttemptStore{}
			cfg := normalizeConfig(Config{
				Project:             scheduler.ProjectCandidate{ID: "detent"},
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress", "Rework"},
				ObservedStates:      []string{"Blocked", "Human Review"},
				TerminalStates:      []string{"Done"},
				AutoPromote:         AutoPromoteConfig{NoProgressLimit: 3},
			})
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, workflowMetrics: metrics}
			// The newly reported Workpad already prevents the second dispatch that the
			// old completion threshold required.
			preview := newState(cfg)
			decision := orch.liveDispatchPlanner(t.Context()).dispatchableIssueDecision(current, &preview, false, now, "")
			if decision.dispatchable || decision.reason != dispatchSkipBlockedByDependency {
				t.Fatalf("next dispatch = %+v, want recorded blocker suppression", decision)
			}
			state := newState(cfg)
			diff := DiffStats{Status: "clean", HeadSHA: "head"}
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 42, Mode: runpkg.RunModeImplement, StartedAt: now.Add(-time.Minute), DiffStats: diff}
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement},
				Result:  runpkg.RunResult{FinalState: FinalStateCompleted, DiffStats: diff},
			})
			if len(tracker.updates) != 1 || tracker.updates[0].state != "Blocked" {
				t.Fatalf("first completion lane writes = %+v; next dispatch is suppressed, so no second completion can repair the lane", tracker.updates)
			}
			if len(metrics.events) == 0 {
				t.Fatal("missing lane transition audit")
			}
			if reset := lastAllowanceOperatorMoveAt(metrics.events); !reset.IsZero() {
				t.Fatalf("automated Blocked move renewed attempt allowance at %s", reset)
			}
			blocked := state.Blocked[issue.ID]
			if blocked.Recovery == nil || blocked.Recovery.Owner != blockedRecoveryOwnerHuman || blocked.RecoveryReason != "Approve the hardware check." {
				t.Fatalf("human recovery = %+v", blocked)
			}
			if len(state.Retry) != 0 || len(state.Claimed) != 0 {
				t.Fatalf("blocked issue retained retry or claim")
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalNoProgress {
				t.Fatalf("completions = %+v", attempts.completions)
			}
		})
	}
}
