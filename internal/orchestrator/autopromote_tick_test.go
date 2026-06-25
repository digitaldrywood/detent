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
			if !autoPromoteTickStatesEqual(tracker.fetchByStatesRequests[0], []string{"Blocked", "Human Review", "Merging"}) {
				t.Fatalf("FetchIssuesByStates() states = %#v, want Blocked/Human Review/Merging", tracker.fetchByStatesRequests[0])
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
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, runner, cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if running := state.Running[issue.ID]; running.cancel != nil {
		running.cancel()
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
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:        cfg,
		connector:  tracker,
		supervisor: newTestSupervisor(t, newWorkerHostRunner(), cfg),
		runResults: make(chan runpkg.Completion, 1),
		logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)

	orch.tick(context.Background(), &state, now)

	wantUpdates := []autoPromoteTickUpdate{
		{issueID: "issue-merged-pr", state: "Done"},
		{issueID: "issue-conflicting-merging", state: "Rework"},
	}
	if !reflect.DeepEqual(tracker.updates, wantUpdates) {
		t.Fatalf("updates = %#v, want %#v", tracker.updates, wantUpdates)
	}
	if len(tracker.comments) != 2 {
		t.Fatalf("comments = %#v, want two reconciliation comments", tracker.comments)
	}
	wantComments := map[string][]string{
		"issue-merged-pr": {
			"Reconciled this issue from Merging to Done.",
			"reason: pull_request_merged",
			"https://github.test/digitaldrywood/creswoodcorners-phone/pull/71",
		},
		"issue-conflicting-merging": {
			"Reconciled this issue from Merging to Rework.",
			"reason: merge_conflicts",
			"mergeable_state: dirty",
			"https://github.test/digitaldrywood/creswoodcorners-phone/pull/72",
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
		"reason=merge_conflicts",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
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

func TestStaleMergingDispatchCandidatesFiltersUnsafePullRequests(t *testing.T) {
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
		},
		{
			name: "non green ci",
			pullRequest: &connector.PullRequest{
				State:          "OPEN",
				MergeableState: "clean",
				CIStatus:       "pending",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := autoPromoteTickIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), []string{"bug"}, tt.pullRequest)
			issue.State = "Merging"
			got := staleMergingDispatchCandidates([]connector.Issue{issue})
			if tt.want {
				if len(got) != 1 || got[0].ID != issue.ID {
					t.Fatalf("staleMergingDispatchCandidates() = %#v, want %s", got, issue.ID)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("staleMergingDispatchCandidates() = %#v, want none", got)
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

type autoPromoteTickConnector struct {
	stateIssues           []connector.Issue
	candidateIssues       []connector.Issue
	candidateIssuesSet    bool
	fetchByStatesRequests [][]string
	updates               []autoPromoteTickUpdate
	comments              []autoPromoteTickComment
	prComments            []autoPromoteTickComment
}

func (c *autoPromoteTickConnector) Name() string {
	return "auto-promote-tick"
}

func (c *autoPromoteTickConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	if c.candidateIssuesSet {
		return cloneIssues(c.candidateIssues), nil
	}
	return issuesInStates(c.stateIssues, []string{"Todo", "In Progress", "Rework", "Merging"}), nil
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

func (c *autoPromoteTickConnector) SetField(context.Context, string, string, string) error {
	return nil
}
