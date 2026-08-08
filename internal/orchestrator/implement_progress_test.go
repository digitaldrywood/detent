package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestHandleRunResultClassifiesImplementWorkerProgress(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 8, 16, 0, 0, 0, time.UTC)
	signature := autoPromoteReworkSignature{
		PRNumber:     1070,
		HeadSHA:      "same-head",
		FailedChecks: []string{"Test"},
	}

	tests := []struct {
		name               string
		runningIssue       connector.Issue
		hydratedIssue      connector.Issue
		hydrateErr         error
		history            []store.WorkAttempt
		diffStats          DiffStats
		noProgressLimit    int
		wantTerminal       store.WorkAttemptTerminalState
		wantReason         string
		wantPreviousHead   string
		wantCurrentHead    string
		wantHydrations     int
		wantBlocked        bool
		wantComment        string
		wantRetry          bool
		wantLogContains    string
		wantFailedAdded    []string
		wantFailedRemoved  []string
		wantConsecutive    int
		wantBlockReason    string
		wantRejectedRef    string
		workpadHumanAction string
		workpadBlockerRef  string
		resolvedBlockers   []connector.Issue
		refreshedState     string
		pullRequestUpdated bool
	}{
		{
			name:            "first attempt succeeds with linked PR",
			runningIssue:    implementProgressIssue("same-head", "Test"),
			hydratedIssue:   implementProgressIssue("same-head", "Test"),
			diffStats:       DiffStats{Status: "clean"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalSuccess,
			wantReason:      "first_completed_attempt",
			wantCurrentHead: "same-head",
			wantHydrations:  1,
			wantRetry:       true,
		},
		{
			name:             "new head SHA succeeds",
			runningIssue:     implementProgressIssue("same-head", "Test"),
			hydratedIssue:    implementProgressIssue("new-head", "Test"),
			history:          []store.WorkAttempt{implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalSuccess)},
			diffStats:        DiffStats{Status: "clean"},
			noProgressLimit:  3,
			wantTerminal:     store.WorkAttemptTerminalSuccess,
			wantReason:       "signature_changed",
			wantPreviousHead: "same-head",
			wantCurrentHead:  "new-head",
			wantHydrations:   1,
			wantRetry:        true,
		},
		{
			name:             "new pull request head with newer unpushed commit remains stranded",
			runningIssue:     implementProgressIssue("same-head", "Test"),
			hydratedIssue:    implementProgressIssue("new-head", "Test"),
			history:          []store.WorkAttempt{implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalSuccess)},
			diffStats:        DiffStats{UnpushedCommits: 1, Status: "clean"},
			noProgressLimit:  3,
			wantTerminal:     store.WorkAttemptTerminalNoProgress,
			wantReason:       strandedUnpushedWorkReason,
			wantConsecutive:  1,
			wantPreviousHead: "same-head",
			wantCurrentHead:  "new-head",
			wantHydrations:   1,
			wantRetry:        true,
		},
		{
			name:             "unchanged signature and clean diff records no progress",
			runningIssue:     implementProgressIssue("same-head", "Test"),
			hydratedIssue:    implementProgressIssue("same-head", "Test"),
			history:          []store.WorkAttempt{implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalSuccess)},
			diffStats:        DiffStats{Status: "clean"},
			noProgressLimit:  3,
			wantTerminal:     store.WorkAttemptTerminalNoProgress,
			wantReason:       "unchanged_signature_clean_diff",
			wantConsecutive:  1,
			wantPreviousHead: "same-head",
			wantCurrentHead:  "same-head",
			wantHydrations:   1,
			wantRetry:        true,
		},
		{
			name:          "stranded unpushed work on existing pull request is distinct",
			runningIssue:  implementProgressIssue("same-head", "Test"),
			hydratedIssue: implementProgressIssue("same-head", "Test"),
			history: []store.WorkAttempt{
				implementProgressStrandedHistoryAttempt(2, signature),
				implementProgressStrandedHistoryAttempt(1, signature),
			},
			diffStats:        DiffStats{UnpushedCommits: 1, Status: "clean"},
			noProgressLimit:  3,
			wantTerminal:     store.WorkAttemptTerminalNoProgress,
			wantReason:       strandedUnpushedWorkReason,
			wantConsecutive:  3,
			wantPreviousHead: "same-head",
			wantCurrentHead:  "same-head",
			wantHydrations:   1,
			wantBlocked:      true,
			wantBlockReason:  strandedUnpushedWorkReason,
			wantComment:      "unpushed_commits: 1",
		},
		{
			name:          "limit trip blocks with comment",
			runningIssue:  implementProgressIssue("same-head", "Test"),
			hydratedIssue: implementProgressIssue("same-head", "Test"),
			history: []store.WorkAttempt{
				implementProgressHistoryAttempt(2, signature, store.WorkAttemptTerminalNoProgress),
				implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalNoProgress),
			},
			diffStats:        DiffStats{Status: "clean"},
			noProgressLimit:  3,
			wantTerminal:     store.WorkAttemptTerminalNoProgress,
			wantReason:       "unchanged_signature_clean_diff",
			wantConsecutive:  3,
			wantPreviousHead: "same-head",
			wantCurrentHead:  "same-head",
			wantHydrations:   1,
			wantBlocked:      true,
			wantBlockReason:  noProgressLimitReason,
			wantComment:      "no_progress_limit",
		},
		{
			name:               "new pull request creation avoids false no progress",
			runningIssue:       implementProgressIssueWithoutPR(),
			diffStats:          DiffStats{Status: "clean"},
			noProgressLimit:    3,
			pullRequestUpdated: true,
			wantTerminal:       store.WorkAttemptTerminalSuccess,
			wantReason:         "pull_request_created_or_updated",
			wantRetry:          true,
		},
		{
			name:            "first clean completion without linked PR records no progress",
			runningIssue:    implementProgressIssueWithoutPR(),
			diffStats:       DiffStats{Status: "clean"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalNoProgress,
			wantReason:      "completed_clean_diff_without_pull_request",
			wantConsecutive: 1,
			wantRetry:       true,
		},
		{
			name:         "third clean dependency wait defers without tripping limit",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressDependencyDeferralHistoryAttempt(2, "digitaldrywood/detent#134", "Todo"),
				implementProgressDependencyDeferralHistoryAttempt(1, "digitaldrywood/detent#134", "Todo"),
			},
			diffStats:         DiffStats{Status: "clean"},
			noProgressLimit:   3,
			wantTerminal:      store.WorkAttemptTerminalSuccess,
			wantReason:        "dependency_deferral",
			workpadBlockerRef: "digitaldrywood/detent#134",
			resolvedBlockers:  []connector.Issue{{ID: "blocker-134", Identifier: "digitaldrywood/detent#134", State: "Todo"}},
			wantLogContains:   "",
			wantConsecutive:   0,
			wantBlocked:       false,
			wantRetry:         false,
		},
		{
			name:              "malformed blocker ref counts as no progress",
			runningIssue:      implementProgressIssueWithoutPR(),
			diffStats:         DiffStats{Status: "clean"},
			noProgressLimit:   3,
			wantTerminal:      store.WorkAttemptTerminalNoProgress,
			wantReason:        "completed_clean_diff_without_pull_request",
			wantConsecutive:   1,
			workpadBlockerRef: "fabricated-ref",
			wantRejectedRef:   "fabricated-ref",
			wantLogContains:   "fabricated-ref",
			wantRetry:         true,
		},
		{
			name:              "unresolvable blocker ref counts as no progress",
			runningIssue:      implementProgressIssueWithoutPR(),
			diffStats:         DiffStats{Status: "clean"},
			noProgressLimit:   3,
			wantTerminal:      store.WorkAttemptTerminalNoProgress,
			wantReason:        "completed_clean_diff_without_pull_request",
			wantConsecutive:   1,
			workpadBlockerRef: "digitaldrywood/detent#9999",
			wantRejectedRef:   "digitaldrywood/detent#9999",
			wantLogContains:   "digitaldrywood/detent#9999",
			wantRetry:         true,
		},
		{
			name:              "already terminal blocker does not defer empty attempt",
			runningIssue:      implementProgressIssueWithoutPR(),
			diffStats:         DiffStats{Status: "clean"},
			noProgressLimit:   3,
			wantTerminal:      store.WorkAttemptTerminalNoProgress,
			wantReason:        "completed_clean_diff_without_pull_request",
			wantConsecutive:   1,
			workpadBlockerRef: "digitaldrywood/detent#134",
			resolvedBlockers:  []connector.Issue{{ID: "blocker-134", Identifier: "digitaldrywood/detent#134", State: "Done"}},
			wantRetry:         true,
		},
		{
			name:         "July telemetry replay trips third clean completion without linked PR",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressLegacyNoPRHistoryAttempt(2),
				implementProgressLegacyNoPRHistoryAttempt(1),
			},
			diffStats:       DiffStats{Status: "clean"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalNoProgress,
			wantReason:      "completed_clean_diff_without_pull_request",
			wantConsecutive: 3,
			wantBlocked:     true,
			wantBlockReason: noProgressLimitReason,
			wantComment:     "consecutive_no_progress_attempts: 3",
		},
		{
			name:         "identical non-empty diff trips third completion without linked PR",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressNoPRHistoryAttempt(2, DiffStats{FilesChanged: 9, AddedLines: 120, RemovedLines: 30, Fingerprint: "same-diff", Status: "changed"}, "", ""),
				implementProgressNoPRHistoryAttempt(1, DiffStats{FilesChanged: 9, AddedLines: 120, RemovedLines: 30, Fingerprint: "same-diff", Status: "changed"}, "", ""),
			},
			diffStats:       DiffStats{FilesChanged: 9, AddedLines: 120, RemovedLines: 30, Fingerprint: "same-diff", Status: "changed"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalNoProgress,
			wantReason:      "unchanged_workspace_diff_without_pull_request",
			wantConsecutive: 3,
			wantBlocked:     true,
			wantBlockReason: noProgressLimitReason,
			wantComment:     "consecutive_no_progress_attempts: 3",
		},
		{
			name:         "stranded unpushed work trips distinct third completion without linked PR",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressNoPRHistoryAttempt(2, DiffStats{UnpushedCommits: 1, Status: "clean"}, "", ""),
				implementProgressNoPRHistoryAttempt(1, DiffStats{UnpushedCommits: 1, Status: "clean"}, "", ""),
			},
			diffStats:       DiffStats{UnpushedCommits: 1, Status: "clean"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalNoProgress,
			wantReason:      strandedUnpushedWorkReason,
			wantConsecutive: 3,
			wantBlocked:     true,
			wantBlockReason: strandedUnpushedWorkReason,
			wantComment:     "work produced but stranded unpushed",
		},
		{
			name:         "changing non-empty diff does not trip without linked PR",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressNoPRHistoryAttempt(2, DiffStats{FilesChanged: 9, AddedLines: 120, RemovedLines: 30, Fingerprint: "second-diff", Status: "changed"}, "", ""),
				implementProgressNoPRHistoryAttempt(1, DiffStats{FilesChanged: 8, AddedLines: 119, RemovedLines: 30, Fingerprint: "first-diff", Status: "changed"}, "", ""),
			},
			diffStats:       DiffStats{FilesChanged: 10, AddedLines: 121, RemovedLines: 30, Fingerprint: "third-diff", Status: "changed"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalSuccess,
			wantReason:      "workspace_diff_present_without_pull_request",
			wantRetry:       true,
		},
		{
			name:         "repeated blocked human action trips despite diff noise",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressNoPRHistoryAttempt(1, DiffStats{FilesChanged: 1, AddedLines: 1, Status: "changed"}, "Choose the exhaustive review path.", "In Progress"),
			},
			diffStats:          DiffStats{FilesChanged: 12, AddedLines: 240, RemovedLines: 17, Status: "changed"},
			noProgressLimit:    3,
			wantTerminal:       store.WorkAttemptTerminalNoProgress,
			wantReason:         "workpad_blocked_unactioned",
			wantBlocked:        true,
			wantBlockReason:    "workpad_blocked_unactioned",
			wantComment:        "> Choose the exhaustive review path.",
			workpadHumanAction: "Choose the exhaustive review path.",
			workpadBlockerRef:  "digitaldrywood/detent#134",
		},
		{
			name:         "changing blocked human action does not trip",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressNoPRHistoryAttempt(2, DiffStats{FilesChanged: 2, AddedLines: 2, Fingerprint: "same-diff", Status: "changed"}, "Choose the old review path.", "In Progress"),
				implementProgressNoPRHistoryAttempt(1, DiffStats{FilesChanged: 2, AddedLines: 2, Fingerprint: "same-diff", Status: "changed"}, "Choose the old review path.", "In Progress"),
			},
			diffStats:          DiffStats{FilesChanged: 2, AddedLines: 2, Fingerprint: "same-diff", Status: "changed"},
			noProgressLimit:    3,
			wantTerminal:       store.WorkAttemptTerminalSuccess,
			wantReason:         "workspace_diff_present_without_pull_request",
			wantRetry:          true,
			workpadHumanAction: "Choose the new review path.",
		},
		{
			name:         "tracker state change resets repeated blocked human action",
			runningIssue: implementProgressIssueWithoutPR(),
			history: []store.WorkAttempt{
				implementProgressNoPRHistoryAttempt(1, DiffStats{FilesChanged: 1, AddedLines: 1, Fingerprint: "old-diff", Status: "changed"}, "Choose the review path.", "In Progress"),
			},
			diffStats:          DiffStats{FilesChanged: 2, AddedLines: 2, Fingerprint: "new-diff", Status: "changed"},
			noProgressLimit:    3,
			wantTerminal:       store.WorkAttemptTerminalSuccess,
			wantReason:         "workspace_diff_present_without_pull_request",
			wantRetry:          true,
			workpadHumanAction: "Choose the review path.",
			refreshedState:     "Rework",
		},
		{
			name:            "hydration failure fails open to success",
			runningIssue:    implementProgressIssue("same-head", "Test"),
			hydratedIssue:   implementProgressIssue("same-head", "Test"),
			hydrateErr:      errors.New("github hiccup"),
			history:         []store.WorkAttempt{implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalSuccess)},
			diffStats:       DiffStats{Status: "clean"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalSuccess,
			wantReason:      "pull_request_hydration_failed",
			wantHydrations:  1,
			wantRetry:       true,
			wantLogContains: "implement worker progress check failed open",
		},
		{
			name:            "degraded hydration fails open to success",
			runningIssue:    implementProgressIssue("same-head", "Test"),
			hydratedIssue:   implementProgressIssueWithHydrationDegraded("same-head", connector.PullRequestHydrationReasonStaleCachedPullData, "Test"),
			history:         []store.WorkAttempt{implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalSuccess)},
			diffStats:       DiffStats{Status: "clean"},
			noProgressLimit: 3,
			wantTerminal:    store.WorkAttemptTerminalSuccess,
			wantReason:      "pull_request_hydration_unavailable",
			wantHydrations:  1,
			wantRetry:       true,
			wantLogContains: "implement worker progress check failed open",
		},
		{
			name:              "failed check delta is recorded",
			runningIssue:      implementProgressIssue("new-head", "Test", "Lint"),
			hydratedIssue:     implementProgressIssue("new-head", "Test", "Lint"),
			history:           []store.WorkAttempt{implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalSuccess)},
			diffStats:         DiffStats{Status: "clean"},
			noProgressLimit:   3,
			wantTerminal:      store.WorkAttemptTerminalSuccess,
			wantReason:        "signature_changed",
			wantPreviousHead:  "same-head",
			wantCurrentHead:   "new-head",
			wantHydrations:    1,
			wantRetry:         true,
			wantFailedAdded:   []string{"Lint"},
			wantFailedRemoved: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			refreshed := tt.runningIssue
			if tt.refreshedState != "" {
				refreshed.State = tt.refreshedState
			}
			if tt.workpadHumanAction != "" || tt.workpadBlockerRef != "" {
				refreshed.Comments = []connector.IssueComment{{
					Body: implementProgressWorkpadComment(tt.workpadBlockerRef, tt.workpadHumanAction),
					URL:  "https://github.test/workpad",
				}}
			}
			tracker := &implementProgressConnector{
				hydrated:         tt.hydratedIssue,
				refreshed:        refreshed,
				hydrateErr:       tt.hydrateErr,
				resolvedBlockers: tt.resolvedBlockers,
			}
			attempts := &implementProgressAttemptStore{history: tt.history}
			cfg := normalizeConfig(Config{
				Project:                scheduler.ProjectCandidate{ID: "detent"},
				AutoPromote:            AutoPromoteConfig{NoProgressLimit: tt.noProgressLimit},
				ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
				ObservedStates:         []string{"Human Review", "Blocked"},
				TerminalStates:         []string{"Done", "Cancelled"},
				ContinuationRetryDelay: time.Minute,
			})
			orch := &Orchestrator{
				cfg:          cfg,
				connector:    tracker,
				workAttempts: attempts,
				logger:       slog.New(slog.NewTextHandler(&logs, nil)),
			}
			state := newState(cfg)
			running := Running{
				Issue:         tt.runningIssue,
				Attempt:       1,
				WorkAttemptID: 42,
				Mode:          runpkg.RunModeImplement,
				StartedAt:     base.Add(-time.Minute),
				DiffStats:     tt.diffStats,
			}
			state.Running[tt.runningIssue.ID] = running
			state.Claimed[tt.runningIssue.ID] = Claimed{Issue: tt.runningIssue, ClaimedAt: running.StartedAt}

			orch.handleRunResult(context.Background(), &state, runpkg.Completion{
				IssueID:     tt.runningIssue.ID,
				CompletedAt: base,
				Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
				Result: runpkg.RunResult{
					FinalState:         FinalStateCompleted,
					DiffStats:          tt.diffStats,
					PullRequestUpdated: tt.pullRequestUpdated,
				},
			})

			if len(attempts.completions) != 1 {
				t.Fatalf("completions len = %d, want 1", len(attempts.completions))
			}
			completion := attempts.completions[0]
			if completion.TerminalState != tt.wantTerminal {
				t.Fatalf("TerminalState = %q, want %q", completion.TerminalState, tt.wantTerminal)
			}
			record := implementProgressRecordFromCompletion(t, completion)
			if record.Reason != tt.wantReason {
				t.Fatalf("metadata reason = %q, want %q", record.Reason, tt.wantReason)
			}
			if record.PreviousHeadSHA != tt.wantPreviousHead {
				t.Fatalf("previous head = %q, want %q", record.PreviousHeadSHA, tt.wantPreviousHead)
			}
			if record.CurrentHeadSHA != tt.wantCurrentHead {
				t.Fatalf("current head = %q, want %q", record.CurrentHeadSHA, tt.wantCurrentHead)
			}
			if record.ConsecutiveNoProgress != tt.wantConsecutive {
				t.Fatalf("consecutive no progress = %d, want %d", record.ConsecutiveNoProgress, tt.wantConsecutive)
			}
			if record.BlockReason != tt.wantBlockReason {
				t.Fatalf("block reason = %q, want %q", record.BlockReason, tt.wantBlockReason)
			}
			if tt.wantRejectedRef != "" && !strings.Contains(strings.Join(record.RejectedBlockerRefs, ","), tt.wantRejectedRef) {
				t.Fatalf("rejected blocker refs = %#v, want %q", record.RejectedBlockerRefs, tt.wantRejectedRef)
			}
			if !slicesEqual(record.FailedChecksAdded, tt.wantFailedAdded) {
				t.Fatalf("failed checks added = %#v, want %#v", record.FailedChecksAdded, tt.wantFailedAdded)
			}
			if !slicesEqual(record.FailedChecksRemoved, tt.wantFailedRemoved) {
				t.Fatalf("failed checks removed = %#v, want %#v", record.FailedChecksRemoved, tt.wantFailedRemoved)
			}
			if tracker.hydrations != tt.wantHydrations {
				t.Fatalf("hydrations = %d, want %d", tracker.hydrations, tt.wantHydrations)
			}
			if _, ok := state.Blocked[tt.runningIssue.ID]; ok != tt.wantBlocked {
				t.Fatalf("blocked present = %v, want %v", ok, tt.wantBlocked)
			}
			if tt.wantBlocked {
				if len(tracker.updates) != 1 || tracker.updates[0].state != blockedStatusState {
					t.Fatalf("updates = %#v, want one Blocked update", tracker.updates)
				}
				if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, tt.wantComment) {
					t.Fatalf("comments = %#v, want comment containing %q", tracker.comments, tt.wantComment)
				}
				if _, ok := state.Retry[tt.runningIssue.ID]; ok {
					t.Fatalf("Retry[%q] present after block", tt.runningIssue.ID)
				}
			} else if _, ok := state.Retry[tt.runningIssue.ID]; ok != tt.wantRetry {
				t.Fatalf("retry present = %v, want %v", ok, tt.wantRetry)
			}
			if tt.wantLogContains != "" && !strings.Contains(logs.String(), tt.wantLogContains) {
				t.Fatalf("logs did not contain %q:\n%s", tt.wantLogContains, logs.String())
			}
		})
	}
}

