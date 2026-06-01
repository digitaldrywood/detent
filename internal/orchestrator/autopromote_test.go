package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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
			name:  "missing Codex review awaits human review",
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

func autoPromoteTestIssue(id string, labels []string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#15"
	issue.Title = "Auto promote"
	issue.State = "Human Review"
	issue.Labels = append([]string(nil), labels...)
	return issue
}
