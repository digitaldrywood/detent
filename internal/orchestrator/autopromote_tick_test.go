package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestTickAutoPromoteHumanReviewIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	recentReview := now.Add(-30 * time.Second)

	tests := []struct {
		name                 string
		cfg                  AutoPromoteConfig
		issue                connector.Issue
		wantUpdates          []autoPromoteTickUpdate
		wantCommentFragments []string
		wantLogFragments     []string
		rejectLogFragments   []string
	}{
		{
			name: "promotes ready issue to merging",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-ready", []string{"bug"}, &connector.PullRequest{
				Number:                 42,
				URL:                    "https://github.test/digitaldrywood/detent/pull/42",
				State:                  "OPEN",
				CIStatus:               "success",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-ready",
				state:   "Merging",
			}},
			wantCommentFragments: []string{
				"Auto-promoted this issue from Human Review to Merging.",
				"reason: ready",
				"https://github.test/digitaldrywood/detent/pull/42",
			},
		},
		{
			name: "linked pull request without required automated review waits for review",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-linked-missing-review", []string{"bug"}, &connector.PullRequest{
				Number:     390,
				URL:        "https://github.test/digitaldrywood/detent/pull/390",
				BranchName: "detent/detent-digitaldrywood_detent_387-29d3e4765f21",
				State:      "OPEN",
				CIStatus:   "pass",
			}),
			wantLogFragments: []string{
				"reason=automated_review_missing",
			},
			rejectLogFragments: []string{
				"reason=missing_pull_request",
			},
		},
		{
			name: "degraded pull request hydration waits",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-degraded-pr", []string{"bug"}, &connector.PullRequest{
				Number:                  391,
				URL:                     "https://github.test/digitaldrywood/detent/pull/391",
				State:                   "OPEN",
				MergeableState:          "clean",
				CIStatus:                "success",
				CodexReviewState:        "COMMENTED",
				CodexReviewSubmittedAt:  &oldReview,
				HydrationDegradedReason: connector.PullRequestHydrationReasonStaleCachedPullData,
			}),
			wantLogFragments: []string{
				"reason=pull_request_hydration_unavailable",
				"pull_request_hydration_degraded_reason=stale_cached_pull_request",
			},
			rejectLogFragments: []string{
				"target_state=Merging",
			},
		},
		{
			name: "routes P1 findings to rework",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				ReworkLimit:   0,
			},
			issue: autoPromoteTickIssue("issue-p1", []string{"bug"}, &connector.PullRequest{
				Number:                 43,
				URL:                    "https://github.test/digitaldrywood/detent/pull/43",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "P1",
				CodexReviewSubmittedAt: &oldReview,
				CodexReviewFindings: []connector.PullRequestFinding{{
					Body: "![P1 Badge](https://example.test/p1.svg) Unsafe migration.",
					URL:  "https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
				}},
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-p1",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework.",
				"reason: p1_findings",
				"Unsafe migration.",
				"https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
			},
		},
		{
			name: "routes failing ci to rework by default",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-red-ci", []string{"bug"}, &connector.PullRequest{
				Number:   48,
				URL:      "https://github.test/digitaldrywood/detent/pull/48",
				State:    "OPEN",
				CIStatus: "fail",
				SlowChecks: []connector.PullRequestCheck{
					{Name: "browser-e2e", Status: "completed", Conclusion: "failure"},
					{Name: "lint", Status: "completed", Conclusion: "failure"},
					{Name: "backend", Status: "completed", Conclusion: "success"},
				},
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-red-ci",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework: current-head CI is failing.",
				"reason: ci_not_green",
				"ci_status: red",
				"failed_checks: browser-e2e, lint",
				"https://github.test/digitaldrywood/detent/pull/48",
			},
			wantLogFragments: []string{
				"failed_checks=\"browser-e2e, lint\"",
			},
		},
		{
			name: "routes conflicting pull request to rework",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 0,
				Gate: gate.Config{
					Kind:            gate.KindCommand,
					CIFailureAction: gate.CIFailureActionRework,
				},
			},
			issue: autoPromoteTickIssue("issue-conflicting-pr", []string{"bug"}, &connector.PullRequest{
				Number:         49,
				URL:            "https://github.test/digitaldrywood/detent/pull/49",
				State:          "OPEN",
				MergeableState: "dirty",
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-conflicting-pr",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework: linked PR has merge conflicts.",
				"reason: merge_conflicts",
				"https://github.test/digitaldrywood/detent/pull/49",
			},
			wantLogFragments: []string{
				"reason=merge_conflicts",
				"target_state=Rework",
			},
		},
		{
			name: "waits for quiet period",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-recent", []string{"bug"}, &connector.PullRequest{
				Number:                 44,
				URL:                    "https://github.test/digitaldrywood/detent/pull/44",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &recentReview,
			}),
		},
		{
			name: "skips closed pull request",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-closed-pr", []string{"bug"}, &connector.PullRequest{
				Number:                 47,
				URL:                    "https://github.test/digitaldrywood/detent/pull/47",
				State:                  "CLOSED",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
		{
			name: "honors evaluator label filters",
			cfg: AutoPromoteConfig{
				Enabled:            true,
				QuietDuration:      10 * time.Minute,
				AllowedIssueLabels: []string{"release"},
			},
			issue: autoPromoteTickIssue("issue-label", []string{"bug"}, &connector.PullRequest{
				Number:                 45,
				URL:                    "https://github.test/digitaldrywood/detent/pull/45",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
		{
			name: "disabled config does not evaluate",
			cfg: AutoPromoteConfig{
				Enabled:       false,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-disabled", []string{"bug"}, &connector.PullRequest{
				Number:                 46,
				URL:                    "https://github.test/digitaldrywood/detent/pull/46",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				AutoPromote:         tt.cfg,
				ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			mergingSlot := dispatchTestIssue("issue-merging-slot", "Merging")
			state.Running[mergingSlot.ID] = Running{Issue: mergingSlot}
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{tt.issue}}
			var logs strings.Builder
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			}

			orch.tick(context.Background(), &state, now)

			if !reflect.DeepEqual(tracker.updates, tt.wantUpdates) {
				t.Fatalf("updates = %#v, want %#v", tracker.updates, tt.wantUpdates)
			}
			if len(tracker.fetchByStatesRequests) != 1 {
				t.Fatalf("FetchIssuesByStates() calls = %d, want 1", len(tracker.fetchByStatesRequests))
			}
			if !autoPromoteTickStatesEqual(tracker.fetchByStatesRequests[0], []string{"Blocked", "Human Review"}) {
				t.Fatalf("FetchIssuesByStates() states = %#v, want Blocked/Human Review", tracker.fetchByStatesRequests[0])
			}
			if len(tt.wantCommentFragments) == 0 {
				if len(tracker.comments) != 0 {
					t.Fatalf("comments = %#v, want none", tracker.comments)
				}
			} else {
				if len(tracker.comments) != 1 {
					t.Fatalf("comments = %#v, want one comment", tracker.comments)
				}
				for _, fragment := range tt.wantCommentFragments {
					if !strings.Contains(tracker.comments[0].body, fragment) {
						t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
					}
				}
			}
			for _, fragment := range tt.wantLogFragments {
				if !strings.Contains(logs.String(), fragment) {
					t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
				}
			}
			for _, fragment := range tt.rejectLogFragments {
				if strings.Contains(logs.String(), fragment) {
					t.Fatalf("logs %q contain rejected fragment %q", logs.String(), fragment)
				}
			}
		})
	}
}

func TestTickAutoPromoteCompletedActiveIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)

	tests := []struct {
		name                 string
		issue                connector.Issue
		wantUpdates          []autoPromoteTickUpdate
		wantCommentFragments []string
	}{
		{
			name: "promotes active completed issue directly to merging",
			issue: func() connector.Issue {
				issue := autoPromoteTickIssue("issue-active-ready", []string{"bug"}, &connector.PullRequest{
					Number:                 142,
					URL:                    "https://github.test/digitaldrywood/detent/pull/142",
					State:                  "OPEN",
					CIStatus:               "success",
					CodexReviewState:       "COMMENTED",
					CodexReviewSubmittedAt: &oldReview,
				})
				issue.State = "In Progress"
				return issue
			}(),
			wantUpdates: []autoPromoteTickUpdate{{issueID: "issue-active-ready", state: "Merging"}},
			wantCommentFragments: []string{
				"Auto-promoted this issue from In Progress to Merging.",
				"reason: ready",
				"https://github.test/digitaldrywood/detent/pull/142",
			},
		},
		{
			name: "promotes completed todo issue directly to merging",
			issue: func() connector.Issue {
				issue := autoPromoteTickIssue("issue-todo-ready", []string{"bug"}, &connector.PullRequest{
					Number:                 145,
					URL:                    "https://github.test/digitaldrywood/detent/pull/145",
					State:                  "OPEN",
					CIStatus:               "success",
					CodexReviewState:       "COMMENTED",
					CodexReviewSubmittedAt: &oldReview,
				})
				issue.State = "Todo"
				return issue
			}(),
			wantUpdates: []autoPromoteTickUpdate{{issueID: "issue-todo-ready", state: "Merging"}},
			wantCommentFragments: []string{
				"Auto-promoted this issue from Todo to Merging.",
				"reason: ready",
				"https://github.test/digitaldrywood/detent/pull/145",
			},
		},
		{
			name: "routes active completed issue directly to rework",
			issue: func() connector.Issue {
				issue := autoPromoteTickIssue("issue-active-rework", []string{"bug"}, &connector.PullRequest{
					Number:                 143,
					URL:                    "https://github.test/digitaldrywood/detent/pull/143",
					State:                  "OPEN",
					CIStatus:               "failure",
					CodexReviewState:       "COMMENTED",
					CodexReviewSubmittedAt: &oldReview,
				})
				issue.State = "In Progress"
				return issue
			}(),
			wantUpdates: []autoPromoteTickUpdate{{issueID: "issue-active-rework", state: "Rework"}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from In Progress to Rework: current-head CI is failing.",
				"reason: ci_not_green",
				"https://github.test/digitaldrywood/detent/pull/143",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				AutoPromote: AutoPromoteConfig{
					Enabled:       true,
					QuietDuration: 0,
					Gate:          gate.Config{Kind: gate.KindCommand},
				},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			state.Completed[tt.issue.ID] = Completed{
				Issue:      tt.issue,
				FinalState: FinalStateCompleted,
			}
			mergingSlot := dispatchTestIssue(tt.issue.ID+"-merging-slot", "Merging")
			state.Running[mergingSlot.ID] = Running{Issue: mergingSlot}
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{tt.issue}}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.tick(context.Background(), &state, now)

			if !reflect.DeepEqual(tracker.updates, tt.wantUpdates) {
				t.Fatalf("updates = %#v, want %#v", tracker.updates, tt.wantUpdates)
			}
			if len(tracker.comments) != 1 {
				t.Fatalf("comments = %#v, want one auto-promote audit comment", tracker.comments)
			}
			for _, fragment := range tt.wantCommentFragments {
				if !strings.Contains(tracker.comments[0].body, fragment) {
					t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
				}
			}
			if _, ok := state.Completed[tt.issue.ID]; ok {
				t.Fatalf("Completed[%q] present after auto-promote transition", tt.issue.ID)
			}
		})
	}
}

func TestTickAutoPromoteRecoversActiveIssueAfterRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 12, 30, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-restart-gate-pending", []string{"bug"}, &connector.PullRequest{
		Number:                 144,
		URL:                    "https://github.test/digitaldrywood/detent/pull/144",
		State:                  "OPEN",
		CIStatus:               "pending",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	issue.State = "In Progress"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			Gate:          gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	mergingSlot := dispatchTestIssue("issue-restart-merging-slot", "Merging")
	state.Running[mergingSlot.ID] = Running{Issue: mergingSlot}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	prNumber := int64(issue.PullRequest.Number)
	attempts := &recordingWorkAttemptStore{history: []store.WorkAttempt{{
		ProjectID:          cfg.Project.ID,
		IssueID:            issue.ID,
		Identifier:         issue.Identifier,
		IssueURL:           issue.URL,
		PRNumber:           &prNumber,
		WorkerType:         "agent",
		Status:             store.WorkAttemptStatusTerminal,
		StartedAt:          now.Add(-15 * time.Minute),
		CompletedAt:        now.Add(-10 * time.Minute),
		TerminalState:      store.WorkAttemptTerminalSuccess,
		WorkerMetadataJSON: implementProgressMetadataJSON(autoPromoteReworkSignature{PRNumber: prNumber, HeadSHA: "restart-head"}, store.WorkAttemptTerminalSuccess),
	}}}
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates with pending CI = %#v, want none", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments with pending CI = %#v, want none", tracker.comments)
	}
	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after pending restart recovery tick", issue.ID)
	}
	if completed, ok := state.Completed[issue.ID]; !ok || completed.FinalState != FinalStateCompleted {
		t.Fatalf("Completed[%q] = %#v, want durable successful completion restored", issue.ID, completed)
	}
	if len(attempts.historyQueries) == 0 {
		t.Fatal("durable work attempt history was not queried")
	}

	tracker.stateIssues[0].PullRequest.CIStatus = "success"
	orch.tick(context.Background(), &state, now.Add(time.Minute))

	wantUpdates := []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates after CI pass = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments after CI pass = %#v, want one auto-promote audit comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promoted this issue from In Progress to Merging.",
		"reason: ready",
		"https://github.test/digitaldrywood/detent/pull/144",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
}

