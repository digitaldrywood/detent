package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestEvaluateAutoPromote(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	oldActivity := now.Add(-20 * time.Minute)
	recentActivity := now.Add(-30 * time.Second)
	finding := AutoPromoteFinding{
		Body: "![P1 Badge](https://example.test/p1.svg) The migration drops rows.",
		URL:  "https://github.test/comment/p1",
		Path: "db/migrations/example.sql",
		Line: 7,
	}

	enabled := AutoPromoteConfig{
		Enabled:       true,
		QuietDuration: 10 * time.Minute,
		OptoutLabel:   "requires-human-review",
	}
	ready := AutoPromoteSummary{
		PullRequestURL: "https://github.test/pull/42",
		CIStatus:       "green",
		ReviewState:    "COMMENTED",
		LastActivityAt: &oldActivity,
	}

	tests := []struct {
		name  string
		issue connector.Issue
		cfg   AutoPromoteConfig
		input AutoPromoteSummary
		want  AutoPromoteDecision
	}{
		{
			name:  "disabled",
			issue: autoPromoteTestIssue("issue-disabled", nil),
			cfg:   AutoPromoteConfig{Enabled: false},
			input: ready,
			want: AutoPromoteDecision{
				Action: AutoPromoteActionSkip,
				Reason: AutoPromoteReasonDisabled,
			},
		},
		{
			name:  "opt-out label awaits human review",
			issue: autoPromoteTestIssue("issue-optout", []string{"Requires-Human-Review", "docs"}),
			cfg: AutoPromoteConfig{
				Enabled:            true,
				QuietDuration:      10 * time.Minute,
				OptoutLabel:        "requires-human-review",
				AllowedIssueLabels: []string{"docs"},
			},
			input: ready,
			want: AutoPromoteDecision{
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonOptoutLabel,
			},
		},
		{
			name:  "allowed label miss awaits human review",
			issue: autoPromoteTestIssue("issue-label-miss", []string{"enhancement"}),
			cfg: AutoPromoteConfig{
				Enabled:            true,
				QuietDuration:      10 * time.Minute,
				OptoutLabel:        "requires-human-review",
				AllowedIssueLabels: []string{"docs"},
			},
			input: ready,
			want: AutoPromoteDecision{
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonLabelNotAllowed,
			},
		},
		{
			name:  "missing pull request skips",
			issue: autoPromoteTestIssue("issue-missing-pr", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionSkip,
				Reason: AutoPromoteReasonMissingPullRequest,
			},
		},
		{
			name:  "red ci skips",
			issue: autoPromoteTestIssue("issue-red-ci", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "red",
			},
			want: AutoPromoteDecision{
				Action:   AutoPromoteActionSkip,
				Reason:   AutoPromoteReasonCINotGreen,
				CIStatus: "red",
			},
		},
		{
			name:  "missing automated review awaits review",
			issue: autoPromoteTestIssue("issue-missing-review", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonCodexReviewMissing,
			},
		},
		{
			name:  "automated review disabled promotes after green ci and quiet period",
			issue: autoPromoteTestIssue("issue-no-review-required", nil),
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				OptoutLabel:   "requires-human-review",
				Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
			},
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				LastActivityAt: &oldActivity,
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionPromote,
				Reason: AutoPromoteReasonReady,
			},
		},
		{
			name:  "P1 findings move to rework",
			issue: autoPromoteTestIssue("issue-p1", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "APPROVED",
				P1Findings:     []AutoPromoteFinding{finding},
				LastActivityAt: &oldActivity,
			},
			want: AutoPromoteDecision{
				Action:   AutoPromoteActionRework,
				Reason:   AutoPromoteReasonP1Findings,
				Findings: []AutoPromoteFinding{finding},
			},
		},
		{
			name:  "GitHub changes requested review counts as submitted",
			issue: autoPromoteTestIssue("issue-github-changes-requested", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "CHANGES_REQUESTED",
				P1Findings:     []AutoPromoteFinding{finding},
				LastActivityAt: &oldActivity,
			},
			want: AutoPromoteDecision{
				Action:   AutoPromoteActionRework,
				Reason:   AutoPromoteReasonP1Findings,
				Findings: []AutoPromoteFinding{finding},
			},
		},
		{
			name:  "recent Codex activity awaits quiet window",
			issue: autoPromoteTestIssue("issue-recent", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "REQUESTED_CHANGES",
				LastActivityAt: &recentActivity,
			},
			want: AutoPromoteDecision{
				Action:         AutoPromoteActionAwaitReview,
				Reason:         AutoPromoteReasonCodexReviewNotQuiet,
				QuietRemaining: 570 * time.Second,
			},
		},
		{
			name:  "missing last activity awaits full quiet window",
			issue: autoPromoteTestIssue("issue-missing-activity", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "COMMENTED",
			},
			want: AutoPromoteDecision{
				Action:         AutoPromoteActionAwaitReview,
				Reason:         AutoPromoteReasonCodexReviewNotQuiet,
				QuietRemaining: 10 * time.Minute,
			},
		},
		{
			name:  "quiet validated pull request promotes",
			issue: autoPromoteTestIssue("issue-promote", nil),
			cfg:   enabled,
			input: ready,
			want: AutoPromoteDecision{
				Action: AutoPromoteActionPromote,
				Reason: AutoPromoteReasonReady,
			},
		},
		{
			name:  "zero quiet duration promotes without activity timestamp",
			issue: autoPromoteTestIssue("issue-zero-quiet", nil),
			cfg: AutoPromoteConfig{
				Enabled:     true,
				OptoutLabel: "requires-human-review",
			},
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "COMMENTED",
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionPromote,
				Reason: AutoPromoteReasonReady,
			},
		},
		{
			name:  "human review gate waits for approval label",
			issue: autoPromoteTestIssue("issue-human-wait", []string{"strategy"}),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindHumanReview, ApprovalLabel: "approved-by-human"},
			},
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "red",
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonHumanApprovalMissing,
			},
		},
		{
			name:  "human review gate promotes with approval label without ci",
			issue: autoPromoteTestIssue("issue-human-ready", []string{"Approved-By-Human"}),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindHumanReview, ApprovalLabel: "approved-by-human"},
			},
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "red",
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionPromote,
				Reason: AutoPromoteReasonReady,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := EvaluateAutoPromote(tt.issue, tt.input, tt.cfg, now)
			if got.Action != tt.want.Action {
				t.Fatalf("Action = %q, want %q", got.Action, tt.want.Action)
			}
			if got.Reason != tt.want.Reason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tt.want.Reason)
			}
			if got.CIStatus != tt.want.CIStatus {
				t.Fatalf("CIStatus = %q, want %q", got.CIStatus, tt.want.CIStatus)
			}
			if got.QuietRemaining != tt.want.QuietRemaining {
				t.Fatalf("QuietRemaining = %s, want %s", got.QuietRemaining, tt.want.QuietRemaining)
			}
			if len(got.Findings) != len(tt.want.Findings) {
				t.Fatalf("Findings len = %d, want %d", len(got.Findings), len(tt.want.Findings))
			}
			for i := range tt.want.Findings {
				if got.Findings[i] != tt.want.Findings[i] {
					t.Fatalf("Findings[%d] = %#v, want %#v", i, got.Findings[i], tt.want.Findings[i])
				}
			}
		})
	}
}