func TestHandleRunResultStopsCompletedGateWaitContinuations(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 9, 18, 50, 0, 0, time.UTC)
	issue := implementProgressIssue("same-head", "Test")
	signature := autoPromoteReworkSignature{
		PRNumber:     1070,
		HeadSHA:      "same-head",
		FailedChecks: []string{"Test"},
	}
	tests := []struct {
		name           string
		history        []store.WorkAttempt
		wantTerminal   store.WorkAttemptTerminalState
		wantHydrations int
	}{
		{
			name:           "initial success waits for gate without continuation",
			wantTerminal:   store.WorkAttemptTerminalSuccess,
			wantHydrations: 1,
		},
		{
			name:         "redundant dispatch is superseded without breaker strike",
			history:      []store.WorkAttempt{implementProgressHistoryAttempt(1, signature, store.WorkAttemptTerminalSuccess)},
			wantTerminal: store.WorkAttemptTerminalSuperseded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &implementProgressConnector{hydrated: issue}
			attempts := &implementProgressAttemptStore{history: tt.history}
			cfg := normalizeConfig(Config{
				Project: scheduler.ProjectCandidate{ID: "detent"},
				AutoPromote: AutoPromoteConfig{
					Enabled:         true,
					QuietDuration:   0,
					GateWaitState:   autoPromoteGateWaitSource,
					NoProgressLimit: 1,
					Gate:            gate.Config{Kind: gate.KindCommand},
				},
				ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
				ObservedStates:         []string{"Human Review", "Blocked"},
				TerminalStates:         []string{"Done", "Cancelled"},
				ContinuationRetryDelay: time.Minute,
			})
			orch := &Orchestrator{
				cfg:          cfg,
				connector:    tracker,
				workAttempts: attempts,
				logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
			}
			state := newState(cfg)
			state.Running[issue.ID] = Running{
				Issue:         issue,
				Attempt:       2,
				WorkAttemptID: 42,
				Mode:          runpkg.RunModeImplement,
				StartedAt:     base.Add(-time.Minute),
				DiffStats:     DiffStats{Status: "clean"},
			}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: base.Add(-time.Minute)}

			orch.handleRunResult(context.Background(), &state, runpkg.Completion{
				IssueID:     issue.ID,
				CompletedAt: base,
				Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
				Result: runpkg.RunResult{
					FinalState: FinalStateCompleted,
					DiffStats:  DiffStats{Status: "clean"},
				},
			})

			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != tt.wantTerminal {
				t.Fatalf("completions = %#v, want terminal %q", attempts.completions, tt.wantTerminal)
			}
			if tracker.hydrations != tt.wantHydrations {
				t.Fatalf("hydrations = %d, want %d", tracker.hydrations, tt.wantHydrations)
			}
			if _, ok := state.Completed[issue.ID]; !ok {
				t.Fatalf("Completed[%q] missing after gate-wait completion", issue.ID)
			}
			if _, ok := state.Retry[issue.ID]; ok {
				t.Fatalf("Retry[%q] present after gate-wait completion", issue.ID)
			}
			if _, ok := state.Claimed[issue.ID]; ok {
				t.Fatalf("Claimed[%q] present after gate-wait completion", issue.ID)
			}
			if _, ok := state.Blocked[issue.ID]; ok {
				t.Fatalf("Blocked[%q] present after gate-wait completion", issue.ID)
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("state updates = %#v, want breaker untouched", tracker.updates)
			}
		})
	}
}