func TestLatestSuccessfulGateWaitAttemptRequiresCurrentImplementationEvidence(t *testing.T) {
	t.Parallel()

	currentPR := int64(144)
	otherPR := int64(143)
	currentSignature := autoPromoteReworkSignature{PRNumber: currentPR, HeadSHA: "current-head"}
	otherSignature := autoPromoteReworkSignature{PRNumber: otherPR, HeadSHA: "other-head"}
	issue := autoPromoteTickIssue("issue-gate-wait-evidence", []string{"bug"}, &connector.PullRequest{
		Number: 144,
		State:  "OPEN",
	})
	issue.State = "In Progress"

	tests := []struct {
		name     string
		attempts []store.WorkAttempt
		wantID   int64
	}{
		{
			name: "ignores plan success",
			attempts: []store.WorkAttempt{{
				ID:                 1,
				TerminalState:      store.WorkAttemptTerminalSuccess,
				WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"run_mode": runpkg.RunModePlan}),
			}},
		},
		{
			name: "ignores implementation success without PR association",
			attempts: []store.WorkAttempt{{
				ID:                 2,
				TerminalState:      store.WorkAttemptTerminalSuccess,
				WorkerMetadataJSON: implementProgressMetadataJSON(autoPromoteReworkSignature{}, store.WorkAttemptTerminalSuccess),
			}},
		},
		{
			name: "ignores implementation success for another PR",
			attempts: []store.WorkAttempt{{
				ID:                 3,
				PRNumber:           &otherPR,
				TerminalState:      store.WorkAttemptTerminalSuccess,
				WorkerMetadataJSON: implementProgressMetadataJSON(otherSignature, store.WorkAttemptTerminalSuccess),
			}},
		},
		{
			name: "accepts current PR from attempt",
			attempts: []store.WorkAttempt{{
				ID:                 4,
				PRNumber:           &currentPR,
				TerminalState:      store.WorkAttemptTerminalSuccess,
				WorkerMetadataJSON: implementProgressMetadataJSON(autoPromoteReworkSignature{}, store.WorkAttemptTerminalSuccess),
			}},
			wantID: 4,
		},
		{
			name: "accepts current PR from completion record",
			attempts: []store.WorkAttempt{{
				ID:                 5,
				TerminalState:      store.WorkAttemptTerminalSuccess,
				WorkerMetadataJSON: implementProgressMetadataJSON(currentSignature, store.WorkAttemptTerminalSuccess),
			}},
			wantID: 5,
		},
		{
			name: "skips newer unrelated success for older current completion",
			attempts: []store.WorkAttempt{
				{
					ID:                 6,
					TerminalState:      store.WorkAttemptTerminalSuccess,
					WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"run_mode": runpkg.RunModePlan}),
				},
				{
					ID:                 5,
					TerminalState:      store.WorkAttemptTerminalSuccess,
					WorkerMetadataJSON: implementProgressMetadataJSON(currentSignature, store.WorkAttemptTerminalSuccess),
				},
			},
			wantID: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			attempts := &recordingWorkAttemptStore{history: tt.attempts}
			orch := &Orchestrator{
				cfg:          normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}}),
				workAttempts: attempts,
			}

			attempt, ok, err := orch.latestSuccessfulGateWaitAttempt(context.Background(), issue)
			if err != nil {
				t.Fatalf("latestSuccessfulGateWaitAttempt() error = %v", err)
			}
			if ok != (tt.wantID > 0) {
				t.Fatalf("latestSuccessfulGateWaitAttempt() ok = %v, want %v", ok, tt.wantID > 0)
			}
			if attempt.ID != tt.wantID {
				t.Fatalf("latestSuccessfulGateWaitAttempt() ID = %d, want %d", attempt.ID, tt.wantID)
			}
		})
	}
}

func TestTickAutoPromotesCompletedGateWaitWhileDispatchRunning(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 18, 50, 0, 0, time.UTC)
	oldReview := now.Add(-10 * time.Minute)
	issue := autoPromoteTickIssue("issue-running-gate-wait", []string{"bug"}, &connector.PullRequest{
		Number:                 1125,
		URL:                    "https://github.test/digitaldrywood/detent/pull/1125",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	issue.State = "In Progress"
	issue.Comments = []connector.IssueComment{{
		Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```",
	}}
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:         true,
			QuietDuration:   0,
			GateWaitState:   autoPromoteGateWaitSource,
			NoProgressLimit: 3,
			Gate: gate.Config{
				Kind:                   gate.KindCommand,
				RequireAutomatedReview: new(false),
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-5 * time.Minute),
		FinalState:  FinalStateCompleted,
	}
	state.Running[issue.ID] = Running{
		Issue:     issue,
		StartedAt: now.Add(-time.Minute),
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	result := orch.autoPromoteHumanReviewIssues(context.Background(), &state, []connector.Issue{issue}, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want %s", result.transitioned, issue.ID)
	}
	if _, ok := state.Running[issue.ID]; !ok {
		t.Fatalf("Running[%q] missing; promotion must not wait for stale dispatch completion", issue.ID)
	}
	if _, ok := state.Blocked[issue.ID]; ok {
		t.Fatalf("Blocked[%q] present after gate promotion", issue.ID)
	}
}

func TestTickAutoPromoteHydratesWorkpadBlockerBeforeTransition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 6, 15, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-workpad-blocker", []string{"bug"}, &connector.PullRequest{
		Number:                 185,
		URL:                    "https://github.test/digitaldrywood/creswoodcorners-phone/pull/185",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	mergingSlot := dispatchTestIssue("issue-merging-slot", "Merging")
	state.Running[mergingSlot.ID] = Running{Issue: mergingSlot}
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {{
				Body: "## Codex Workpad\n\n### Blockers\n- no generated seasonal MP3s were copied into `assets/audio/`\n- Gate A/B/C owner listening approval is still required before approved audio assets are copied and committed",
			}},
		},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no transition while Workpad blocker is present", tracker.updates)
	}
	if got, want := tracker.fetchComments, []string{issue.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueComments issue IDs = %#v, want %#v", got, want)
	}
	for _, fragment := range []string{
		"action=await_review",
		"reason=workpad_blocker",
		"workpad_blocker=\"no generated seasonal MP3s were copied into `assets/audio/`; Gate A/B/C owner listening approval is still required before approved audio assets are copied and committed\"",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestTickAutoPromoteFetchesStructuredWorkpadOverStaleBlockerReason(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-stale-blocker-reason", []string{"bug"}, &connector.PullRequest{
		Number:                 1494,
		URL:                    "https://github.test/digitaldrywood/detent/pull/1494",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	issue.BlockerReason = "Blocked by: #1462 stale issue-body prose"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {{
				Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```",
				URL:  "https://github.test/comment/structured-complete",
			}},
		},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	result := orch.autoPromoteHumanReviewIssues(context.Background(), &state, []connector.Issue{issue}, now)

	if got, want := tracker.fetchComments, []string{issue.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueComments issue IDs = %#v, want %#v", got, want)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want %s", result.transitioned, issue.ID)
	}
	for _, fragment := range []string{
		"reason=ready",
		"workpad_signal_source=structured",
		"workpad_comment_url=https://github.test/comment/structured-complete",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if strings.Contains(logs.String(), "reason=workpad_blocker") {
		t.Fatalf("logs %q contain stale workpad_blocker decision", logs.String())
	}
}

func TestTickAutoPromoteResolvesClosedWorkpadBlockerBeforeTransition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 22, 30, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-resolved-workpad-blocker", []string{"bug"}, &connector.PullRequest{
		Number:                 1480,
		URL:                    "https://github.test/digitaldrywood/pyroapex/pull/1480",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	issue.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#1462"}}
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	mergingSlot := dispatchTestIssue("issue-merging-slot", "Merging")
	state.Running[mergingSlot.ID] = Running{Issue: mergingSlot}
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {{
				Body: "## Codex Workpad\n\n### Blockers\n- Blocked by: #1462\n\n### Validation\n- make check passed.",
			}},
		},
		resolvedIssues: []connector.Issue{{
			ID:         "issue-1462",
			Identifier: "digitaldrywood/detent#1462",
			State:      "Done",
		}},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	result := orch.autoPromoteHumanReviewIssues(context.Background(), &state, []connector.Issue{issue}, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want %s", result.transitioned, issue.ID)
	}
	if got, want := tracker.fetchIdentifiers, [][]string{{"digitaldrywood/detent#1462"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueStatesByIdentifiers = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one auto-promote audit comment", tracker.comments)
	}
	for _, fragment := range []string{
		"action=promote",
		"reason=ready",
		"resolved_workpad_blockers=digitaldrywood/detent#1462",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if strings.Contains(logs.String(), "reason=workpad_blocker") {
		t.Fatalf("logs %q contain workpad_blocker after resolved dependency hydration", logs.String())
	}
}

func TestTickAutoPromoteResolvesMergedWorkpadBlockerBeforeTransition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 22, 35, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-merged-workpad-blocker", []string{"bug"}, &connector.PullRequest{
		Number:                 1481,
		URL:                    "https://github.test/digitaldrywood/pyroapex/pull/1481",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {{
				Body: "## Codex Workpad\n\n### Blockers\n- Blocked by: #1462\n\n### Validation\n- make check passed.",
			}},
		},
		resolvedIssues: []connector.Issue{{
			ID:          "issue-1462",
			Identifier:  "digitaldrywood/detent#1462",
			State:       "In Progress",
			PullRequest: &connector.PullRequest{Number: 1482, State: "MERGED"},
		}},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	result := orch.autoPromoteHumanReviewIssues(context.Background(), &state, []connector.Issue{issue}, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want %s", result.transitioned, issue.ID)
	}
	if !strings.Contains(logs.String(), "resolved_workpad_blockers=digitaldrywood/detent#1462") {
		t.Fatalf("logs %q missing resolved_workpad_blockers", logs.String())
	}
}

func TestTickAutoPromoteCommentsOnceForInvalidStructuredWorkpad(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-invalid-workpad-status", []string{"bug"}, &connector.PullRequest{
		Number:                 1490,
		URL:                    "https://github.test/digitaldrywood/detent/pull/1490",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	invalidWorkpad := connector.IssueComment{
		Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: null\n```",
		URL:  "https://github.test/comment/invalid-workpad",
	}
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {invalidWorkpad},
		},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	result := orch.autoPromoteHumanReviewIssues(context.Background(), &State{}, []connector.Issue{issue}, now)

	if len(result.transitioned) != 0 {
		t.Fatalf("transitioned = %#v, want no transition", result.transitioned)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one invalid Workpad comment", tracker.comments)
	}
	for _, fragment := range []string{
		"<!-- detent-workpad-status-invalid:",
		"status blocked requires at least one blocker ref or human_action",
		"https://github.test/comment/invalid-workpad",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("invalid comment %q missing %q", tracker.comments[0].body, fragment)
		}
	}
	for _, fragment := range []string{
		"reason=workpad_status_invalid",
		"workpad_signal_source=structured",
		"workpad_comment_url=https://github.test/comment/invalid-workpad",
		"workpad_status_hash=",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}

	tracker.issueComments[issue.ID] = append(tracker.issueComments[issue.ID], connector.IssueComment{Body: tracker.comments[0].body})
	orch.autoPromoteHumanReviewIssues(context.Background(), &State{}, []connector.Issue{issue}, now)
	if len(tracker.comments) != 1 {
		t.Fatalf("comments after dedupe = %#v, want still one invalid Workpad comment", tracker.comments)
	}
}

func TestTickAutoPromoteRoutesSuccessfulInvalidStructuredWorkpadToRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 13, 30, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-invalid-workpad-success", []string{"bug"}, &connector.PullRequest{
		Number:                 1491,
		URL:                    "https://github.test/digitaldrywood/detent/pull/1491",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {{
				Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: human-review\nblockers: []\nhuman_action: null\n```",
				URL:  "https://github.test/comment/invalid-status-value",
			}},
		},
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	result := orch.autoPromoteHumanReviewIssues(context.Background(), &state, []connector.Issue{issue}, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want %s", result.transitioned, issue.ID)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one rework comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promote routed this issue from Human Review to Rework",
		"reason: workpad_status_invalid",
		"human-review",
		"in_progress, blocked, complete",
		"https://github.test/comment/invalid-status-value",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
}

func TestTickAutoPromoteBlocksWhenReworkLimitReached(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 2, 16, 10, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-rework-limit", []string{"bug"}, &connector.PullRequest{
		Number:                 43,
		URL:                    "https://github.test/digitaldrywood/detent/pull/43",
		State:                  "OPEN",
		CIStatus:               "pass",
		CodexReviewState:       "P1",
		CodexReviewSubmittedAt: &oldReview,
		CodexReviewFindings: []connector.PullRequestFinding{{
			Body: "![P1 Badge](https://example.test/p1.svg) Unsafe migration.",
			URL:  "https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
		}},
	})
	issue.Identifier = "digitaldrywood/detent#857"
	issue.URL = "https://github.test/digitaldrywood/detent/issues/857"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			ReworkLimit:   1,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    "Rework",
		Reason:       string(AutoPromoteReasonP1Findings),
		Status:       "entered",
		StartedAt:    now.Add(-2 * time.Hour),
		MetadataJSON: "{}",
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	orch := &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		workflowMetrics: metrics,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Blocked"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one Blocked handoff comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promote routed this issue from Human Review to Blocked because the Rework limit was reached.",
		"rework_limit: 1",
		"prior_rework_transitions: 1",
		"current_rework_reason: p1_findings",
		"repeated_rework_reasons: p1_findings x1",
		"Unsafe migration.",
		"https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	events := metrics.snapshot()
	if len(events) != 3 {
		t.Fatalf("workflow metric events = %#v, want prior Rework plus exit/Blocked enter", events)
	}
	blocked := events[2]
	if blocked.PhaseName != "Blocked" || blocked.Status != "entered" || blocked.Reason != "rework_limit" {
		t.Fatalf("blocked metric = %#v, want Blocked entered with rework_limit reason", blocked)
	}
}

func TestTickAutoPromoteBlocksRepeatedCINotGreenSignature(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 14, 30, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-red-ci-loop", []string{"bug"}, &connector.PullRequest{
		Number:   1046,
		URL:      "https://github.test/digitaldrywood/detent/pull/1046",
		State:    "OPEN",
		HeadSHA:  "same-head",
		CIStatus: "fail",
		RequiredCheckFailures: []connector.PullRequestCheck{
			{Name: "Test", Status: "completed", Conclusion: "failure"},
			{Name: "Tier-1 Race Tests", Status: "completed", Conclusion: "failure"},
		},
	})
	issue.Identifier = "digitaldrywood/detent#1046"
	issue.URL = "https://github.test/digitaldrywood/detent/issues/1046"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			ReworkLimit:   1,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	prNumber := int64(1046)
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PRNumber:     &prNumber,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    "Rework",
		Reason:       string(AutoPromoteReasonCINotGreen),
		Status:       "entered",
		StartedAt:    now.Add(-time.Hour),
		MetadataJSON: autoPromoteReworkEventMetadata(1046, "same-head", "Test", "Tier-1 Race Tests"),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	orch := &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		workflowMetrics: metrics,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Blocked"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one Blocked handoff comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Rework limit was reached",
		"prior_rework_transitions: 1",
		"current_rework_reason: ci_not_green",
		"repeated_rework_reasons: ci_not_green x1",
		"failed_checks: Test, Tier-1 Race Tests",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	events := metrics.snapshot()
	blocked := events[len(events)-1]
	if blocked.PhaseName != "Blocked" || blocked.Reason != "rework_limit" {
		t.Fatalf("latest workflow event = %#v, want Blocked rework_limit entry", blocked)
	}
}