func TestAutoPromoteWaitsForFreshPullRequestActivityWithoutAutomatedReview(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	oldActivity := now.Add(-30 * time.Minute)
	recentPullRequestActivity := now.Add(-30 * time.Second)
	issue := autoPromoteTestIssue("issue-fresh-pr-activity", []string{"bug"})
	issue.UpdatedAt = &oldActivity
	issue.PullRequest = &connector.PullRequest{
		Number:     42,
		URL:        "https://github.test/digitaldrywood/detent/pull/42",
		State:      "OPEN",
		CIStatus:   "pass",
		ActivityAt: &recentPullRequestActivity,
	}

	summary := AutoPromoteSummaryFromIssue(issue)
	if summary.LastActivityAt == nil || !summary.LastActivityAt.Equal(recentPullRequestActivity) {
		t.Fatalf("LastActivityAt = %v, want pull request activity %v", summary.LastActivityAt, recentPullRequestActivity)
	}

	got := EvaluateAutoPromote(issue, summary, AutoPromoteConfig{
		Enabled:       true,
		QuietDuration: 10 * time.Minute,
		OptoutLabel:   "requires-human-review",
		Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
	}, now)
	if got.Action != AutoPromoteActionAwaitReview {
		t.Fatalf("Action = %q, want %q", got.Action, AutoPromoteActionAwaitReview)
	}
	if got.Reason != AutoPromoteReasonCodexReviewNotQuiet {
		t.Fatalf("Reason = %q, want %q", got.Reason, AutoPromoteReasonCodexReviewNotQuiet)
	}
	if got.QuietRemaining != 570*time.Second {
		t.Fatalf("QuietRemaining = %s, want 570s", got.QuietRemaining)
	}
}

func TestEvaluateAutoPromoteRoutesConflictingPullRequestToRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		mergeableState string
	}{
		{
			name:           "rest dirty state",
			mergeableState: "dirty",
		},
		{
			name:           "graphql conflicting state",
			mergeableState: "CONFLICTING",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := autoPromoteTestIssue("issue-conflicting", []string{"bug"})
			issue.PullRequest = &connector.PullRequest{
				Number:         614,
				URL:            "https://github.test/digitaldrywood/detent/pull/614",
				State:          "OPEN",
				MergeableState: tt.mergeableState,
			}

			got := EvaluateAutoPromote(issue, AutoPromoteSummaryFromIssue(issue), AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 0,
				OptoutLabel:   "requires-human-review",
				Gate: gate.Config{
					Kind:            gate.KindCommand,
					CIFailureAction: gate.CIFailureActionRework,
				},
			}, now)
			if got.Action != AutoPromoteActionRework {
				t.Fatalf("Action = %q, want %q", got.Action, AutoPromoteActionRework)
			}
			if string(got.Reason) != "merge_conflicts" {
				t.Fatalf("Reason = %q, want merge_conflicts", got.Reason)
			}
		})
	}
}

func autoPromoteTestIssue(id string, labels []string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#15"
	issue.Title = "Auto promote"
	issue.State = "Human Review"
	issue.Labels = append([]string(nil), labels...)
	return issue
}