func TestHandleRunResultCommentsOnObservedLaneTransition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 9, 30, 0, 0, time.UTC)
	runningIssue := implementProgressIssue("head")
	hydratedIssue := cloneIssue(runningIssue)
	hydratedIssue.State = blockedStatusState
	hydratedIssue.WorkpadSignal = &workpad.Signal{
		Source:      workpad.SourceStructured,
		Status:      workpad.StatusBlocked,
		HumanAction: "Restart the browser session.",
	}
	tracker := &implementProgressConnector{hydrated: hydratedIssue}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project:                scheduler.ProjectCandidate{ID: "detent"},
		AutoPromote:            AutoPromoteConfig{NoProgressLimit: 3},
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Human Review", "Blocked"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Minute,
	})
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[runningIssue.ID] = Running{
		Issue:         runningIssue,
		Attempt:       1,
		WorkAttemptID: 42,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     now.Add(-time.Minute),
		DiffStats:     DiffStats{Status: "clean"},
	}
	state.Claimed[runningIssue.ID] = Claimed{Issue: runningIssue, ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			DiffStats:  runpkg.DiffStats{Status: "clean"},
		},
	})

	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one observed routing audit comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Observed this issue move from In Progress to Blocked during worker completion.",
		"source: tracker_refresh",
		"reason: workpad_blocked",
		"human_action: Restart the browser session.",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing %q", tracker.comments[0].body, fragment)
		}
	}
}