func TestTickAutoPromoteResetsReworkLimitAfterHeadSHAChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 14, 45, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-red-ci-new-head", []string{"bug"}, &connector.PullRequest{
		Number:   1047,
		URL:      "https://github.test/digitaldrywood/detent/pull/1047",
		State:    "OPEN",
		HeadSHA:  "new-head",
		CIStatus: "fail",
		RequiredCheckFailures: []connector.PullRequestCheck{{
			Name:       "Test",
			Status:     "completed",
			Conclusion: "failure",
		}},
	})
	issue.Identifier = "digitaldrywood/detent#1047"
	issue.URL = "https://github.test/digitaldrywood/detent/issues/1047"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			ReworkLimit:   1,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	prNumber := int64(1047)
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PRNumber:     &prNumber,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    "Rework",
		Reason:       string(AutoPromoteReasonCINotGreen),
		Status:       "entered",
		StartedAt:    now.Add(-time.Hour),
		MetadataJSON: autoPromoteReworkEventMetadata(1047, "old-head", "Test"),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	orch := &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		workflowMetrics: metrics,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 || strings.Contains(tracker.comments[0].body, "Rework limit was reached") {
		t.Fatalf("comments = %#v, want ordinary Rework handoff", tracker.comments)
	}
	events := metrics.snapshot()
	rework := events[len(events)-1]
	signature := autoPromoteReworkSignatureFromEvent(rework)
	if rework.PhaseName != "Rework" || signature.HeadSHA != "new-head" || !slices.Equal(signature.FailedChecks, []string{"Test"}) {
		t.Fatalf("latest Rework event = %#v signature = %#v, want new-head Test signature", rework, signature)
	}
}

func TestTickRetriesTransientHumanReviewCIBeforeRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 14, 5, 0, 0, time.UTC)
	reviewedAt := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-transient-ci", []string{"bug"}, &connector.PullRequest{
		Number:                 51,
		URL:                    "https://github.test/digitaldrywood/detent/pull/51",
		State:                  "OPEN",
		HeadSHA:                "head-transient",
		CIStatus:               "fail",
		BranchName:             "detent/transient-ci",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &reviewedAt,
		TransientFailedChecks: []connector.PullRequestCheck{{
			ID:            9001,
			WorkflowRunID: 8001,
			Name:          "Checks",
			Status:        "completed",
			Conclusion:    "failure",
			DetailsURL:    "https://github.test/digitaldrywood/detent/actions/runs/8001/job/9001",
		}},
	})
	retryLimit := 2
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled: true,
			Gate: gate.Config{
				Kind:                  gate.KindCommand,
				CIFailureAction:       gate.CIFailureActionRework,
				TransientCIRetryLimit: &retryLimit,
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{issue},
		candidateIssuesSet: true,
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if len(tracker.reruns) != 1 || tracker.reruns[0].issueID != issue.ID || len(tracker.reruns[0].checks) != 1 {
		t.Fatalf("reruns = %#v, want one rerun for transient check", tracker.reruns)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no Rework transition while retrying transient CI", tracker.updates)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "Transient CI failure detected") {
		t.Fatalf("comments = %#v, want transient CI retry audit comment", tracker.comments)
	}
}

func TestTickDoesNotRetryTransientHumanReviewCIBeforeGateBlocksOnCI(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 14, 6, 0, 0, time.UTC)
	reviewedAt := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-transient-ci-waiting-review", []string{"bug"}, &connector.PullRequest{
		Number:                 53,
		URL:                    "https://github.test/digitaldrywood/detent/pull/53",
		State:                  "OPEN",
		HeadSHA:                "head-transient",
		CIStatus:               "fail",
		BranchName:             "detent/transient-ci",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &reviewedAt,
		TransientFailedChecks: []connector.PullRequestCheck{{
			ID:            9003,
			WorkflowRunID: 8003,
			Name:          "Checks",
			Status:        "completed",
			Conclusion:    "failure",
			DetailsURL:    "https://github.test/digitaldrywood/detent/actions/runs/8003/job/9003",
		}},
	})
	retryLimit := 2
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:            true,
			AllowedIssueLabels: []string{"allowed"},
			Gate: gate.Config{
				Kind:                  gate.KindCommand,
				CIFailureAction:       gate.CIFailureActionRework,
				TransientCIRetryLimit: &retryLimit,
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{issue},
		candidateIssuesSet: true,
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if len(tracker.reruns) != 0 {
		t.Fatalf("reruns = %#v, want no reruns before CI is the blocking gate reason", tracker.reruns)
	}
	if len(state.TransientCheckRetries) != 0 {
		t.Fatalf("TransientCheckRetries = %#v, want none before CI blocks promotion", state.TransientCheckRetries)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no Rework transition while label is disallowed", tracker.updates)
	}
}

func TestTickRetriesTransientMergingCIBeforeRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 14, 7, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-merging-transient-ci", []string{"bug"}, &connector.PullRequest{
		Number:     52,
		URL:        "https://github.test/digitaldrywood/detent/pull/52",
		State:      "OPEN",
		HeadSHA:    "head-transient",
		CIStatus:   "fail",
		BranchName: "detent/transient-ci",
		TransientFailedChecks: []connector.PullRequestCheck{{
			ID:            9002,
			WorkflowRunID: 8002,
			Name:          "Checks",
			Status:        "completed",
			Conclusion:    "failure",
			DetailsURL:    "https://github.test/digitaldrywood/detent/actions/runs/8002/job/9002",
		}},
	})
	issue.State = "Merging"
	retryLimit := 2
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled: true,
			Gate: gate.Config{
				Kind:                  gate.KindCommand,
				CIFailureAction:       gate.CIFailureActionRework,
				TransientCIRetryLimit: &retryLimit,
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Human Review", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{issue},
		candidateIssuesSet: true,
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if len(tracker.reruns) != 1 || tracker.reruns[0].issueID != issue.ID || len(tracker.reruns[0].checks) != 1 {
		t.Fatalf("reruns = %#v, want one rerun for transient check", tracker.reruns)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no Rework transition while retrying transient CI", tracker.updates)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "Transient CI failure detected") {
		t.Fatalf("comments = %#v, want transient CI retry audit comment", tracker.comments)
	}
}

