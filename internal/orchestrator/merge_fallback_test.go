package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestMergeFallbackRoutesBoundedOutcomesToRework(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		result            runpkg.RunResult
		runErr            error
		wantReason        string
		wantTerminalState store.WorkAttemptTerminalState
	}{
		{
			name: "structured review finding",
			result: runpkg.RunResult{
				FinalState:            runpkg.FinalStateCompleted,
				Output:                runpkg.RunOutputMergeFallbackRework,
				MergeFallbackFindings: "Found an unrelated authorization defect and stopped.",
			},
			wantReason:        mergeFallbackRequiresReworkReason,
			wantTerminalState: store.WorkAttemptTerminalSuccess,
		},
		{
			name: "fallback budget exceeded",
			result: runpkg.RunResult{
				FinalState: runpkg.FinalStateMergeFallbackExceeded,
				Output:     "Conflict resolved; validation still running.",
			},
			runErr:            errors.Join(runpkg.ErrMergeFallbackBudgetExceeded, context.DeadlineExceeded),
			wantReason:        mergeFallbackBudgetExceededReason,
			wantTerminalState: store.WorkAttemptTerminalTimedOut,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)
			issue := connector.Issue{
				ID:           "issue-1809-" + strings.ReplaceAll(tt.name, " ", "-"),
				Identifier:   "digitaldrywood/detent#1809",
				State:        "Merging",
				PRRepository: "digitaldrywood/detent",
				PullRequest: &connector.PullRequest{
					Number:         1810,
					URL:            "https://github.test/digitaldrywood/detent/pull/1810",
					State:          "OPEN",
					MergeableState: "dirty",
					HeadSHA:        "conflicted-head",
				},
			}
			tracker := &autoPromoteTickMergeConnector{
				autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
			}
			cfg := normalizeConfig(Config{
				ActiveStates:   []string{"Rework", "Merging"},
				ObservedStates: []string{"Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			attempts := &recordingWorkAttemptStore{}
			orch := &Orchestrator{
				cfg:          cfg,
				connector:    tracker,
				workAttempts: attempts,
				logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			state := newState(cfg)
			state.Running[issue.ID] = Running{
				Issue:         cloneIssue(issue),
				Attempt:       1,
				WorkAttemptID: 1809,
				StartedAt:     now.Add(-20 * time.Minute),
				Mode:          runpkg.RunModeMerge,
			}
			state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-20 * time.Minute)}

			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID:     issue.ID,
				CompletedAt: now,
				Request:     runpkg.RunRequest{Mode: runpkg.RunModeMerge},
				Result:      tt.result,
				Err:         tt.runErr,
			})

			if len(tracker.updates) != 1 || tracker.updates[0].state != "Rework" {
				t.Fatalf("state updates = %#v, want one Rework transition", tracker.updates)
			}
			if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, tt.wantReason) {
				t.Fatalf("comments = %#v, want reason %q", tracker.comments, tt.wantReason)
			}
			if !strings.Contains(tracker.comments[0].body, "authorization defect") &&
				!strings.Contains(tracker.comments[0].body, "validation still running") {
				t.Fatalf("comment = %q, want preserved merge-fallback findings", tracker.comments[0].body)
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != tt.wantTerminalState {
				t.Fatalf("attempt completions = %#v, want terminal state %q", attempts.completions, tt.wantTerminalState)
			}
			if _, ok := state.Retry[issue.ID]; ok {
				t.Fatalf("Retry[%q] present after Rework handoff", issue.ID)
			}
			if _, ok := state.Claimed[issue.ID]; ok {
				t.Fatalf("Claimed[%q] present after Rework handoff", issue.ID)
			}
		})
	}
}