func TestHandleRunResultReappliesCITriggerAfterWorkerPush(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 9, 45, 0, 0, time.UTC)
	staggerSeconds := 15
	runningIssue := implementProgressIssue("old-head")
	hydratedIssue := implementProgressIssue("new-head")
	relabelStarted := make(chan autoPromoteTickRelabel, 1)
	relabelRelease := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case relabelRelease <- struct{}{}:
		default:
		}
	})
	tracker := &implementProgressConnector{
		hydrated:       hydratedIssue,
		relabelStarted: relabelStarted,
		relabelRelease: relabelRelease,
	}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"},
		AutoPromote: AutoPromoteConfig{
			NoProgressLimit: 3,
			Gate: gate.Config{
				Kind:                         gate.KindCommand,
				RequiredStatusChecks:         []string{"Test", "Checks"},
				CITriggerLabel:               "ci:ready",
				CITriggerLabelStaggerSeconds: &staggerSeconds,
			},
		},
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Human Review", "Blocked"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Minute,
	})
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[runningIssue.ID] = Running{
		Issue:         runningIssue,
		Attempt:       1,
		WorkAttemptID: 42,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     now.Add(-time.Minute),
		DiffStats:     DiffStats{Status: "clean"},
	}
	state.Claimed[runningIssue.ID] = Claimed{Issue: runningIssue, ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState:            runpkg.FinalStateCompleted,
			DiffStats:             runpkg.DiffStats{Status: "clean"},
			PullRequestHeadPushed: true,
		},
	})

	select {
	case got := <-relabelStarted:
		want := autoPromoteTickRelabel{repository: "digitaldrywood/detent", number: 1070, label: "ci:ready", stagger: 15 * time.Second}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("relabel = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker-push trigger-label reapplication")
	}
}