func TestObservedStatusFetchStatesForTickDoesNotThrottleCustomPassState(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Production Rework",
			Gate: gate.Config{
				Kind: gate.KindArtifact,
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Production Rework"},
		ObservedStates: []string{"Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.Running["issue-merging"] = Running{
		Issue: connector.Issue{
			ID:    "issue-merging",
			State: "Merging",
		},
	}
	orch := Orchestrator{cfg: cfg}

	got := orch.observedStatusFetchStatesForTick(&state)
	want := []string{"Blocked", "Review", "Merging"}
	if !autoPromoteTickStatesEqual(got, want) {
		t.Fatalf("observedStatusFetchStatesForTick() = %#v, want %#v", got, want)
	}
}

func TestTickAutoPromoteLogsNonTransitionDecisionsAtInfo(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 14, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-missing-review", []string{"bug"}, &connector.PullRequest{
		Number:     390,
		URL:        "https://github.test/digitaldrywood/detent/pull/390",
		BranchName: "detent/detent-digitaldrywood_detent_387-29d3e4765f21",
		State:      "OPEN",
		CIStatus:   "pass",
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	for _, fragment := range []string{
		"level=INFO",
		"auto promote decision",
		"action=await_review",
		"reason=automated_review_missing",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestTickAutoPromoteDefersWhenPullRequestHydrationRateLimited(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 7, 24, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	prior := autoPromoteTickIssue("issue-rate-limited-pr", []string{"bug"}, &connector.PullRequest{
		Number:                 77,
		URL:                    "https://github.test/digitaldrywood/creswoodcorners-phone/pull/77",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	prior.Identifier = "digitaldrywood/creswoodcorners-phone#69"
	current := autoPromoteTickIssue("issue-rate-limited-pr", []string{"bug"}, &connector.PullRequest{
		Number:                     77,
		HydrationUnavailableReason: "rate_limited",
	})
	current.Identifier = prior.Identifier
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{current}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	state := newState(cfg)
	state.Pipeline = []connector.Issue{prior}
	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if strings.Contains(logs.String(), "reason=missing_pull_request") {
		t.Fatalf("logs %q contain missing_pull_request", logs.String())
	}
	for _, fragment := range []string{
		"reason=pull_request_hydration_unavailable",
		"pull_request_hydration_reason=rate_limited",
		"pull_request_number=77",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if len(state.Pipeline) != 1 || state.Pipeline[0].PullRequest == nil {
		t.Fatalf("Pipeline = %#v, want retained pull request metadata", state.Pipeline)
	}
	pr := state.Pipeline[0].PullRequest
	if pr.URL != "https://github.test/digitaldrywood/creswoodcorners-phone/pull/77" {
		t.Fatalf("retained PullRequest.URL = %q, want prior URL", pr.URL)
	}
	if pr.HydrationUnavailableReason != "rate_limited" {
		t.Fatalf("retained HydrationUnavailableReason = %q, want rate_limited", pr.HydrationUnavailableReason)
	}
}

func TestLogAutoPromoteDecisionIncludesHydrationReasons(t *testing.T) {
	t.Parallel()

	retryAt := time.Date(2026, 6, 25, 12, 5, 0, 0, time.UTC)
	tests := []struct {
		name        string
		pullRequest *connector.PullRequest
		want        []string
	}{
		{
			name: "primary exhausted",
			pullRequest: &connector.PullRequest{
				Number:                     77,
				HydrationUnavailableReason: connector.PullRequestHydrationReasonPrimaryExhausted,
			},
			want: []string{"pull_request_hydration_reason=primary_exhausted"},
		},
		{
			name: "secondary throttled",
			pullRequest: &connector.PullRequest{
				Number:                     77,
				HydrationUnavailableReason: connector.PullRequestHydrationReasonSecondaryThrottled,
				HydrationNextRetryAt:       &retryAt,
			},
			want: []string{
				"pull_request_hydration_reason=secondary_throttled",
				"pull_request_hydration_next_retry_at=2026-06-25T12:05:00Z",
			},
		},
		{
			name: "rest budget reserved",
			pullRequest: &connector.PullRequest{
				Number:                     77,
				HydrationUnavailableReason: connector.PullRequestHydrationReasonRESTBudgetReserved,
			},
			want: []string{"pull_request_hydration_reason=rest_budget_reserved"},
		},
		{
			name: "stale cached data",
			pullRequest: &connector.PullRequest{
				Number:                  77,
				HydrationDegradedReason: connector.PullRequestHydrationReasonStaleCachedPullData,
				HydrationNextRetryAt:    &retryAt,
			},
			want: []string{
				"pull_request_hydration_degraded_reason=stale_cached_pull_request",
				"pull_request_hydration_next_retry_at=2026-06-25T12:05:00Z",
			},
		},
		{
			name: "stale successful check run",
			pullRequest: &connector.PullRequest{
				Number:   77,
				CIStatus: "pass",
				StaleSuccessfulChecks: []connector.PullRequestCheck{{
					Name:       "Installer Smoke (ubuntu-latest)",
					Status:     "in_progress",
					Conclusion: "success",
				}},
			},
			want: []string{
				"ci_anomaly=stale_successful_check_run",
				"stale_successful_checks=\"Installer Smoke (ubuntu-latest)\"",
				"ci_anomaly_action=treated_completed_successful_check_runs_as_passed",
			},
		},
		{
			name: "review state disagreement",
			pullRequest: &connector.PullRequest{
				Number:                  77,
				CodexReviewAPIState:     "APPROVED",
				CodexReviewBodySeverity: "P1",
			},
			want: []string{
				"review_api_state=APPROVED",
				"review_body_severity=P1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs strings.Builder
			orch := &Orchestrator{
				logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
			}
			issue := autoPromoteTickIssue("issue-hydration-log", []string{"bug"}, tt.pullRequest)
			orch.logAutoPromoteDecision(issue, AutoPromoteDecision{
				Action: AutoPromoteActionSkip,
				Reason: AutoPromoteReasonPullRequestHydrationUnavailable,
			}, "")

			for _, fragment := range tt.want {
				if !strings.Contains(logs.String(), fragment) {
					t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
				}
			}
		})
	}
}

func TestTickReconcilesStaleTodoLinkedPullRequests(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	ready := autoPromoteTickIssue("issue-ready-todo", []string{"bug"}, &connector.PullRequest{
		Number:                 36,
		URL:                    "https://github.test/digitaldrywood/creswoodcorners-phone/pull/36",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	ready.State = "Todo"
	ready.Identifier = "digitaldrywood/creswoodcorners-phone#33"
	conflicting := autoPromoteTickIssue("issue-conflicting-todo", []string{"bug"}, &connector.PullRequest{
		Number:         38,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/38",
		State:          "OPEN",
		MergeableState: "DIRTY",
		CIStatus:       "success",
	})
	conflicting.State = "Todo"
	conflicting.Identifier = "digitaldrywood/creswoodcorners-phone#32"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{ready, conflicting}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{
		{issueID: "issue-ready-todo", state: "Merging"},
		{issueID: "issue-conflicting-todo", state: "Rework"},
	}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if len(tracker.comments) != 2 {
		t.Fatalf("comments = %#v, want stale todo reconciliation comments", tracker.comments)
	}
	wantComments := map[string][]string{
		"issue-ready-todo": {
			"Auto-promoted this issue from Todo to Merging.",
			"reason: ready",
			"https://github.test/digitaldrywood/creswoodcorners-phone/pull/36",
		},
		"issue-conflicting-todo": {
			"Auto-promote routed this issue from Todo to Rework: linked PR has merge conflicts.",
			"reason: merge_conflicts",
			"mergeable_state: dirty",
			"https://github.test/digitaldrywood/creswoodcorners-phone/pull/38",
		},
	}
	for _, comment := range tracker.comments {
		for _, fragment := range wantComments[comment.issueID] {
			if !strings.Contains(comment.body, fragment) {
				t.Fatalf("comment for %s = %q, missing %q", comment.issueID, comment.body, fragment)
			}
		}
	}
	for _, fragment := range []string{
		"stale_todo_pr_reconciled",
		"reason=ready",
		"reason=merge_conflicts",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestTickReconcilesStaleTodoMergedPullRequestToDone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-pyroapex-1462", []string{"bug"}, &connector.PullRequest{
		Number:   1471,
		URL:      "https://github.test/digitaldrywood/pyroapex/pull/1471",
		State:    "MERGED",
		CIStatus: "success",
	})
	issue.State = "Todo"
	issue.Identifier = "digitaldrywood/pyroapex#1462"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	attempts := &recordingWorkAttemptStore{}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{{issueID: issue.ID, state: "Done"}}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one merged PR reconciliation comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Reconciled this issue from Todo to Done because its linked PR is already merged.",
		"reason: pull_request_merged",
		"https://github.test/digitaldrywood/pyroapex/pull/1471",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	if strings.Contains(logs.String(), "skip_reason=duplicate_pull_request_work") {
		t.Fatalf("logs %q contain duplicate_pull_request_work skip", logs.String())
	}
	for _, decision := range attempts.decisions {
		if decision.IssueID == issue.ID && decision.Reason == dispatchSkipDuplicatePullRequest {
			t.Fatalf("scheduler decision = %#v, want merged PR reconciliation instead of duplicate skip", decision)
		}
	}
}

func TestTickParksStaleTodoMergedPullRequestWhenHydrationUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 15, 5, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-pyroapex-merged-stale", []string{"bug"}, &connector.PullRequest{
		Number:                  1472,
		URL:                     "https://github.test/digitaldrywood/pyroapex/pull/1472",
		State:                   "MERGED",
		CIStatus:                "success",
		HydrationDegradedReason: connector.PullRequestHydrationReasonStaleCachedPullData,
	})
	issue.State = "Todo"
	issue.Identifier = "digitaldrywood/pyroapex#1464"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one hydration reconciliation comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Reconciled this issue from Todo to Human Review because linked PR status hydration is unavailable.",
		"reason: pull_request_hydration_unavailable",
		"pull_request_hydration_degraded_reason: stale_cached_pull_request",
		"https://github.test/digitaldrywood/pyroapex/pull/1472",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	for _, fragment := range []string{
		"reason=pull_request_hydration_unavailable",
		"pull_request_hydration_degraded_reason=stale_cached_pull_request",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestTickReconcilesStaleTodoMergedPullRequestWithFailedChecksToRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 15, 10, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-pyroapex-1463", []string{"bug"}, &connector.PullRequest{
		Number:   1470,
		URL:      "https://github.test/digitaldrywood/pyroapex/pull/1470",
		State:    "MERGED",
		CIStatus: "fail",
		RequiredCheckFailures: []connector.PullRequestCheck{{
			Name:       "Tier-1 Race Tests",
			Status:     "completed",
			Conclusion: "failure",
		}},
	})
	issue.State = "Todo"
	issue.Identifier = "digitaldrywood/pyroapex#1463"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	attempts := &recordingWorkAttemptStore{}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one failed merged PR reconciliation comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Reconciled this issue from Todo to Rework because its merged linked PR has failing CI evidence.",
		"reason: ci_not_green",
		"ci_status: fail",
		"failed_checks: Tier-1 Race Tests",
		"https://github.test/digitaldrywood/pyroapex/pull/1470",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	if strings.Contains(logs.String(), "skip_reason=duplicate_pull_request_work") {
		t.Fatalf("logs %q contain duplicate_pull_request_work skip", logs.String())
	}
	for _, decision := range attempts.decisions {
		if decision.IssueID == issue.ID && decision.Reason == dispatchSkipDuplicatePullRequest {
			t.Fatalf("scheduler decision = %#v, want failed merged PR reconciliation instead of duplicate skip", decision)
		}
	}
}

func TestTickReconcilesStaleTodoHydratesWorkpadBlockerBeforePromotion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-stale-todo-workpad-blocker", []string{"bug"}, &connector.PullRequest{
		Number:                 180,
		URL:                    "https://github.test/digitaldrywood/creswoodcorners-phone/pull/180",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "pass",
		CodexReviewSubmittedAt: &oldReview,
	})
	issue.State = "Todo"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#175"
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		issueComments: map[string][]connector.IssueComment{
			issue.ID: {{
				Body: "## Codex Workpad\n\n### Blockers\n- Gate A/B/C owner listening approval is still required before approved audio assets are copied and committed.",
			}},
		},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if got, want := tracker.fetchComments, []string{issue.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchIssueComments issue IDs = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one stale Todo reconciliation comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Reconciled this issue from Todo to Human Review because it already has a linked PR.",
		"reason: workpad_blocker",
		"https://github.test/digitaldrywood/creswoodcorners-phone/pull/180",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	for _, fragment := range []string{
		"reason=workpad_blocker",
		"target_state=\"Human Review\"",
		"workpad_blocker=\"Gate A/B/C owner listening approval is still required before approved audio assets are copied and committed.\"",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestTickDoesNotReconcileActiveTodoPullRequests(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 13, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	running := autoPromoteTickIssue("issue-running-todo-pr", []string{"bug"}, &connector.PullRequest{
		Number:                 40,
		URL:                    "https://github.test/digitaldrywood/detent/pull/40",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	running.State = "Todo"
	claimed := autoPromoteTickIssue("issue-claimed-todo-pr", []string{"bug"}, &connector.PullRequest{
		Number:                 41,
		URL:                    "https://github.test/digitaldrywood/detent/pull/41",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	claimed.State = "Todo"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{running, claimed}}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	state := newState(cfg)
	state.Running[running.ID] = Running{Issue: cloneIssue(running), StartedAt: now.Add(-time.Minute)}
	state.Claimed[running.ID] = Claimed{Issue: cloneIssue(running), ClaimedAt: now.Add(-time.Minute)}
	state.Claimed[claimed.ID] = Claimed{Issue: cloneIssue(claimed), ClaimedAt: now.Add(-time.Minute)}

	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none for active Todo PRs", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none for active Todo PRs", tracker.comments)
	}
	if _, ok := state.Running[running.ID]; !ok {
		t.Fatalf("Running[%q] missing after stale Todo PR reconciliation", running.ID)
	}
	if _, ok := state.Claimed[running.ID]; !ok {
		t.Fatalf("Claimed[%q] missing after stale Todo PR reconciliation", running.ID)
	}
	if _, ok := state.Claimed[claimed.ID]; !ok {
		t.Fatalf("Claimed[%q] missing after stale Todo PR reconciliation", claimed.ID)
	}
}

func TestTickAutoPromoteRunsValidatorStage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate: gate.Config{
				Kind: gate.KindCommand,
				Validator: gate.ValidatorConfig{
					Enabled:  true,
					MinScore: 0.8,
					BlockOn:  []string{"p1"},
				},
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-validator", []string{"enhancement"}, &connector.PullRequest{
		Number:                 522,
		URL:                    "https://github.test/digitaldrywood/detent/pull/522",
		BranchName:             "detent/digitaldrywood_detent_522",
		HeadSHA:                "head-validator",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	validator := &autoPromoteTickValidator{
		result: gate.ValidatorResult{
			Submitted: true,
			Verdict:   gate.ValidatorVerdictPass,
			Score:     0.91,
			Summary:   "Acceptance criteria pass.",
		},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		validator: validator,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	waitForValidatorRequests(t, validator, 1)
	waitForValidatorResult(t, orch, issue)
	if got := tracker.updates; len(got) != 0 {
		t.Fatalf("updates after scheduling validator = %#v, want none", got)
	}
	requests := validator.Requests()
	if requests[0].Issue.ID != "issue-validator" {
		t.Fatalf("validator issue = %#v, want issue-validator", requests[0].Issue)
	}

	mergingSlot := dispatchTestIssue("issue-validator-merging-slot", "Merging")
	state.Running[mergingSlot.ID] = Running{Issue: mergingSlot}
	orch.tick(context.Background(), &state, now.Add(time.Second))

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: "issue-validator", state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.prComments) != 1 {
		t.Fatalf("pull request comments = %#v, want validator result comment", tracker.prComments)
	}
	for _, fragment := range []string{"Validator verdict: pass", "score: 0.91", "Acceptance criteria pass."} {
		if !strings.Contains(tracker.prComments[0].body, fragment) {
			t.Fatalf("pull request comment %q missing %q", tracker.prComments[0].body, fragment)
		}
	}
}

func TestTickAutoPromoteUsesPersistedValidatorVerdictAfterRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	memo := openValidatorMemoStore(t)
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := autoPromoteValidatorTestConfig()
	issue := autoPromoteTickIssue("issue-validator-restart", []string{"enhancement"}, &connector.PullRequest{
		Number:                 858,
		URL:                    "https://github.test/digitaldrywood/detent/pull/858",
		BranchName:             "detent/digitaldrywood_detent_858",
		HeadSHA:                "head-validator-restart",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	validator := &autoPromoteTickValidator{
		result: gate.ValidatorResult{
			Submitted: true,
			Verdict:   gate.ValidatorVerdictPass,
			Score:     0.94,
			Summary:   "Stored validator result.",
			Findings: []gate.Finding{{
				Severity: "p2",
				Body:     "non-blocking note",
				Path:     "internal/orchestrator/autopromote_tick.go",
				Line:     12,
			}},
		},
	}
	orch := &Orchestrator{
		cfg:           cfg,
		connector:     &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
		validator:     validator,
		validatorMemo: memo,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time {
			return now
		},
	}
	state := newState(cfg)
	orch.tick(ctx, &state, now)
	waitForValidatorRequests(t, validator, 1)
	waitForPersistedValidatorVerdict(t, memo, store.ValidatorVerdictKey{
		ProjectID: "detent",
		IssueID:   issue.ID,
		HeadSHA:   "head-validator-restart",
	})

	restartedValidator := &autoPromoteTickValidator{err: errors.New("validator should not dispatch")}
	restartedTracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	restarted := &Orchestrator{
		cfg:           cfg,
		connector:     restartedTracker,
		validator:     restartedValidator,
		validatorMemo: memo,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time {
			return now.Add(time.Minute)
		},
	}
	restartedState := newState(cfg)
	mergingSlot := dispatchTestIssue("issue-validator-restart-merging-slot", "Merging")
	restartedState.Running[mergingSlot.ID] = Running{Issue: mergingSlot}
	restarted.tick(ctx, &restartedState, now.Add(time.Minute))

	if got := restartedValidator.Requests(); len(got) != 0 {
		t.Fatalf("validator requests after restart = %#v, want none", got)
	}
	if got, want := restartedTracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates after restart = %#v, want %#v", got, want)
	}
	if len(restartedTracker.prComments) != 1 {
		t.Fatalf("pull request comments after restart = %#v, want one validator result comment", restartedTracker.prComments)
	}
	for _, fragment := range []string{"Validator verdict: pass", "score: 0.94", "Stored validator result.", "non-blocking note"} {
		if !strings.Contains(restartedTracker.prComments[0].body, fragment) {
			t.Fatalf("pull request comment %q missing %q", restartedTracker.prComments[0].body, fragment)
		}
	}
}

func TestTickAutoPromoteValidatorVerdictHeadSHAInvalidatesMemo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	memo := openValidatorMemoStore(t)
	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := autoPromoteValidatorTestConfig()
	issue := autoPromoteTickIssue("issue-validator-new-head", []string{"enhancement"}, &connector.PullRequest{
		Number:                 859,
		URL:                    "https://github.test/digitaldrywood/detent/pull/859",
		BranchName:             "detent/digitaldrywood_detent_859",
		HeadSHA:                "head-validator-old",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	if err := memo.RecordValidatorVerdict(ctx, store.ValidatorVerdict{
		ProjectID:  "detent",
		IssueID:    issue.ID,
		HeadSHA:    "head-validator-old",
		Identifier: issue.Identifier,
		Submitted:  true,
		Verdict:    gate.ValidatorVerdictPass,
		Score:      0.99,
		RecordedAt: now.Add(-time.Minute),
		UpdatedAt:  now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("RecordValidatorVerdict() error = %v", err)
	}

	fresh := cloneIssue(issue)
	fresh.PullRequest.HeadSHA = "head-validator-new"
	validator := &autoPromoteTickValidator{
		result: gate.ValidatorResult{
			Submitted: true,
			Verdict:   gate.ValidatorVerdictPass,
			Score:     0.91,
		},
	}
	orch := &Orchestrator{
		cfg:           cfg,
		connector:     &autoPromoteTickConnector{stateIssues: []connector.Issue{fresh}},
		validator:     validator,
		validatorMemo: memo,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time {
			return now
		},
	}
	state := newState(cfg)
	orch.tick(ctx, &state, now)

	waitForValidatorRequests(t, validator, 1)
	requests := validator.Requests()
	if requests[0].Issue.PullRequest == nil || requests[0].Issue.PullRequest.HeadSHA != "head-validator-new" {
		t.Fatalf("validator request head SHA = %#v, want new head", requests[0].Issue.PullRequest)
	}
	waitForValidatorResult(t, orch, fresh)
}

func TestTickAutoPromoteValidatorFailureBackoff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clock := newAutoPromoteTickClock(time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC))
	oldReview := clock.Now().Add(-20 * time.Minute)
	cfg := autoPromoteValidatorTestConfig()
	cfg.FailureRetryBaseDelay = 30 * time.Second
	cfg.MaxRetryBackoff = 2 * time.Minute
	issue := autoPromoteTickIssue("issue-validator-backoff", []string{"enhancement"}, &connector.PullRequest{
		Number:                 860,
		URL:                    "https://github.test/digitaldrywood/detent/pull/860",
		BranchName:             "detent/digitaldrywood_detent_860",
		HeadSHA:                "head-validator-backoff",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	validator := &autoPromoteTickValidator{err: errors.New("validator unavailable")}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		validator: validator,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:       clock.Now,
	}
	state := newState(cfg)

	orch.tick(ctx, &state, clock.Now())
	waitForValidatorRequests(t, validator, 1)
	failure := waitForValidatorFailure(t, orch, issue, 1)
	if got, want := failure.NextRetryAt, clock.Now().Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("NextRetryAt = %s, want %s", got, want)
	}

	immediate := clock.Now().Add(time.Second)
	clock.Set(immediate)
	orch.tick(ctx, &state, immediate)
	if got := len(validator.Requests()); got != 1 {
		t.Fatalf("validator requests during backoff = %d, want 1", got)
	}

	resumeAt := failure.NextRetryAt
	clock.Set(resumeAt)
	orch.tick(ctx, &state, resumeAt)
	waitForValidatorRequests(t, validator, 2)
}

func TestTickAutoPromoteRecordsValidatorReworkHandoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 11, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate: gate.Config{
				Kind: gate.KindCommand,
				Validator: gate.ValidatorConfig{
					Enabled:  true,
					MinScore: 0.8,
					BlockOn:  []string{"p1"},
				},
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-validator-rework", []string{"enhancement"}, &connector.PullRequest{
		Number:                 856,
		URL:                    "https://github.test/digitaldrywood/detent/pull/856",
		BranchName:             "detent/digitaldrywood_detent_856",
		HeadSHA:                "head-validator-rework",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	validator := &autoPromoteTickValidator{
		result: gate.ValidatorResult{
			Submitted: true,
			Verdict:   gate.ValidatorVerdictRework,
			Score:     0.42,
			Summary:   "Missing deterministic rework context.",
			Findings: []gate.Finding{{
				Severity: "p1",
				Body:     "Prior validator finding is absent from rework prompt.",
				Path:     "internal/runner/prompt.go",
				Line:     44,
			}},
		},
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		validator: validator,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)
	waitForValidatorRequests(t, validator, 1)
	waitForValidatorResult(t, orch, issue)
	orch.tick(context.Background(), &state, now.Add(time.Second))

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: "issue-validator-rework", state: "Rework"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	handoff, ok := state.PriorAttempts[issue.ID]
	if !ok {
		t.Fatalf("PriorAttempts[%q] missing", issue.ID)
	}
	if handoff.Source != "auto_promote" || handoff.Reason != string(AutoPromoteReasonValidatorBlockedSeverity) {
		t.Fatalf("handoff = %#v, want auto_promote validator_blocked_severity", handoff)
	}
	if handoff.Validator.Verdict != gate.ValidatorVerdictRework || handoff.Validator.Score != 0.42 {
		t.Fatalf("handoff validator = %#v", handoff.Validator)
	}
	if len(handoff.Validator.Findings) != 1 || handoff.Validator.Findings[0].Path != "internal/runner/prompt.go" || handoff.Validator.Findings[0].Line != 44 {
		t.Fatalf("handoff findings = %#v", handoff.Validator.Findings)
	}
}

func TestRunDrainsInFlightValidatorStageOnShutdown(t *testing.T) {
	t.Parallel()

	oldReview := time.Now().Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-validator-shutdown", []string{"bug"}, &connector.PullRequest{
		Number:                 826,
		URL:                    "https://github.test/digitaldrywood/detent/pull/826",
		BranchName:             "detent/digitaldrywood_detent_826",
		HeadSHA:                "head-validator-shutdown",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	runningIssue := dispatchTestIssue("issue-running-shutdown", "Todo")
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue, runningIssue}}
	runner := newBlockingAutoPromoteValidatorRunner()
	t.Cleanup(runner.Release)
	globalGate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))

	orch, err := New(Config{
		PollInterval:        time.Hour,
		MaxConcurrentAgents: 1,
		Project:             scheduler.ProjectCandidate{ID: "alpha", Weight: 1},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate: gate.Config{
				Kind: gate.KindCommand,
				Validator: gate.ValidatorConfig{
					Enabled: true,
				},
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	}, Dependencies{
		Connector:          tracker,
		Runner:             runner,
		GlobalDispatchGate: globalGate,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- orch.Run(runCtx)
	}()

	select {
	case request := <-runner.started:
		if request.Issue.ID != issue.ID {
			t.Fatalf("validator issue ID = %q, want %q", request.Issue.ID, issue.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("validator did not start")
	}
	select {
	case request := <-runner.runStarted:
		if request.Issue.ID != runningIssue.ID {
			t.Fatalf("run issue ID = %q, want %q", request.Issue.ID, runningIssue.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("worker run did not start")
	}

	cancel()

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("validator did not observe shutdown cancellation")
	}
	waitForGlobalDispatchSlot(t, globalGate, "bravo")

	select {
	case err := <-runDone:
		t.Fatalf("Run() returned before validator exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	runner.Release()

	select {
	case <-runner.done:
	case <-time.After(time.Second):
		t.Fatal("validator did not exit")
	}

	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after validator exited")
	}
}

func TestTickRequeuesObservedStaleMergingIssueForDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 15, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-stale-merging", []string{"bug"}, &connector.PullRequest{
		Number:                 54,
		URL:                    "https://github.test/digitaldrywood/creswoodcorners-phone/pull/54",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#49"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{issue},
		candidateIssuesSet: true,
	}
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}
	if request.Issue.State != "Merging" {
		t.Fatalf("RunRequest.Issue.State = %q, want Merging", request.Issue.State)
	}
	if _, ok := state.Running[issue.ID]; !ok {
		t.Fatalf("Running[%q] missing after stale Merging dispatch", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing after stale Merging dispatch", issue.ID)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	for _, fragment := range []string{"merge_worker_pickup", "source=stale_merging", "merge_worker_attempt", "pull_request_number=54"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if running := state.Running[issue.ID]; running.cancel != nil {
		running.cancel()
	}
}

func TestTickDefersStaleMergingCandidateWhenObservedHydrationUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 16, 4, 0, 0, time.UTC)
	retryAt := now.Add(3 * time.Minute)
	candidate := autoPromoteTickIssue("issue-stale-merging-rate-limited", []string{"bug"}, &connector.PullRequest{
		Number:         80,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/80",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        "b8e85ef7554b4f9cf385adba88ed151e2f69a4f0",
	})
	candidate.State = "Merging"
	candidate.Identifier = "digitaldrywood/creswoodcorners-phone#79"
	observed := cloneIssue(candidate)
	observed.PullRequest.HydrationUnavailableReason = connector.PullRequestHydrationReasonSecondaryThrottled
	observed.PullRequest.HydrationNextRetryAt = &retryAt
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{observed},
		candidateIssues:    []connector.Issue{candidate},
		candidateIssuesSet: true,
	}
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if _, ok := state.Running[candidate.ID]; ok {
		t.Fatalf("Running[%q] present, want no merge worker dispatch", candidate.ID)
	}
	if _, ok := state.Claimed[candidate.ID]; ok {
		t.Fatalf("Claimed[%q] present, want no merge worker claim", candidate.ID)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected merge worker dispatch = %#v", request)
	default:
	}
	for _, fragment := range []string{
		"stale_merging_pr_reconciliation_deferred",
		"reason=pull_request_hydration_unavailable",
		"pull_request_hydration_reason=secondary_throttled",
		"pull_request_hydration_next_retry_at=2026-06-25T16:07:00Z",
		"pull_request_number=80",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	for _, fragment := range []string{"merge_worker_pickup", "merge_worker_attempt"} {
		if strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q contain %q, want no merge worker pickup or attempt", logs.String(), fragment)
		}
	}
}

func TestTickFailsAndRetriesStaleMergingWithoutStartupTelemetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 16, 0, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-stale-merging-no-startup", []string{"bug"}, &connector.PullRequest{
		Number:         71,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/71",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        "aacd52414e368678a912a7cc638f78d8ccae7131",
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#63"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{issue},
		candidateIssuesSet: true,
	}
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}

	orch.tick(context.Background(), &state, now.Add(2*time.Minute+time.Second))

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after startup timeout", issue.ID)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing after startup timeout", issue.ID)
	}
	if retry.Attempt != 1 {
		t.Fatalf("Retry[%q].Attempt = %d, want 1", issue.ID, retry.Attempt)
	}
	if !strings.Contains(retry.Error, "did not report process or session startup") {
		t.Fatalf("Retry[%q].Error = %q, want startup telemetry detail", issue.ID, retry.Error)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing after startup timeout", issue.ID)
	}
	for _, fragment := range []string{"merge_worker_failure", "reason=runner_startup_timeout", "did not report process or session startup"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected immediate redispatch after startup timeout = %#v", request)
	default:
	}
}

