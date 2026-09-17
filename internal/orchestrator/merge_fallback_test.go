package orchestrator

import (
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
		lane              string
		result            runpkg.RunResult
		runErr            error
		wantReason        string
		wantTerminalState store.WorkAttemptTerminalState
	}{
		{
			name: "structured review finding",
			lane: "Merging",
			result: runpkg.RunResult{
				FinalState:            runpkg.FinalStateCompleted,
				Output:                runpkg.RunOutputMergeFallbackRework,
				MergeFallbackFindings: "Found an unrelated authorization defect and stopped.",
			},
			wantReason:        mergeFallbackRequiresReworkReason,
			wantTerminalState: store.WorkAttemptTerminalSuccess,
		},
	}

	for _, lane := range []string{"Rework", "In Progress"} {
		test := tests[0]
		test.name = lane + " review finding"
		test.lane = lane
		tests = append(tests, test)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)
			issue := connector.Issue{
				ID:           "issue-1809-" + strings.ReplaceAll(tt.name, " ", "-"),
				Identifier:   "digitaldrywood/detent#1809",
				State:        tt.lane,
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

func TestMergeFallbackResolvedHeadHandoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		lane       string
		head       string
		ci         string
		wantRework bool
	}{
		{lane: "Merging", name: "resolved pushed head waits past resolution deadline", head: "validated-head", ci: "pending"},
		{lane: "Merging", name: "validation failure", head: "validated-head", ci: "failure", wantRework: true},
		{lane: "Merging", name: "replaced head with green CI", head: "replacement-head", ci: "success", wantRework: true},
	}
	for _, lane := range []string{"Rework", "In Progress"} {
		resolved := tests[0]
		resolved.name, resolved.lane, resolved.ci = lane+" resolved", lane, "success"
		replaced := tests[2]
		replaced.name, replaced.lane = lane+" replaced head", lane
		tests = append(tests, resolved, replaced)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 7, 22, 2, 24, 0, time.UTC)
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1, MergeFastPathEnabled: true,
				ActiveStates: []string{"In Progress", "Rework", "Merging"}, ObservedStates: []string{"Merging"}, TerminalStates: []string{"Done"},
			})
			issue := connector.Issue{
				ID: "issue-2273", Identifier: "digitaldrywood/detent#2273", State: tt.lane, PRRepository: "digitaldrywood/detent",
				PullRequest: &connector.PullRequest{
					Number: 2274, State: "OPEN", MergeableState: "clean", HeadSHA: tt.head, CIStatus: tt.ci,
				},
			}
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			dispatched := cloneIssue(issue)
			dispatched.PullRequest.HeadSHA = "conflicted-head"
			dispatched.PullRequest.MergeableState = "dirty"
			previous := autoPromoteReworkSignatureFromIssue(dispatched, AutoPromoteSummaryFromIssue(dispatched))
			attempts := &recordingWorkAttemptStore{history: []store.WorkAttempt{implementProgressHistoryAttempt(1, previous, store.WorkAttemptTerminalSuccess)}}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			state.Running[issue.ID] = Running{Issue: dispatched, WorkAttemptID: 2, Attempt: 1, Mode: runpkg.RunModeMerge, StartedAt: now.Add(-21 * time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-21 * time.Minute)}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge},
				Result: runpkg.RunResult{
					FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFallbackResolved,
					MergePrecheck: &runpkg.MergePrecheck{Status: "clean", HeadSHA: "validated-head"},
				},
			})
			if len(tracker.merges) != 0 || len(state.Running) != 0 || len(state.Claimed) != 0 {
				t.Fatalf("handoff retained execution or merged: merges=%v running=%v claimed=%v", tracker.merges, state.Running, state.Claimed)
			}
			if tt.wantRework {
				if len(tracker.updates) != 1 || tracker.updates[0].state != "Rework" {
					t.Fatalf("updates = %#v, want Rework", tracker.updates)
				}
				return
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want passive CI wait", tracker.updates)
			}
			if tt.lane != "Merging" {
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
					t.Fatalf("completions = %#v, want success", attempts.completions)
				}
				progress := implementProgressRecordFromCompletion(t, attempts.completions[0])
				if progress.Reason != "signature_changed" || progress.PreviousHeadSHA != "conflicted-head" || progress.CurrentHeadSHA != "validated-head" {
					t.Fatalf("progress = %#v, want changed repaired head", progress)
				}
				completed, ok := state.Completed[issue.ID]
				if !ok || completed.Issue.State != tt.lane || completed.Issue.PullRequest.HeadSHA != "validated-head" {
					t.Fatalf("completion = %#v, want original lane and verified head", completed)
				}
				if len(state.Retry) != 0 || len(state.mergeReservations) != 0 {
					t.Fatal("repair retained merge retry or reservation")
				}
				return
			}

			retry := state.Retry[issue.ID]
			if retry.Wait.Kind != retryWaitCurrentHeadCI || retry.Attempt != 1 {
				t.Fatalf("retry = %#v, want current-head CI wait without another implementation", retry)
			}
			reservation := state.mergeReservations[issue.ID]
			if reservation.ExpiresAt.IsZero() || !reservation.ExpiresAt.After(now) {
				t.Fatalf("reservation = %#v, want bounded merge ownership", reservation)
			}
			_, handled, _ := orch.pollMergeWorkerCurrentHeadCI(t.Context(), &state, issue, retry, now.Add(time.Minute))
			if !handled || len(state.Running) != 0 || len(tracker.updates) != 0 {
				t.Fatal("pending CI redispatched resolution or transitioned the issue after the fallback deadline")
			}
		})
	}
}