func TestHandleRunResultRefreshesNewPullRequestBeforeCITrigger(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	staggerSeconds := 15
	runningIssue := implementProgressIssue("")
	runningIssue.PullRequest = nil
	refreshedIssue := implementProgressIssue("new-head")
	relabelStarted := make(chan autoPromoteTickRelabel, 1)
	relabelRelease := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case relabelRelease <- struct{}{}:
		default:
		}
	})
	tracker := &implementProgressConnector{
		refreshed:      refreshedIssue,
		hydrated:       refreshedIssue,
		relabelStarted: relabelStarted,
		relabelRelease: relabelRelease,
	}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"},
		AutoPromote: AutoPromoteConfig{
			NoProgressLimit: 3,
			Gate: gate.Config{
				Kind:                         gate.KindCommand,
				RequiredStatusChecks:         []string{"Test", "Checks"},
				CITriggerLabel:               "ci:ready",
				CITriggerLabelStaggerSeconds: &staggerSeconds,
			},
		},
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Human Review", "Blocked"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Minute,
	})
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[runningIssue.ID] = Running{
		Issue:         runningIssue,
		Attempt:       1,
		WorkAttemptID: 42,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     now.Add(-time.Minute),
		DiffStats:     DiffStats{Status: "clean"},
	}
	state.Claimed[runningIssue.ID] = Claimed{Issue: runningIssue, ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState:            runpkg.FinalStateCompleted,
			DiffStats:             runpkg.DiffStats{Status: "clean"},
			PullRequestUpdated:    true,
			PullRequestHeadPushed: true,
		},
	})

	select {
	case got := <-relabelStarted:
		want := autoPromoteTickRelabel{repository: "digitaldrywood/detent", number: 1070, label: "ci:ready", stagger: 15 * time.Second}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("relabel = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for new-PR trigger-label reapplication")
	}
	if tracker.hydrations != 1 {
		t.Fatalf("hydrations = %d, want 1", tracker.hydrations)
	}
}

func TestHandleRunResultRefreshesStalePullRequestAfterHydrationFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 10, 30, 0, 0, time.UTC)
	staggerSeconds := 15
	runningIssue := implementProgressIssue("old-head")
	refreshedIssue := implementProgressIssue("new-head")
	relabelStarted := make(chan autoPromoteTickRelabel, 1)
	relabelRelease := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case relabelRelease <- struct{}{}:
		default:
		}
	})
	tracker := &implementProgressConnector{
		hydrated: refreshedIssue,
		hydrateErrs: []error{
			errors.New("completion hydration failure"),
			errors.New("push refresh hydration failure"),
		},
		relabelStarted: relabelStarted,
		relabelRelease: relabelRelease,
	}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"},
		AutoPromote: AutoPromoteConfig{
			NoProgressLimit: 3,
			Gate: gate.Config{
				Kind:                         gate.KindCommand,
				RequiredStatusChecks:         []string{"Test", "Checks"},
				CITriggerLabel:               "ci:ready",
				CITriggerLabelStaggerSeconds: &staggerSeconds,
			},
		},
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:         []string{"Human Review", "Blocked"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Minute,
	})
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ciTriggerLabelHeads: map[string]ciTriggerLabelHead{
			"digitaldrywood/detent#1070|ci:ready": {HeadSHA: "old-head"},
		},
	}
	state := newState(cfg)
	state.Running[runningIssue.ID] = Running{
		Issue:         runningIssue,
		Attempt:       1,
		WorkAttemptID: 42,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     now.Add(-time.Minute),
		DiffStats:     DiffStats{Status: "clean"},
	}
	state.Claimed[runningIssue.ID] = Claimed{Issue: runningIssue, ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState:            runpkg.FinalStateCompleted,
			DiffStats:             runpkg.DiffStats{Status: "clean"},
			PullRequestHeadPushed: true,
		},
	})

	select {
	case got := <-relabelStarted:
		want := autoPromoteTickRelabel{repository: "digitaldrywood/detent", number: 1070, label: "ci:ready", stagger: 15 * time.Second}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("relabel = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refreshed-head trigger-label reapplication")
	}
	if tracker.hydrations != 2 {
		t.Fatalf("hydrations = %d, want completion hydration plus push refresh attempt", tracker.hydrations)
	}
}