func TestTickDispatchesFreshAutoPromotedMergingIssue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 17, 27, 1, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-auto-promoted-merging", []string{"bug"}, &connector.PullRequest{
		Number:                 70,
		URL:                    "https://github.test/digitaldrywood/creswoodcorners-phone/pull/70",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#62"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}
	if request.Issue.State != "Merging" {
		t.Fatalf("RunRequest.Issue.State = %q, want Merging", request.Issue.State)
	}
	for _, fragment := range []string{
		"merge_worker_pickup",
		"source=auto_promote",
		"merge_worker_attempt",
		"pull_request_number=70",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if running := state.Running[issue.ID]; running.cancel != nil {
		running.cancel()
	}
}

func TestTickReconcilesStaleMergingPullRequestStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 18, 0, 0, 0, time.UTC)
	merged := autoPromoteTickIssue("issue-merged-pr", []string{"bug"}, &connector.PullRequest{
		Number:         71,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/71",
		State:          "MERGED",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	merged.State = "Merging"
	merged.Identifier = "digitaldrywood/creswoodcorners-phone#63"
	conflicting := autoPromoteTickIssue("issue-conflicting-merging", []string{"bug"}, &connector.PullRequest{
		Number:         72,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/72",
		State:          "OPEN",
		MergeableState: "DIRTY",
		CIStatus:       "success",
	})
	conflicting.State = "Merging"
	conflicting.Identifier = "digitaldrywood/creswoodcorners-phone#64"
	pending := autoPromoteTickIssue("issue-pending-merging", []string{"bug"}, &connector.PullRequest{
		Number:         74,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/74",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "pending",
	})
	pending.State = "Merging"
	pending.Identifier = "digitaldrywood/creswoodcorners-phone#66"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{merged, conflicting, pending},
		candidateIssuesSet: true,
	}
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{
		{issueID: "issue-merged-pr", state: "Done"},
	}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != conflicting.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want dirty queue head %q", request.Issue.ID, conflicting.ID)
	}
	if _, ok := state.Running[conflicting.ID]; !ok {
		t.Fatalf("Running[%q] missing after dirty Merging queue head dispatch", conflicting.ID)
	}
	if _, ok := state.Running[pending.ID]; ok {
		t.Fatalf("Running[%q] present, want same-repo sibling left queued", pending.ID)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one reconciliation comment", tracker.comments)
	}
	wantComments := map[string][]string{
		"issue-merged-pr": {
			"Reconciled this issue from Merging to Done.",
			"reason: pull_request_merged",
			"https://github.test/digitaldrywood/creswoodcorners-phone/pull/71",
		},
	}
	for _, comment := range tracker.comments {
		for _, fragment := range wantComments[comment.issueID] {
			if !strings.Contains(comment.body, fragment) {
				t.Fatalf("comment for %s = %q, missing %q", comment.issueID, comment.body, fragment)
			}
		}
	}
	for _, fragment := range []string{
		"stale_merging_pr_reconciled",
		"reason=pull_request_merged",
		"merge_worker_attempt",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if strings.Contains(logs.String(), "reason=merge_conflicts") {
		t.Fatalf("logs %q contain merge_conflicts, want dirty Merging PR handled by merge worker", logs.String())
	}
	if running := state.Running[conflicting.ID]; running.cancel != nil {
		running.cancel()
	}
}

func TestTickAdvancesStaleMergingLaneAfterFrontPRReconcilesDone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 18, 30, 0, 0, time.UTC)
	front := autoPromoteTickIssue("issue-front-merged", []string{"bug"}, &connector.PullRequest{
		Number:         71,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/71",
		State:          "MERGED",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	front.State = "Merging"
	front.Identifier = "digitaldrywood/creswoodcorners-phone#63"
	next := autoPromoteTickIssue("issue-next-ready", []string{"bug"}, &connector.PullRequest{
		Number:         72,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/72",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	next.State = "Merging"
	next.Identifier = "digitaldrywood/creswoodcorners-phone#64"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{front, next},
		candidateIssuesSet: true,
	}
	runner := newWorkerHostRunner()
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: front.ID, state: "Done"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != next.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, next.ID)
	}
	if _, ok := state.Running[next.ID]; !ok {
		t.Fatalf("Running[%q] missing after front PR reconciliation", next.ID)
	}
	if running := state.Running[next.ID]; running.cancel != nil {
		running.cancel()
	}
}

func TestTickDispatchesDirtySameRepoMergingQueueHeadForRefresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 22, 0, 0, 0, time.UTC)
	headCreatedAt := now.Add(-2 * time.Hour)
	siblingCreatedAt := now.Add(-time.Hour)
	head := autoPromoteTickIssue("issue-head-dirty", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "DIRTY",
		CIStatus:       "success",
	})
	head.State = "Merging"
	head.Identifier = "digitaldrywood/creswoodcorners-phone#66"
	head.CreatedAt = &headCreatedAt
	sibling := autoPromoteTickIssue("issue-sibling-dirty", []string{"bug"}, &connector.PullRequest{
		Number:         76,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/76",
		State:          "OPEN",
		MergeableState: "DIRTY",
		CIStatus:       "success",
	})
	sibling.State = "Merging"
	sibling.Identifier = "digitaldrywood/creswoodcorners-phone#68"
	sibling.CreatedAt = &siblingCreatedAt
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{sibling, head},
		candidateIssuesSet: true,
	}
	runner := newWorkerHostRunner()
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no Rework transition before merge-worker refresh", tracker.updates)
	}
	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != head.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want queue head %q", request.Issue.ID, head.ID)
	}
	if _, ok := state.Running[head.ID]; !ok {
		t.Fatalf("Running[%q] missing after dirty Merging queue head dispatch", head.ID)
	}
	if _, ok := state.Running[sibling.ID]; ok {
		t.Fatalf("Running[%q] present, want same-repo sibling left queued", sibling.ID)
	}
	if running := state.Running[head.ID]; running.cancel != nil {
		running.cancel()
	}
}

func TestTickReworksRedStaleMergingQueueHead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 22, 15, 0, 0, time.UTC)
	issue := autoPromoteTickIssue("issue-head-red", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "fail",
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#66"
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{issue},
		candidateIssuesSet: true,
	}
	runner := newWorkerHostRunner()
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected merge worker dispatch = %#v", request)
	default:
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one stale Merging reconciliation comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Reconciled this issue from Merging to Rework.",
		"reason: ci_not_green",
		"ci_status: fail",
		"https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment = %q, missing %q", tracker.comments[0].body, fragment)
		}
	}
	for _, fragment := range []string{"stale_merging_pr_reconciled", "reason=ci_not_green", "target_state=Rework"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if strings.Contains(logs.String(), "merge_worker_attempt") {
		t.Fatalf("logs %q contain merge_worker_attempt, want no merge worker dispatch for red CI", logs.String())
	}
}

