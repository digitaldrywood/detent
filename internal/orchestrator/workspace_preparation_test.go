package orchestrator

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestWorkspacePreparationFailuresDoNotTripWorkFailureBreakers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "dangling gitdir",
			err:  fmt.Errorf("%w: create workspace: dangling gitdir", runpkg.ErrWorkspacePreparation),
		},
		{
			name: "stale clean recovery",
			err:  fmt.Errorf("%w: create workspace: recover stale clean workspace", runpkg.ErrWorkspacePreparation),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{
				ActiveStates:   []string{"In Progress"},
				ObservedStates: []string{"Blocked"},
				TerminalStates: []string{"Done"},
				FailureBreaker: FailureBreakerConfig{
					SameClassLimit: repeatedFailureThreshold,
					Window:         time.Hour,
					Cooldown:       time.Hour,
				},
			})
			orch := &Orchestrator{cfg: cfg}
			state := newState(cfg)
			issue := connector.Issue{ID: "issue-workspace", Identifier: "digitaldrywood/detent#1907", State: "In Progress"}
			base := time.Date(2026, 8, 18, 20, 0, 0, 0, time.UTC)

			for attempt := 1; attempt <= repeatedFailureThreshold; attempt++ {
				completedAt := base.Add(time.Duration(attempt) * time.Minute)
				running := Running{
					Issue:     issue,
					Mode:      runpkg.RunModePlan,
					Attempt:   attempt,
					StartedAt: completedAt.Add(-time.Minute),
				}
				state.Running[issue.ID] = running
				orch.handleRunResult(t.Context(), &state, runpkg.Completion{
					IssueID:      issue.ID,
					Request:      runpkg.RunRequest{Mode: runpkg.RunModePlan},
					Err:          tt.err,
					CompletedAt:  completedAt,
					RetryAttempt: attempt + 1,
					RetryDelay:   time.Second,
				})
			}

			if _, blocked := state.Blocked[issue.ID]; blocked {
				t.Fatalf("Blocked[%q] present after workspace preparation failures", issue.ID)
			}
			if _, ok := state.Retry[issue.ID]; !ok {
				t.Fatalf("Retry[%q] missing after workspace preparation failure", issue.ID)
			}
			if _, ok := state.InstantFailures[issue.ID]; ok {
				t.Fatalf("InstantFailures[%q] present after workspace preparation failure", issue.ID)
			}
			if _, ok := state.RepeatedFailures[issue.ID]; ok {
				t.Fatalf("RepeatedFailures[%q] present after workspace preparation failure", issue.ID)
			}
			if state.FailureBreaker.Class != workAttemptErrorWorkspace {
				t.Fatalf("FailureBreaker.Class = %q, want %q", state.FailureBreaker.Class, workAttemptErrorWorkspace)
			}
			if event, ok := recentStateEvent(state, "workspace_repair_retry_scheduled"); !ok || event.Message == "" {
				t.Fatalf("RecentEvents = %#v, want workspace repair retry event", state.RecentEvents)
			}
		})
	}
}