func TestStickyBlockReasonIncludesCircuitBreakers(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{
		noProgressLimitReason,
		workpadBlockedUnactionedReason,
		"token_ceiling_circuit_breaker",
		tokenCeilingBlockedReasonPrefix + "observed 16100000 tokens above the 16000000 max_session_tokens ceiling",
		mergeWorkerRetryExhaustedReason,
	} {
		if !stickyBlockReason(reason) {
			t.Fatalf("stickyBlockReason(%q) = false, want true", reason)
		}
	}
}

func TestImplementProgressHelperBoundaries(t *testing.T) {
	t.Parallel()

	issue := connector.Issue{WorkpadSignal: &workpad.Signal{
		Source:      workpad.SourceStructured,
		Status:      workpad.StatusBlocked,
		HumanAction: " approve the deployment ",
	}}
	if status, action := implementProgressBlockedHumanAction(issue); status != workpad.StatusBlocked || action != "approve the deployment" {
		t.Fatalf("implementProgressBlockedHumanAction() = %q, %q", status, action)
	}
	issue.WorkpadSignal.Source = workpad.SourceProse
	if status, action := implementProgressBlockedHumanAction(issue); status != "" || action != "" {
		t.Fatalf("prose workpad signal = %q, %q, want empty", status, action)
	}

	usable := autoPromoteReworkSignature{PRNumber: 42, HeadSHA: " head ", FailedChecks: []string{"test"}}
	attempts := []store.WorkAttempt{
		{TerminalState: store.WorkAttemptTerminalFailure},
		implementProgressHistoryAttempt(1, usable, store.WorkAttemptTerminalSuccess),
	}
	if got, ok := latestImplementProgressSignature(attempts); !ok || got.PRNumber != usable.PRNumber || got.HeadSHA != "head" {
		t.Fatalf("latestImplementProgressSignature() = %#v, %t", got, ok)
	}
	if got := consecutiveImplementBlockedHumanActionAttempts(nil, "", "Blocked"); got != 0 {
		t.Fatalf("consecutiveImplementBlockedHumanActionAttempts() = %d, want 0", got)
	}

	legacy := implementProgressRecord{
		Outcome:            string(store.WorkAttemptTerminalSuccess),
		Reason:             "no_linked_pull_request",
		WorkspaceDiffStats: implementProgressDiffStats{Status: "clean"},
	}
	if !implementProgressRecordMatchesNoProgress(legacy, autoPromoteReworkSignature{}) {
		t.Fatal("legacy clean completion did not match no progress")
	}
	legacy.WorkspaceDiffStats.FilesChanged = 1
	if implementProgressRecordMatchesNoProgress(legacy, autoPromoteReworkSignature{}) {
		t.Fatal("legacy dirty completion matched no progress")
	}

	invalidAttempts := []store.WorkAttempt{
		{TerminalState: store.WorkAttemptTerminalFailure, WorkerMetadataJSON: `{}`},
		{TerminalState: store.WorkAttemptTerminalSuccess, WorkerMetadataJSON: `{`},
		{TerminalState: store.WorkAttemptTerminalSuccess, WorkerMetadataJSON: `{}`},
	}
	for _, attempt := range invalidAttempts {
		if _, ok := implementProgressRecordFromAttempt(attempt); ok {
			t.Fatalf("implementProgressRecordFromAttempt(%#v) unexpectedly succeeded", attempt)
		}
	}

	added, removed := implementProgressFailedCheckDelta([]string{"test", "lint"}, []string{"lint", "build"})
	if !slicesEqual(added, []string{"build"}) || !slicesEqual(removed, []string{"test"}) {
		t.Fatalf("implementProgressFailedCheckDelta() = %#v, %#v", added, removed)
	}
}

func TestImplementProgressBlockCommentIncludesBoundaryEvidence(t *testing.T) {
	t.Parallel()

	decision := implementCompletionProgressDecision{
		BlockReason:            workpadBlockedUnactionedReason,
		NoProgressLimit:        3,
		ConsecutiveNoProgress:  2,
		ConsecutiveHumanAction: 3,
		HumanAction:            "Approve release\nConfirm rollback",
		CurrentSignature:       autoPromoteReworkSignature{PRNumber: 42, HeadSHA: "head", FailedChecks: []string{"test"}},
		PreviousSignature:      autoPromoteReworkSignature{HeadSHA: "previous"},
		FailedChecksAdded:      []string{"test"},
		FailedChecksRemoved:    []string{"lint"},
		WorkspaceDiffStats:     DiffStats{Status: "clean"},
	}
	issue := connector.Issue{PullRequest: &connector.PullRequest{URL: "https://github.test/pull/42"}}
	comment := implementProgressBlockComment(issue, decision)
	for _, want := range []string{"workpad_blocked_unactioned", "pull/42", "head", "previous", "failed_checks_added", "0 files", "> Approve release", "> Confirm rollback"} {
		if !strings.Contains(comment, want) {
			t.Fatalf("comment missing %q:\n%s", want, comment)
		}
	}
	if got := implementProgressRecoveryReason(decision); got != decision.HumanAction {
		t.Fatalf("implementProgressRecoveryReason() = %q, want %q", got, decision.HumanAction)
	}
}

func TestEvaluateImplementCompletionProgressFailureBoundaries(t *testing.T) {
	t.Parallel()

	noPR := implementProgressIssueWithoutPR()
	var logs bytes.Buffer
	orch := &Orchestrator{
		cfg:          Config{AutoPromote: AutoPromoteConfig{NoProgressLimit: 3}},
		connector:    &implementProgressConnector{},
		workAttempts: &implementProgressAttemptStore{historyErr: errors.New("history unavailable")},
		logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	}
	decision := orch.evaluateImplementCompletionProgress(t.Context(), Running{Issue: noPR}, FinalStateCompleted, false)
	if decision.Reason != "attempt_history_lookup_failed" || !strings.Contains(decision.Warning, "history unavailable") || !strings.Contains(logs.String(), "history unavailable") {
		t.Fatalf("history failure decision = %#v logs = %q", decision, logs.String())
	}

	orch.workAttempts = &implementProgressAttemptStore{}
	decision = orch.evaluateImplementCompletionProgress(t.Context(), Running{
		Issue:     noPR,
		DiffStats: DiffStats{FilesChanged: 1, Status: "dirty"},
	}, FinalStateCompleted, false)
	if decision.Reason != "workspace_diff_fingerprint_unavailable_without_pull_request" {
		t.Fatalf("dirty no-PR decision = %#v", decision)
	}

	linked := implementProgressIssue("head")
	orch.connector = &backendCapacityTestConnector{}
	decision = orch.evaluateImplementCompletionProgress(t.Context(), Running{Issue: linked}, FinalStateCompleted, false)
	if decision.Reason != "pull_request_hydrator_unavailable" {
		t.Fatalf("missing hydrator decision = %#v", decision)
	}

	orch.warnImplementProgressRefresh(linked, "refresh unavailable", errors.New("tracker unavailable"))
	if !strings.Contains(logs.String(), "tracker unavailable") {
		t.Fatalf("refresh warning logs = %q", logs.String())
	}
}