func TestMergeWorkerLogsRunResultSuccessAndFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 19, 0, 0, 0, time.UTC)
	enteredAt := now.Add(-8 * time.Minute)
	slotAcquiredAt := now.Add(-6 * time.Minute)
	startedAt := now.Add(-5 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-merge-log", []string{"bug"}, &connector.PullRequest{
		Number:         73,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/73",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        "head-merge-log",
		BaseSHA:        "base-merge-log",
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#65"

	var failureLogs strings.Builder
	failureState := newState(cfg)
	failureState.MergeTimings[issue.ID] = MergeTiming{
		EnteredMergingAt:          enteredAt,
		MergeWorkerSlotAcquiredAt: slotAcquiredAt,
		MergeStartedAt:            startedAt,
	}
	failureState.Running[issue.ID] = Running{Issue: cloneIssue(issue), StartedAt: startedAt}
	failureOrch := &Orchestrator{
		cfg:    cfg,
		logger: slog.New(slog.NewTextHandler(&failureLogs, nil)),
	}
	failureOrch.handleRunResult(context.Background(), &failureState, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Err:         errors.New("merge command failed"),
	})
	for _, fragment := range []string{
		"merge_failed",
		"reason=runner_failed",
		"merge command failed",
		"queue_wait_seconds=120",
		"active_merge_duration_seconds=300",
		"total_merging_seconds=480",
		"head_sha=head-merge-log",
		"base_sha=base-merge-log",
	} {
		if !strings.Contains(failureLogs.String(), fragment) {
			t.Fatalf("failure logs %q missing fragment %q", failureLogs.String(), fragment)
		}
	}
	if timing := failureState.MergeTimings[issue.ID]; timing.MergeFailedAt.IsZero() || timing.MergeFailureReason != "runner_failed" {
		t.Fatalf("failure MergeTimings[%q] = %#v, want failed terminal state", issue.ID, timing)
	}

	var successLogs strings.Builder
	successIssue := cloneIssue(issue)
	successIssue.Closed = true
	successIssue.ClosedReason = "completed"
	successState := newState(cfg)
	successState.MergeTimings[successIssue.ID] = MergeTiming{
		EnteredMergingAt:          enteredAt,
		MergeWorkerSlotAcquiredAt: slotAcquiredAt,
		MergeStartedAt:            startedAt,
	}
	successState.Running[successIssue.ID] = Running{Issue: cloneIssue(successIssue), StartedAt: startedAt}
	successOrch := &Orchestrator{
		cfg:       cfg,
		connector: &autoPromoteTickConnector{stateIssues: []connector.Issue{successIssue}},
		logger:    slog.New(slog.NewTextHandler(&successLogs, nil)),
	}
	successOrch.completeTerminalRunning(context.Background(), &successState, successIssue.ID, successState.Running[successIssue.ID], now, TokenTotals{})
	for _, fragment := range []string{
		"merge_completed",
		"final_state=Done",
		"pull_request_number=73",
		"queue_wait_seconds=120",
		"active_merge_duration_seconds=300",
		"total_merging_seconds=480",
		"head_sha=head-merge-log",
		"base_sha=base-merge-log",
	} {
		if !strings.Contains(successLogs.String(), fragment) {
			t.Fatalf("success logs %q missing fragment %q", successLogs.String(), fragment)
		}
	}
	completed := successState.Completed[successIssue.ID]
	if completed.MergeTiming.MergedAt.IsZero() || completed.MergeTiming.ActiveMergeDurationSeconds != 300 {
		t.Fatalf("Completed[%q].MergeTiming = %#v, want successful terminal durations", successIssue.ID, completed.MergeTiming)
	}
}

func TestStaleMergingQueueDispatchCandidatesFiltersUnsafePullRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pullRequest *connector.PullRequest
		want        bool
	}{
		{
			name: "ready",
			pullRequest: &connector.PullRequest{
				State:          "OPEN",
				MergeableState: "clean",
				CIStatus:       "success",
			},
			want: true,
		},
		{
			name:        "missing pull request",
			pullRequest: nil,
		},
		{
			name: "merged pull request",
			pullRequest: &connector.PullRequest{
				State:    "MERGED",
				CIStatus: "success",
			},
		},
		{
			name: "draft pull request",
			pullRequest: &connector.PullRequest{
				State:          "OPEN",
				Draft:          true,
				MergeableState: "clean",
				CIStatus:       "success",
			},
		},
		{
			name: "conflicting pull request",
			pullRequest: &connector.PullRequest{
				State:          "OPEN",
				MergeableState: "dirty",
				CIStatus:       "success",
			},
			want: true,
		},
		{
			name: "non green ci",
			pullRequest: &connector.PullRequest{
				State:          "OPEN",
				MergeableState: "clean",
				CIStatus:       "pending",
			},
			want: true,
		},
		{
			name: "failed ci",
			pullRequest: &connector.PullRequest{
				State:          "OPEN",
				MergeableState: "clean",
				CIStatus:       "failure",
			},
		},
		{
			name: "hydration unavailable",
			pullRequest: &connector.PullRequest{
				State:                      "OPEN",
				MergeableState:             "clean",
				CIStatus:                   "success",
				HydrationUnavailableReason: "rate_limited",
			},
		},
		{
			name: "hydration degraded",
			pullRequest: &connector.PullRequest{
				State:                   "OPEN",
				MergeableState:          "clean",
				CIStatus:                "success",
				HydrationDegradedReason: connector.PullRequestHydrationReasonStaleCachedPullData,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1,
				MaxConcurrentAgentsByState: map[string]int{
					"Merging": 1,
				},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			orch := &Orchestrator{cfg: cfg}
			issue := autoPromoteTickIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), []string{"bug"}, tt.pullRequest)
			issue.State = "Merging"
			got := orch.staleMergingQueueDispatchCandidates(&state, []connector.Issue{issue})
			if tt.want {
				if len(got) != 1 || got[0].ID != issue.ID {
					t.Fatalf("staleMergingQueueDispatchCandidates() = %#v, want %s", got, issue.ID)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("staleMergingQueueDispatchCandidates() = %#v, want none", got)
			}
		})
	}
}

func TestMergeWorkerDispatchCandidatesPreservesScheduledRetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 24, 20, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-retrying-merge", []string{"bug"}, &connector.PullRequest{
		Number:         74,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/74",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	issue.State = "Merging"
	state := newState(cfg)
	state.Claimed[issue.ID] = Claimed{
		Issue:     cloneIssue(issue),
		ClaimedAt: now.Add(-time.Minute),
	}
	state.Retry[issue.ID] = Retry{
		Issue:   cloneIssue(issue),
		Attempt: 2,
		DueAt:   now.Add(time.Hour),
		Error:   "merge worker failed",
	}
	orch := &Orchestrator{cfg: cfg}

	got := orch.mergeWorkerDispatchCandidates(&state, []connector.Issue{issue})
	if len(got) != 0 {
		t.Fatalf("mergeWorkerDispatchCandidates() = %#v, want none while retry is scheduled", got)
	}
	if claimed, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] missing after stale Merging dispatch candidate scan", issue.ID)
	} else if claimed.Issue.ID != issue.ID {
		t.Fatalf("Claimed[%q].Issue.ID = %q, want %q", issue.ID, claimed.Issue.ID, issue.ID)
	}
	if retry, ok := state.Retry[issue.ID]; !ok {
		t.Fatalf("Retry[%q] missing after stale Merging dispatch candidate scan", issue.ID)
	} else if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", issue.ID, retry.Attempt)
	}
}

