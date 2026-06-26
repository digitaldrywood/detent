package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
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
			name: "routes failing ci to rework when configured",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				Gate: gate.Config{
					Kind:            gate.KindCommand,
					CIFailureAction: gate.CIFailureActionRework,
				},
			},
			issue: autoPromoteTickIssue("issue-red-ci", []string{"bug"}, &connector.PullRequest{
				Number:   48,
				URL:      "https://github.test/digitaldrywood/detent/pull/48",
				State:    "OPEN",
				CIStatus: "fail",
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-red-ci",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework: current-head CI is failing.",
				"reason: ci_not_green",
				"ci_status: red",
				"https://github.test/digitaldrywood/detent/pull/48",
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
				State:                  "MERGED",
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
	})
	issue.State = "Merging"
	issue.Identifier = "digitaldrywood/creswoodcorners-phone#65"

	var failureLogs strings.Builder
	failureState := newState(cfg)
	failureState.Running[issue.ID] = Running{Issue: cloneIssue(issue), StartedAt: now.Add(-time.Minute)}
	failureOrch := &Orchestrator{
		cfg:    cfg,
		logger: slog.New(slog.NewTextHandler(&failureLogs, nil)),
	}
	failureOrch.handleRunResult(context.Background(), &failureState, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Err:         errors.New("merge command failed"),
	})
	for _, fragment := range []string{"merge_worker_failure", "reason=runner_failed", "merge command failed"} {
		if !strings.Contains(failureLogs.String(), fragment) {
			t.Fatalf("failure logs %q missing fragment %q", failureLogs.String(), fragment)
		}
	}

	var successLogs strings.Builder
	successIssue := cloneIssue(issue)
	successIssue.Closed = true
	successIssue.ClosedReason = "completed"
	successState := newState(cfg)
	successState.Running[successIssue.ID] = Running{Issue: cloneIssue(successIssue), StartedAt: now.Add(-time.Minute)}
	successOrch := &Orchestrator{
		cfg:       cfg,
		connector: &autoPromoteTickConnector{stateIssues: []connector.Issue{successIssue}},
		logger:    slog.New(slog.NewTextHandler(&successLogs, nil)),
	}
	successOrch.completeTerminalRunning(context.Background(), &successState, successIssue.ID, successState.Running[successIssue.ID], now, CodexTotals{})
	for _, fragment := range []string{"merge_worker_success", "final_state=Done", "pull_request_number=73"} {
		if !strings.Contains(successLogs.String(), fragment) {
			t.Fatalf("success logs %q missing fragment %q", successLogs.String(), fragment)
		}
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

type autoPromoteTickConnector struct {
	stateIssues           []connector.Issue
	candidateIssues       []connector.Issue
	candidateIssuesSet    bool
	candidateByStates     [][]string
	fetchByStatesRequests [][]string
	updates               []autoPromoteTickUpdate
	comments              []autoPromoteTickComment
	prComments            []autoPromoteTickComment
	setFields             []autoPromoteTickSetField
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

func (c *autoPromoteTickConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, autoPromoteTickComment{issueID: issueID, body: body})
	return nil
}

func (c *autoPromoteTickConnector) CreatePullRequestComment(_ context.Context, repository string, number int, body string) error {
	c.prComments = append(c.prComments, autoPromoteTickComment{issueID: repository, body: body})
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
		if _, _, ok := orch.validatorStageResult(issue); ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("validator result was not recorded")
}

func (c *autoPromoteTickConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, autoPromoteTickUpdate{issueID: issueID, state: state})
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