func implementProgressIssue(headSHA string, failedChecks ...string) connector.Issue {
	prNumber := 1070
	issue := connector.Issue{
		ID:           "issue-1070",
		Identifier:   "digitaldrywood/detent#1070",
		Title:        "No progress",
		State:        "In Progress",
		URL:          "https://github.test/digitaldrywood/detent/issues/1070",
		PRNumber:     &prNumber,
		PRRepository: "digitaldrywood/detent",
		PullRequest: &connector.PullRequest{
			Number:  prNumber,
			URL:     "https://github.test/digitaldrywood/detent/pull/1070",
			State:   "OPEN",
			HeadSHA: headSHA,
		},
	}
	for _, check := range failedChecks {
		issue.PullRequest.RequiredCheckFailures = append(issue.PullRequest.RequiredCheckFailures, connector.PullRequestCheck{
			Name:       check,
			Status:     "completed",
			Conclusion: "failure",
		})
	}
	return issue
}

func implementProgressIssueWithHydrationDegraded(headSHA string, reason string, failedChecks ...string) connector.Issue {
	issue := implementProgressIssue(headSHA, failedChecks...)
	issue.PullRequest.HydrationDegradedReason = reason
	return issue
}

func implementProgressIssueWithoutPR() connector.Issue {
	return connector.Issue{
		ID:         "issue-plan",
		Identifier: "digitaldrywood/detent#1200",
		Title:      "Plan only",
		State:      "In Progress",
		URL:        "https://github.test/digitaldrywood/detent/issues/1200",
	}
}

func implementProgressWorkpadComment(blockerRef string, humanAction string) string {
	var body strings.Builder
	body.WriteString("## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\n")
	if strings.TrimSpace(blockerRef) == "" {
		body.WriteString("blockers: []\n")
	} else {
		body.WriteString("blockers:\n  - ref: \"")
		body.WriteString(blockerRef)
		body.WriteString("\"\n    reason: \"waiting for dependency\"\n")
	}
	if strings.TrimSpace(humanAction) == "" {
		body.WriteString("human_action: null\n```")
	} else {
		body.WriteString("human_action: \"")
		body.WriteString(humanAction)
		body.WriteString("\"\n```")
	}
	return body.String()
}

func implementProgressHistoryAttempt(id int64, signature autoPromoteReworkSignature, terminal store.WorkAttemptTerminalState) store.WorkAttempt {
	return store.WorkAttempt{
		ID:                 id,
		ProjectID:          "detent",
		IssueID:            "issue-1070",
		Identifier:         "digitaldrywood/detent#1070",
		IssueURL:           "https://github.test/digitaldrywood/detent/issues/1070",
		WorkerType:         "agent",
		Status:             store.WorkAttemptStatusTerminal,
		TerminalState:      terminal,
		CompletedAt:        time.Date(2026, 7, 8, 15, int(id), 0, 0, time.UTC),
		WorkerMetadataJSON: implementProgressMetadataJSON(signature, terminal),
	}
}

func implementProgressLegacyNoPRHistoryAttempt(id int64) store.WorkAttempt {
	return store.WorkAttempt{
		ID:            id,
		ProjectID:     "gopher-ai",
		IssueID:       "issue-213",
		Identifier:    "gopherguides/gopher-ai#213",
		IssueURL:      "https://github.test/gopherguides/gopher-ai/issues/213",
		WorkerType:    "agent",
		Status:        store.WorkAttemptStatusTerminal,
		TerminalState: store.WorkAttemptTerminalSuccess,
		CompletedAt:   time.Date(2026, 7, 10, 22, int(id), 0, 0, time.UTC),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			"run_mode": runpkg.RunModeImplement,
			implementProgressMetadataKey: implementProgressRecord{
				Outcome:            string(store.WorkAttemptTerminalSuccess),
				Reason:             "no_linked_pull_request",
				WorkspaceDiffStats: implementProgressDiffStats{Status: "clean"},
			},
		}),
	}
}

func implementProgressDependencyDeferralHistoryAttempt(id int64, identifier string, state string) store.WorkAttempt {
	return store.WorkAttempt{
		ID:            id,
		ProjectID:     "detent",
		IssueID:       "issue-plan",
		Identifier:    "digitaldrywood/detent#1200",
		IssueURL:      "https://github.test/digitaldrywood/detent/issues/1200",
		WorkerType:    "agent",
		Status:        store.WorkAttemptStatusTerminal,
		TerminalState: store.WorkAttemptTerminalSuccess,
		CompletedAt:   time.Date(2026, 8, 8, 18, int(id), 0, 0, time.UTC),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			"run_mode": runpkg.RunModeImplement,
			implementProgressMetadataKey: implementProgressRecord{
				Outcome:            string(store.WorkAttemptTerminalSuccess),
				Reason:             implementDependencyDeferralReason,
				DependencyDeferral: true,
				DependencyBlockers: []implementDependencyBlocker{{ID: "blocker", Identifier: identifier, State: state}},
				WorkspaceDiffStats: implementProgressDiffStats{Status: "clean"},
			},
		}),
	}
}

func implementProgressNoPRHistoryAttempt(id int64, diffStats DiffStats, humanAction string, trackerState string) store.WorkAttempt {
	workpadStatus := ""
	if strings.TrimSpace(humanAction) != "" {
		workpadStatus = "blocked"
	}
	return store.WorkAttempt{
		ID:            id,
		ProjectID:     "detent",
		IssueID:       "issue-plan",
		Identifier:    "digitaldrywood/detent#1200",
		IssueURL:      "https://github.test/digitaldrywood/detent/issues/1200",
		WorkerType:    "agent",
		Status:        store.WorkAttemptStatusTerminal,
		TerminalState: store.WorkAttemptTerminalSuccess,
		CompletedAt:   time.Date(2026, 7, 11, 12, int(id), 0, 0, time.UTC),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			"run_mode": runpkg.RunModeImplement,
			implementProgressMetadataKey: map[string]any{
				"outcome":            string(store.WorkAttemptTerminalSuccess),
				"reason":             "workspace_diff_present_without_pull_request",
				"workspace_diffstat": implementProgressDiffStatsFromDiffStats(diffStats),
				"workpad_status":     workpadStatus,
				"human_action":       humanAction,
				"tracker_state":      trackerState,
			},
		}),
	}
}