func TestMergeWorkerDispatchCandidatesSelectsOneQueueHeadPerRepository(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 21, 30, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 3,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 3,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	phoneHeadCreatedAt := now.Add(-3 * time.Hour)
	phoneSiblingCreatedAt := now.Add(-2 * time.Hour)
	outletCreatedAt := now.Add(-time.Hour)
	phoneHead := autoPromoteTickIssue("issue-phone-head", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	phoneHead.State = "Merging"
	phoneHead.Identifier = "digitaldrywood/creswoodcorners-phone#66"
	phoneHead.CreatedAt = &phoneHeadCreatedAt
	phoneSibling := autoPromoteTickIssue("issue-phone-sibling", []string{"bug"}, &connector.PullRequest{
		Number:         76,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/76",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	phoneSibling.State = "Merging"
	phoneSibling.Identifier = "digitaldrywood/creswoodcorners-phone#68"
	phoneSibling.CreatedAt = &phoneSiblingCreatedAt
	outlet := autoPromoteTickIssue("issue-outlet-head", []string{"bug"}, &connector.PullRequest{
		Number:         89,
		URL:            "https://github.test/digitaldrywood/creswoodcornersoutlet/pull/89",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	outlet.State = "Merging"
	outlet.Identifier = "digitaldrywood/creswoodcornersoutlet#89"
	outlet.CreatedAt = &outletCreatedAt
	state := newState(cfg)
	orch := &Orchestrator{cfg: cfg}

	got := orch.mergeWorkerDispatchCandidates(&state, []connector.Issue{phoneSibling, outlet, phoneHead})
	gotIDs := make([]string, 0, len(got))
	for _, issue := range got {
		gotIDs = append(gotIDs, issue.ID)
	}
	wantIDs := []string{"issue-phone-head", "issue-outlet-head"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("mergeWorkerDispatchCandidates() ids = %#v, want %#v", gotIDs, wantIDs)
	}
}

func TestMergeWorkerDispatchCandidatesConsumesNotReadyQueueHeadRepository(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 21, 45, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 3,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 3,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	phoneHeadCreatedAt := now.Add(-3 * time.Hour)
	phoneSiblingCreatedAt := now.Add(-2 * time.Hour)
	outletCreatedAt := now.Add(-time.Hour)
	phoneHead := autoPromoteTickIssue("issue-phone-head-hydration-blocked", []string{"bug"}, &connector.PullRequest{
		Number:                     75,
		URL:                        "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:                      "OPEN",
		MergeableState:             "clean",
		CIStatus:                   "success",
		HydrationUnavailableReason: connector.PullRequestHydrationReasonSecondaryThrottled,
	})
	phoneHead.State = "Merging"
	phoneHead.Identifier = "digitaldrywood/creswoodcorners-phone#66"
	phoneHead.CreatedAt = &phoneHeadCreatedAt
	phoneSibling := autoPromoteTickIssue("issue-phone-sibling-ready", []string{"bug"}, &connector.PullRequest{
		Number:         76,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/76",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	phoneSibling.State = "Merging"
	phoneSibling.Identifier = "digitaldrywood/creswoodcorners-phone#68"
	phoneSibling.CreatedAt = &phoneSiblingCreatedAt
	outlet := autoPromoteTickIssue("issue-outlet-head-ready", []string{"bug"}, &connector.PullRequest{
		Number:         89,
		URL:            "https://github.test/digitaldrywood/creswoodcornersoutlet/pull/89",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	outlet.State = "Merging"
	outlet.Identifier = "digitaldrywood/creswoodcornersoutlet#89"
	outlet.CreatedAt = &outletCreatedAt
	state := newState(cfg)
	orch := &Orchestrator{cfg: cfg}

	got := orch.mergeWorkerDispatchCandidates(&state, []connector.Issue{phoneSibling, outlet, phoneHead})
	gotIDs := make([]string, 0, len(got))
	for _, issue := range got {
		gotIDs = append(gotIDs, issue.ID)
	}
	wantIDs := []string{"issue-outlet-head-ready"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("mergeWorkerDispatchCandidates() ids = %#v, want %#v", gotIDs, wantIDs)
	}
}

func TestMergeWorkerDispatchCandidatesWaitsWhenMergingLaneFull(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 21, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	running := autoPromoteTickIssue("issue-running-merge", []string{"bug"}, &connector.PullRequest{
		Number:         72,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/72",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	running.State = "Merging"
	running.Identifier = "digitaldrywood/creswoodcorners-phone#72"
	waiting := autoPromoteTickIssue("issue-waiting-merge", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcornersoutlet/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	waiting.State = "Merging"
	waiting.Identifier = "digitaldrywood/creswoodcornersoutlet#75"
	state := newState(cfg)
	state.Running[running.ID] = Running{
		Issue:     cloneIssue(running),
		StartedAt: now.Add(-time.Minute),
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:    cfg,
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}

	got := orch.mergeWorkerDispatchCandidates(&state, []connector.Issue{waiting})
	if len(got) != 0 {
		t.Fatalf("mergeWorkerDispatchCandidates() = %#v, want none while Merging lane is full", got)
	}
	logText := logs.String()
	for _, fragment := range []string{
		"merge_worker_slot_wait",
		"reason=project_state_capacity_full",
		"project_state_capacity=1",
		"project_state_used=1",
		"project_state_available=0",
		"pull_request_number=75",
	} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("logs %q missing fragment %q", logText, fragment)
		}
	}
	if strings.Contains(logText, "merge_worker_pickup") {
		t.Fatalf("logs %q contain merge_worker_pickup, want wait telemetry without pickup", logText)
	}
}

func TestFetchTickIssuesSkipsMergingStatusHydrationWhenMergingLaneFull(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 21, 5, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	running := autoPromoteTickIssue("issue-running-merge", []string{"bug"}, &connector.PullRequest{
		Number:         72,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/72",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	running.State = "Merging"
	stale := autoPromoteTickIssue("issue-stale-merge", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	stale.State = "Merging"
	tracker := &autoPromoteTickConnector{
		stateIssues:           []connector.Issue{running, stale},
		candidateIssues:       []connector.Issue{},
		candidateIssuesSet:    true,
		fetchByStatesRequests: nil,
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[running.ID] = Running{
		Issue:     cloneIssue(running),
		StartedAt: now.Add(-time.Minute),
	}

	fetched, ok := orch.fetchTickIssues(context.Background(), &state, now, githubBudgetReserveDecision{})
	if !ok {
		t.Fatal("fetchTickIssues() ok = false, want true")
	}
	if !fetched.statusOK {
		t.Fatal("fetchTickIssues().statusOK = false, want true")
	}
	if len(tracker.candidateByStates) != 1 {
		t.Fatalf("FetchCandidateIssuesByStates requests = %#v, want one candidate fetch", tracker.candidateByStates)
	}
	for _, stateName := range tracker.candidateByStates[0] {
		if normalizeState(stateName) == normalizeState(autoPromoteMergingState) {
			t.Fatalf("FetchCandidateIssuesByStates states = %#v, want Merging omitted while lane is full", tracker.candidateByStates[0])
		}
	}
	if len(tracker.fetchByStatesRequests) != 1 {
		t.Fatalf("FetchIssuesByStates requests = %#v, want one observed status fetch", tracker.fetchByStatesRequests)
	}
	for _, stateName := range tracker.fetchByStatesRequests[0] {
		if normalizeState(stateName) == normalizeState(autoPromoteMergingState) {
			t.Fatalf("FetchIssuesByStates states = %#v, want Merging omitted while lane is full", tracker.fetchByStatesRequests[0])
		}
	}
}

func TestTickPreservesDueMergingRetryWhenLaneFullAndMergingFetchOmitted(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 21, 7, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 2,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		FailureRetryBaseDelay: time.Minute,
		MaxRetryBackoff:       time.Hour,
		ActiveStates:          []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:        []string{"Done", "Cancelled"},
	})
	running := autoPromoteTickIssue("issue-running-merge", []string{"bug"}, &connector.PullRequest{
		Number:         72,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/72",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	running.State = "Merging"
	retrying := autoPromoteTickIssue("issue-retrying-merge", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	retrying.State = "Merging"
	tracker := &autoPromoteTickConnector{
		stateIssues:        []connector.Issue{running, retrying},
		candidateIssues:    []connector.Issue{},
		candidateIssuesSet: true,
	}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[running.ID] = Running{
		Issue:     cloneIssue(running),
		StartedAt: now.Add(-time.Minute),
	}
	state.Claimed[retrying.ID] = Claimed{
		Issue:     cloneIssue(retrying),
		ClaimedAt: now.Add(-time.Minute),
	}
	state.Retry[retrying.ID] = Retry{
		Issue:   cloneIssue(retrying),
		Attempt: 2,
		DueAt:   now.Add(-time.Second),
		Error:   "run agent turn: stream turn: EOF",
	}

	orch.tick(context.Background(), &state, now)

	if len(tracker.candidateByStates) != 1 {
		t.Fatalf("FetchCandidateIssuesByStates requests = %#v, want one candidate fetch", tracker.candidateByStates)
	}
	for _, stateName := range tracker.candidateByStates[0] {
		if normalizeState(stateName) == normalizeState(autoPromoteMergingState) {
			t.Fatalf("FetchCandidateIssuesByStates states = %#v, want Merging omitted while lane is full", tracker.candidateByStates[0])
		}
	}
	if retry, ok := state.Retry[retrying.ID]; !ok {
		t.Fatalf("Retry[%q] missing while Merging lane is full", retrying.ID)
	} else if retry.Attempt != 2 || retry.Error != "run agent turn: stream turn: EOF" {
		t.Fatalf("Retry[%q] = %#v, want original retry preserved", retrying.ID, retry)
	}
	if _, ok := state.Claimed[retrying.ID]; !ok {
		t.Fatalf("Claimed[%q] missing while Merging lane is full", retrying.ID)
	}
	if _, ok := state.Running[retrying.ID]; ok {
		t.Fatalf("Running[%q] present while Merging lane is full", retrying.ID)
	}
}

func TestHandleRunResultReworksMergeWorkerAfterRepeatedRunnerFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 25, 21, 10, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		FailureRetryBaseDelay: time.Minute,
		MaxRetryBackoff:       time.Hour,
		ActiveStates:          []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:        []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-exhausted-merge", []string{"bug"}, &connector.PullRequest{
		Number:         72,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/72",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	issue.State = "Merging"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:     cloneIssue(issue),
		Attempt:   3,
		StartedAt: now.Add(-time.Minute),
	}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Err:         errors.New("run agent turn: stream turn: EOF"),
	})

	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after exhausted merge runner failures", issue.ID)
	}
	if got := tracker.updates; !reflect.DeepEqual(got, []autoPromoteTickUpdate{{issueID: issue.ID, state: autoPromoteReworkState}}) {
		t.Fatalf("updates = %#v, want Rework transition", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one exhausted retry comment", tracker.comments)
	}
	for _, fragment := range []string{"runner_failed_retry_exhausted", "stream turn: EOF", "pull request"} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	if !strings.Contains(logs.String(), "merge_worker_failure") {
		t.Fatalf("logs %q missing merge_worker_failure", logs.String())
	}
}

func TestHandleRunResultRetriesMergeWorkerWhenRunCompletesWithoutTerminalState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 15, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		FailureRetryBaseDelay: time.Minute,
		MaxRetryBackoff:       time.Hour,
		ActiveStates:          []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:        []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-incomplete-merge", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	issue.State = "Merging"
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:    cfg,
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		Attempt:    1,
		StartedAt:  now.Add(-time.Minute),
		WorkerHost: "worker-a",
	}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present after incomplete merge worker result", issue.ID)
	}
	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after incomplete merge worker result", issue.ID)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing after incomplete merge worker result", issue.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", issue.ID, retry.Attempt)
	}
	if retry.WorkerHost != "worker-a" {
		t.Fatalf("Retry[%q].WorkerHost = %q, want worker-a", issue.ID, retry.WorkerHost)
	}
	if retry.Error != "merge worker completed without reaching a terminal issue or pull request state" {
		t.Fatalf("Retry[%q].Error = %q", issue.ID, retry.Error)
	}
	if !retry.DueAt.Equal(now.Add(time.Minute * 2)) {
		t.Fatalf("Retry[%q].DueAt = %v, want %v", issue.ID, retry.DueAt, now.Add(time.Minute*2))
	}
	for _, fragment := range []string{
		"merge_worker_failure",
		"reason=terminal_state_missing",
		"completed without reaching a terminal issue or pull request state",
		"pull_request_number=75",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestHandleRunResultProgrammaticallyMergesCleanMergeWorkerWithoutTerminalState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 15, 30, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		FailureRetryBaseDelay: time.Minute,
		MaxRetryBackoff:       time.Hour,
		ActiveStates:          []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:        []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-clean-merge", []string{"bug"}, &connector.PullRequest{
		Number:         76,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/76",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        "head-clean",
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#68"
	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:       cloneIssue(issue),
		Attempt:     1,
		StartedAt:   now.Add(-time.Minute),
		WorkerHost:  "worker-a",
		TurnCount:   3,
		LastEvent:   "workpad_update",
		LastMessage: "validated current-head CI and updated the Workpad, but left the PR open",
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			Output:     "validated current-head CI and updated the Workpad",
		},
	})

	if len(tracker.merges) != 1 {
		t.Fatalf("merges = %#v, want one programmatic merge", tracker.merges)
	}
	if got := tracker.merges[0]; got.repository != "digitaldrywood/creswoodcorners-phone" || got.number != 76 || got.headSHA != "head-clean" {
		t.Fatalf("merge request = %#v, want repository digitaldrywood/creswoodcorners-phone PR 76 head-clean", got)
	}
	if got := tracker.hydrations; !reflect.DeepEqual(got, []autoPromoteTickHydration{{
		issueID:    issue.ID,
		repository: "digitaldrywood/creswoodcorners-phone",
		number:     76,
	}}) {
		t.Fatalf("hydrations = %#v, want fresh PR hydration", got)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Done"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after programmatic merge", issue.ID)
	}
	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after programmatic merge", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after programmatic merge", issue.ID)
	}
	completed, ok := state.Completed[issue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after programmatic merge", issue.ID)
	}
	if completed.FinalState != "Done" {
		t.Fatalf("Completed[%q].FinalState = %q, want Done", issue.ID, completed.FinalState)
	}
	if completed.Issue.PullRequest == nil || completed.Issue.PullRequest.State != "MERGED" {
		t.Fatalf("Completed[%q].Issue.PullRequest = %#v, want merged PR", issue.ID, completed.Issue.PullRequest)
	}
	for _, fragment := range []string{"merge_worker_programmatic_merge", "merge_worker_success"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
	if strings.Contains(logs.String(), "terminal_state_missing") {
		t.Fatalf("logs %q contain terminal_state_missing", logs.String())
	}
}

func TestHandleRunResultDoesNotProgrammaticallyMergeWhenFreshPullRequestNoLongerGreen(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 15, 45, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		FailureRetryBaseDelay: time.Minute,
		MaxRetryBackoff:       time.Hour,
		ActiveStates:          []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:        []string{"Done", "Cancelled"},
	})
	runningIssue := autoPromoteTickIssue("issue-stale-merge-pr", []string{"bug"}, &connector.PullRequest{
		Number:         76,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/76",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        "stale-head",
	})
	runningIssue.State = "Merging"
	runningIssue.Identifier = "digitaldrywood/creswoodcorners-phone#68"
	refreshedIssue := cloneIssue(runningIssue)
	refreshedIssue.PullRequest = nil
	hydratedIssue := cloneIssue(runningIssue)
	hydratedIssue.PullRequest.CIStatus = "failure"
	hydratedIssue.PullRequest.HeadSHA = "fresh-head"
	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{refreshedIssue}},
		hydratedIssues:           []connector.Issue{hydratedIssue},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[runningIssue.ID] = Running{
		Issue:      cloneIssue(runningIssue),
		Attempt:    1,
		StartedAt:  now.Add(-time.Minute),
		WorkerHost: "worker-a",
	}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: now,
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			Output:     "validated current-head CI and updated the Workpad",
		},
	})

	if len(tracker.merges) != 0 {
		t.Fatalf("merges = %#v, want none when fresh PR status is not green", tracker.merges)
	}
	if got := tracker.hydrations; !reflect.DeepEqual(got, []autoPromoteTickHydration{{
		issueID:    runningIssue.ID,
		repository: "digitaldrywood/creswoodcorners-phone",
		number:     76,
	}}) {
		t.Fatalf("hydrations = %#v, want fresh PR hydration", got)
	}
	if got := tracker.updates; len(got) != 0 {
		t.Fatalf("updates = %#v, want none when fresh PR status is not green", got)
	}
	retry, ok := state.Retry[runningIssue.ID]
	if !ok {
		t.Fatalf("Retry[%q] missing after fresh PR status prevented programmatic merge", runningIssue.ID)
	}
	if retry.Attempt != 2 {
		t.Fatalf("Retry[%q].Attempt = %d, want 2", runningIssue.ID, retry.Attempt)
	}
	if !strings.Contains(logs.String(), "reason=terminal_state_missing") {
		t.Fatalf("logs %q missing terminal_state_missing retry", logs.String())
	}
}

func TestHandleRunResultAbandonsIncompleteMergeWorkerWhileDraining(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 16, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
		Claiming: ClaimingConfig{
			Enabled:    true,
			LeaseField: "Lease",
		},
	})
	issue := autoPromoteTickIssue("issue-incomplete-merge-drain", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
		HeadSHA:        "head-clean",
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#68"
	tracker := &autoPromoteTickMergeConnector{
		autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Draining = true
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		Attempt:    1,
		StartedAt:  now.Add(-time.Minute),
		WorkerHost: "worker-a",
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after draining incomplete merge worker result", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after draining incomplete merge worker result", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after draining incomplete merge worker result", issue.ID)
	}
	if len(tracker.hydrations) != 0 {
		t.Fatalf("hydrations = %#v, want none while draining", tracker.hydrations)
	}
	if len(tracker.merges) != 0 {
		t.Fatalf("merges = %#v, want none while draining", tracker.merges)
	}
	if got := tracker.setFields; !reflect.DeepEqual(got, []autoPromoteTickSetField{{
		issueID: issue.ID,
		field:   "Lease",
		value:   "",
	}}) {
		t.Fatalf("set fields = %#v, want lease release", got)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none while draining", tracker.comments)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none while draining", tracker.updates)
	}
	if strings.Contains(logs.String(), "terminal_state_missing") {
		t.Fatalf("logs %q contain terminal_state_missing", logs.String())
	}
}

func TestHandleRunResultCompletesMergeWorkerWhenLatestIssueIsTerminal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 18, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	runningIssue := autoPromoteTickIssue("issue-terminal-merge", []string{"bug"}, &connector.PullRequest{
		Number:         75,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/75",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	runningIssue.State = "Merging"
	terminalIssue := cloneIssue(runningIssue)
	terminalIssue.Closed = true
	terminalIssue.ClosedReason = "completed"
	terminalIssue.PullRequest.State = "MERGED"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{terminalIssue}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[runningIssue.ID] = Running{
		Issue:     cloneIssue(runningIssue),
		StartedAt: now.Add(-time.Minute),
	}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: now,
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	if _, ok := state.Running[runningIssue.ID]; ok {
		t.Fatalf("Running[%q] present after terminal merge worker result", runningIssue.ID)
	}
	if _, ok := state.Retry[runningIssue.ID]; ok {
		t.Fatalf("Retry[%q] present after terminal merge worker result", runningIssue.ID)
	}
	completed, ok := state.Completed[runningIssue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after terminal merge worker result", runningIssue.ID)
	}
	if completed.FinalState != "Done" {
		t.Fatalf("Completed[%q].FinalState = %q, want Done", runningIssue.ID, completed.FinalState)
	}
	if got := tracker.updates; !reflect.DeepEqual(got, []autoPromoteTickUpdate{{issueID: runningIssue.ID, state: "Done"}}) {
		t.Fatalf("updates = %#v, want Done reconciliation", got)
	}
	if !strings.Contains(logs.String(), "merge_worker_success") {
		t.Fatalf("logs %q missing merge_worker_success", logs.String())
	}
	if strings.Contains(logs.String(), "terminal_state_missing") {
		t.Fatalf("logs %q contain terminal_state_missing", logs.String())
	}
}

func TestHandleRunResultReworksMergeWorkerAfterRepeatedIncompleteResults(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 13, 20, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:   1,
		FailureRetryBaseDelay: time.Minute,
		MaxRetryBackoff:       time.Hour,
		ActiveStates:          []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:        []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-incomplete-merge-exhausted", []string{"bug"}, &connector.PullRequest{
		Number:         76,
		URL:            "https://github.test/digitaldrywood/creswoodcorners-phone/pull/76",
		State:          "OPEN",
		MergeableState: "clean",
		CIStatus:       "success",
	})
	issue.State = "Merging"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:     cloneIssue(issue),
		Attempt:   3,
		StartedAt: now.Add(-time.Minute),
	}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateCompleted},
	})

	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after repeated incomplete merge results", issue.ID)
	}
	if got := tracker.updates; !reflect.DeepEqual(got, []autoPromoteTickUpdate{{issueID: issue.ID, state: autoPromoteReworkState}}) {
		t.Fatalf("updates = %#v, want Rework transition", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one exhausted retry comment", tracker.comments)
	}
	for _, fragment := range []string{"runner_failed_retry_exhausted", "terminal issue or pull request state", "pull request"} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	if !strings.Contains(logs.String(), "reason=terminal_state_missing") {
		t.Fatalf("logs %q missing terminal_state_missing", logs.String())
	}
}

func TestTickAutoPromoteMergingIssueDispatchesAndClearsStaleMemory(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 23, 15, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	issue := autoPromoteTickIssue("issue-promoted-merge", []string{"bug"}, &connector.PullRequest{
		Number:                 639,
		URL:                    "https://github.test/digitaldrywood/detent/pull/639",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 3,
		MaxConcurrentAgentsByState: map[string]int{
			"Merging": 1,
		},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	runner := newWorkerHostRunner()
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
	}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       cloneIssue(issue),
		CompletedAt: now.Add(-5 * time.Minute),
		FinalState:  "Human Review",
	}
	state.Claimed[issue.ID] = Claimed{
		Issue:     cloneIssue(issue),
		ClaimedAt: now.Add(-5 * time.Minute),
	}
	state.Retry[issue.ID] = Retry{
		Issue:   cloneIssue(issue),
		Attempt: 1,
		DueAt:   now.Add(time.Hour),
	}
	runningMerging := dispatchTestIssue("issue-running-merge", "Merging")
	state.Running[runningMerging.ID] = Running{Issue: runningMerging}

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	select {
	case request := <-runner.started:
		t.Fatalf("unexpected dispatch while Merging limit is full = %#v", request)
	default:
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after Merging auto-promote", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after Merging auto-promote", issue.ID)
	}

	orch.tick(context.Background(), &state, now.Add(time.Minute))

	select {
	case request := <-runner.started:
		t.Fatalf("unexpected dispatch while Merging limit is full on candidate refresh = %#v", request)
	default:
	}

	delete(state.Running, runningMerging.ID)
	orch.tick(context.Background(), &state, now.Add(2*time.Minute))

	request := receiveWorkerHostRunRequest(t, runner.started)
	if request.Issue.ID != issue.ID {
		t.Fatalf("RunRequest.Issue.ID = %q, want %q", request.Issue.ID, issue.ID)
	}
	if request.Issue.State != "Merging" {
		t.Fatalf("RunRequest.Issue.State = %q, want Merging", request.Issue.State)
	}
	if running := state.Running[issue.ID]; running.cancel != nil {
		running.cancel()
	}
	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present after Merging dispatch", issue.ID)
	}
}

func autoPromoteTickIssue(id string, labels []string, pullRequest *connector.PullRequest) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#42"
	issue.Title = "Auto promote test"
	issue.State = "Human Review"
	issue.Labels = append([]string(nil), labels...)
	issue.PullRequest = pullRequest
	return issue
}

func autoPromoteTickStatesEqual(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, state := range got {
		seen[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	for _, state := range want {
		if _, ok := seen[strings.ToLower(strings.TrimSpace(state))]; !ok {
			return false
		}
	}
	return true
}

type autoPromoteTickUpdate struct {
	issueID string
	state   string
}

type autoPromoteTickComment struct {
	issueID string
	body    string
}

type autoPromoteTickSetField struct {
	issueID string
	field   string
	value   string
}

type autoPromoteTickMerge struct {
	repository string
	number     int
	headSHA    string
}

type autoPromoteTickRerun struct {
	issueID string
	checks  []connector.PullRequestCheck
}

type autoPromoteTickHydration struct {
	issueID    string
	repository string
	number     int
}

func autoPromoteReworkEventMetadata(prNumber int, headSHA string, failedChecks ...string) string {
	issue := connector.Issue{
		PullRequest: &connector.PullRequest{
			Number:  prNumber,
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
	return workflowLaneMetadataJSON(issue, workflowLaneMetadata{})
}

type autoPromoteWorkflowMetricsRecorder struct {
	mu     sync.Mutex
	events []store.WorkflowPhaseEvent
}

func (r *autoPromoteWorkflowMetricsRecorder) RecordWorkflowPhaseEvent(_ context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}

func (r *autoPromoteWorkflowMetricsRecorder) IssueWorkflowTimeline(_ context.Context, identity store.IssueIdentity) (store.WorkflowTimeline, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	events := make([]store.WorkflowPhaseEvent, 0, len(r.events))
	for _, event := range r.events {
		if event.IssueID != "" && event.IssueID == identity.IssueID {
			events = append(events, event)
			continue
		}
		if event.Identifier != "" && event.Identifier == identity.Identifier {
			events = append(events, event)
			continue
		}
		if event.IssueURL != "" && event.IssueURL == identity.IssueURL {
			events = append(events, event)
		}
	}
	return store.WorkflowTimeline{Events: events}, nil
}

func (r *autoPromoteWorkflowMetricsRecorder) snapshot() []store.WorkflowPhaseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]store.WorkflowPhaseEvent(nil), r.events...)
}

type autoPromoteTickConnector struct {
	stateIssues           []connector.Issue
	candidateIssues       []connector.Issue
	candidateIssuesSet    bool
	candidateByStates     [][]string
	fetchByStatesRequests [][]string
	fetchComments         []string
	fetchIdentifiers      [][]string
	issueComments         map[string][]connector.IssueComment
	resolvedIssues        []connector.Issue
	updates               []autoPromoteTickUpdate
	comments              []autoPromoteTickComment
	prComments            []autoPromoteTickComment
	setFields             []autoPromoteTickSetField
	reruns                []autoPromoteTickRerun
	updateErr             error
}

type autoPromoteTickMergeConnector struct {
	*autoPromoteTickConnector
	merges         []autoPromoteTickMerge
	hydrations     []autoPromoteTickHydration
	hydratedIssues []connector.Issue
	err            error
	hydrateErr     error
}

func (c *autoPromoteTickConnector) Name() string {
	return "auto-promote-tick"
}

func (c *autoPromoteTickConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	return c.FetchCandidateIssuesByStates(ctx, []string{"Todo", "In Progress", "Rework", "Merging"})
}

func (c *autoPromoteTickConnector) FetchCandidateIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.candidateByStates = append(c.candidateByStates, append([]string(nil), states...))
	if c.candidateIssuesSet {
		return issuesInStates(c.candidateIssues, states), nil
	}
	return issuesInStates(c.stateIssues, states), nil
}

func (c *autoPromoteTickConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.fetchByStatesRequests = append(c.fetchByStatesRequests, append([]string(nil), states...))
	wanted := make(map[string]struct{}, len(states))
	for _, state := range states {
		wanted[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.State))]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) FetchIssueComments(_ context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	c.fetchComments = append(c.fetchComments, strings.TrimSpace(issue.ID))
	return cloneIssueComments(c.issueComments[strings.TrimSpace(issue.ID)]), nil
}

func (c *autoPromoteTickConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[issue.ID]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) ([]connector.Issue, error) {
	c.fetchIdentifiers = append(c.fetchIdentifiers, append([]string(nil), identifiers...))
	wanted := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		wanted[strings.ToLower(strings.TrimSpace(identifier))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.resolvedIssues))
	for _, issue := range c.resolvedIssues {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.Identifier))]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, autoPromoteTickComment{issueID: issueID, body: body})
	return nil
}

func (c *autoPromoteTickConnector) CreatePullRequestComment(_ context.Context, repository string, number int, body string) error {
	c.prComments = append(c.prComments, autoPromoteTickComment{issueID: repository, body: body})
	return nil
}

func (c *autoPromoteTickConnector) RerunPullRequestChecks(_ context.Context, issue connector.Issue, checks []connector.PullRequestCheck) error {
	c.reruns = append(c.reruns, autoPromoteTickRerun{
		issueID: issue.ID,
		checks:  append([]connector.PullRequestCheck(nil), checks...),
	})
	return nil
}

func (c *autoPromoteTickMergeConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.hydrations = append(c.hydrations, autoPromoteTickHydration{
		issueID:    issue.ID,
		repository: pullRequestRepository(issue),
		number:     pullRequestNumber(issue),
	})
	if c.hydrateErr != nil {
		return cloneIssue(issue), c.hydrateErr
	}
	issues := c.hydratedIssues
	if len(issues) == 0 && c.autoPromoteTickConnector != nil {
		issues = c.stateIssues
	}
	for _, candidate := range issues {
		if strings.TrimSpace(candidate.ID) != "" && strings.TrimSpace(candidate.ID) == strings.TrimSpace(issue.ID) {
			return cloneIssue(candidate), nil
		}
		if strings.TrimSpace(candidate.Identifier) != "" && strings.EqualFold(strings.TrimSpace(candidate.Identifier), strings.TrimSpace(issue.Identifier)) {
			return cloneIssue(candidate), nil
		}
		if pullRequestNumber(candidate) > 0 &&
			pullRequestNumber(candidate) == pullRequestNumber(issue) &&
			strings.EqualFold(pullRequestRepository(candidate), pullRequestRepository(issue)) {
			return cloneIssue(candidate), nil
		}
	}
	return cloneIssue(issue), nil
}

func (c *autoPromoteTickMergeConnector) MergePullRequest(_ context.Context, repository string, number int, headSHA string) error {
	c.merges = append(c.merges, autoPromoteTickMerge{repository: repository, number: number, headSHA: headSHA})
	if c.err != nil {
		return c.err
	}
	for index := range c.stateIssues {
		issue := &c.stateIssues[index]
		if pullRequestNumber(*issue) != number || !strings.EqualFold(pullRequestRepository(*issue), repository) || issue.PullRequest == nil {
			continue
		}
		issue.PullRequest.State = "MERGED"
		now := time.Date(2026, 6, 26, 13, 15, 31, 0, time.UTC)
		issue.PullRequest.ActivityAt = &now
		issue.UpdatedAt = &now
		return nil
	}
	return nil
}

type autoPromoteTickValidator struct {
	mu       sync.Mutex
	result   gate.ValidatorResult
	requests []ValidatorRequest
	err      error
}

func (v *autoPromoteTickValidator) Validate(_ context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests = append(v.requests, req)
	return v.result, v.err
}

type blockingAutoPromoteValidatorRunner struct {
	releaseOnce sync.Once
	started     chan ValidatorRequest
	runStarted  chan RunRequest
	canceled    chan struct{}
	release     chan struct{}
	done        chan struct{}
}

func newBlockingAutoPromoteValidatorRunner() *blockingAutoPromoteValidatorRunner {
	return &blockingAutoPromoteValidatorRunner{
		started:    make(chan ValidatorRequest, 1),
		runStarted: make(chan RunRequest, 1),
		canceled:   make(chan struct{}),
		release:    make(chan struct{}),
		done:       make(chan struct{}),
	}
}

func (r *blockingAutoPromoteValidatorRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	r.runStarted <- req
	<-ctx.Done()
	return RunResult{FinalState: runpkg.FinalStateFailed}, ctx.Err()
}

func (r *blockingAutoPromoteValidatorRunner) Validate(ctx context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	r.started <- req
	defer close(r.done)

	select {
	case <-r.release:
		return gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, Score: 1}, nil
	case <-ctx.Done():
		close(r.canceled)
		<-r.release
		return gate.ValidatorResult{}, ctx.Err()
	}
}

func (r *blockingAutoPromoteValidatorRunner) Release() {
	r.releaseOnce.Do(func() {
		close(r.release)
	})
}

func (v *autoPromoteTickValidator) Requests() []ValidatorRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]ValidatorRequest(nil), v.requests...)
}

func waitForValidatorRequests(t *testing.T, validator *autoPromoteTickValidator, count int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(validator.Requests()) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("validator requests = %d, want at least %d", len(validator.Requests()), count)
}

func waitForValidatorResult(t *testing.T, orch *Orchestrator, issue connector.Issue) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := orch.validatorStageResult(context.Background(), issue); ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("validator result was not recorded")
}

func waitForPersistedValidatorVerdict(t *testing.T, memo store.ValidatorMemoStore, key store.ValidatorVerdictKey) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := memo.ValidatorVerdict(context.Background(), key); err == nil {
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ValidatorVerdict() error = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("validator verdict was not persisted for %#v", key)
}

func waitForValidatorFailure(t *testing.T, orch *Orchestrator, issue connector.Issue, attempt int) validatorStageFailure {
	t.Helper()

	identity := validatorStageIdentityForIssue(issue)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		orch.validatorMu.Lock()
		failure, ok := orch.validatorFailures[identity.Key]
		orch.validatorMu.Unlock()
		if ok && failure.Attempt >= attempt {
			return failure
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("validator failure attempt %d was not recorded", attempt)
	return validatorStageFailure{}
}

func openValidatorMemoStore(t *testing.T) store.Store {
	t.Helper()

	backend, err := store.Open(context.Background(), store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return backend
}

func autoPromoteValidatorTestConfig() Config {
	return normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent", Weight: 1},
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate: gate.Config{
				Kind: gate.KindCommand,
				Validator: gate.ValidatorConfig{
					Enabled:  true,
					MinScore: 0.8,
					BlockOn:  []string{"p1"},
				},
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
}

type autoPromoteTickClock struct {
	mu  sync.Mutex
	now time.Time
}

func newAutoPromoteTickClock(now time.Time) *autoPromoteTickClock {
	return &autoPromoteTickClock{now: now}
}

func (c *autoPromoteTickClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *autoPromoteTickClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func waitForGlobalDispatchSlot(t *testing.T, globalGate scheduler.ProjectDispatchGate, projectID string) {
	t.Helper()

	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		slot, ok, err := globalGate.TryAcquire(t.Context(), scheduler.ProjectCandidate{ID: projectID, Weight: 1}, scheduler.SlotRequest{State: "Todo"}, time.Now())
		if err != nil {
			t.Fatalf("TryAcquire() error = %v", err)
		}
		if ok {
			if err := globalGate.Release(slot); err != nil {
				t.Fatalf("Release() error = %v", err)
			}
			return
		}

		select {
		case <-deadline:
			t.Fatal("global dispatch slot was not released before validator drain completed")
		case <-ticker.C:
		}
	}
}

func (c *autoPromoteTickConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, autoPromoteTickUpdate{issueID: issueID, state: state})
	if c.updateErr != nil {
		return c.updateErr
	}
	for index := range c.stateIssues {
		if c.stateIssues[index].ID == issueID {
			c.stateIssues[index].State = state
		}
	}
	return nil
}

func (c *autoPromoteTickConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *autoPromoteTickConnector) SetField(_ context.Context, issueID string, field string, value string) error {
	c.setFields = append(c.setFields, autoPromoteTickSetField{issueID: issueID, field: field, value: value})
	return nil
}