func implementProgressStrandedHistoryAttempt(id int64, signature autoPromoteReworkSignature) store.WorkAttempt {
	return store.WorkAttempt{
		ID:            id,
		ProjectID:     "detent",
		IssueID:       "issue-plan",
		Identifier:    "digitaldrywood/detent#1200",
		IssueURL:      "https://github.test/digitaldrywood/detent/issues/1200",
		WorkerType:    "agent",
		Status:        store.WorkAttemptStatusTerminal,
		TerminalState: store.WorkAttemptTerminalNoProgress,
		CompletedAt:   time.Date(2026, 7, 12, 12, int(id), 0, 0, time.UTC),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			"run_mode": runpkg.RunModeImplement,
			implementProgressMetadataKey: implementProgressRecord{
				Outcome:            implementProgressOutcomeNoProgress,
				Reason:             strandedUnpushedWorkReason,
				CurrentSignature:   implementProgressSignatureRecordFromSignature(signature),
				CurrentHeadSHA:     signature.HeadSHA,
				WorkspaceDiffStats: implementProgressDiffStats{UnpushedCommits: 1, Status: "clean"},
			},
		}),
	}
}

func implementProgressMetadataJSON(signature autoPromoteReworkSignature, terminal store.WorkAttemptTerminalState) string {
	return marshalWorkAttemptJSON(map[string]any{
		"run_mode": runpkg.RunModeImplement,
		implementProgressMetadataKey: implementProgressRecord{
			Outcome:          string(terminal),
			Reason:           "test_history",
			CurrentSignature: implementProgressSignatureRecordFromSignature(signature),
			CurrentHeadSHA:   signature.HeadSHA,
		},
	})
}

func implementProgressRecordFromCompletion(t *testing.T, completion store.WorkAttemptCompletion) implementProgressRecord {
	t.Helper()

	attempt := store.WorkAttempt{
		TerminalState:      completion.TerminalState,
		WorkerMetadataJSON: completion.WorkerMetadataJSON,
	}
	record, ok := implementProgressRecordFromAttempt(attempt)
	if !ok {
		t.Fatalf("completion metadata did not include progress record: %s", completion.WorkerMetadataJSON)
	}
	return record
}

type implementProgressConnector struct {
	hydrated         connector.Issue
	refreshed        connector.Issue
	hydrateErr       error
	hydrateErrs      []error
	refreshErr       error
	hydrations       int
	updates          []implementProgressUpdate
	comments         []implementProgressComment
	relabelStarted   chan autoPromoteTickRelabel
	relabelRelease   chan struct{}
	resolvedBlockers []connector.Issue
}

type implementProgressUpdate struct {
	issueID string
	state   string
}

type implementProgressComment struct {
	issueID string
	body    string
}

func (c *implementProgressConnector) Name() string {
	return "implement-progress"
}

func (c *implementProgressConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c *implementProgressConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *implementProgressConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	if c.refreshErr != nil {
		return nil, c.refreshErr
	}
	if strings.TrimSpace(c.refreshed.ID) == "" {
		return nil, nil
	}
	return []connector.Issue{cloneIssue(c.refreshed)}, nil
}

func (c *implementProgressConnector) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	return cloneIssueComments(c.refreshed.Comments), nil
}

func (c *implementProgressConnector) FetchIssueStatesByIdentifiers(context.Context, []string) ([]connector.Issue, error) {
	return cloneIssues(c.resolvedBlockers), nil
}

func (c *implementProgressConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, implementProgressComment{issueID: issueID, body: body})
	return nil
}

func (c *implementProgressConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, implementProgressUpdate{issueID: issueID, state: state})
	return nil
}

func (c *implementProgressConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *implementProgressConnector) SetField(context.Context, string, string, string) error {
	return nil
}

func (c *implementProgressConnector) HydratePullRequest(context.Context, connector.Issue) (connector.Issue, error) {
	c.hydrations++
	if c.hydrations <= len(c.hydrateErrs) && c.hydrateErrs[c.hydrations-1] != nil {
		return connector.Issue{}, c.hydrateErrs[c.hydrations-1]
	}
	if c.hydrateErr != nil {
		return connector.Issue{}, c.hydrateErr
	}
	return cloneIssue(c.hydrated), nil
}

func (c *implementProgressConnector) ReapplyPullRequestLabel(ctx context.Context, repository string, number int, label string, stagger time.Duration) error {
	relabel := autoPromoteTickRelabel{repository: repository, number: number, label: label, stagger: stagger}
	if c.relabelStarted != nil {
		c.relabelStarted <- relabel
	}
	if c.relabelRelease != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.relabelRelease:
		}
	}
	return nil
}

type implementProgressAttemptStore struct {
	history      []store.WorkAttempt
	historyErr   error
	completions  []store.WorkAttemptCompletion
	historyCalls int
	queries      []store.WorkAttemptHistoryQuery
}

func (s *implementProgressAttemptStore) StartWorkAttempt(context.Context, store.WorkAttemptStart) (int64, error) {
	return 1, nil
}

func (s *implementProgressAttemptStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	return store.WorkAttempt{}, store.ErrNotFound
}

func (s *implementProgressAttemptStore) RecordWorkAttemptHeartbeat(context.Context, store.WorkAttemptHeartbeat) error {
	return nil
}

func (s *implementProgressAttemptStore) CompleteWorkAttempt(_ context.Context, attrs store.WorkAttemptCompletion) error {
	s.completions = append(s.completions, attrs)
	return nil
}

func (s *implementProgressAttemptStore) ListActiveWorkAttempts(context.Context, store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *implementProgressAttemptStore) ListRecentTerminalWorkAttempts(_ context.Context, query store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	s.historyCalls++
	s.queries = append(s.queries, query)
	return append([]store.WorkAttempt(nil), s.history...), s.historyErr
}

func (s *implementProgressAttemptStore) TimeoutExpiredWorkAttempts(context.Context, store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *implementProgressAttemptStore) ReclaimActiveWorkAttempts(context.Context, store.WorkAttemptReclaim) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *implementProgressAttemptStore) RecordSchedulerDecision(context.Context, store.SchedulerDecision) (int64, error) {
	return 0, nil
}

func (s *implementProgressAttemptStore) ListRecentSchedulerDecisions(context.Context, store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return nil, nil
}

func slicesEqual(left []string, right []string) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
