package orchestrator

import (
	"strings"
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
			name:  "red ci reworks by default",
			issue: autoPromoteTestIssue("issue-red-ci", nil),
			cfg:   enabled,
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "red",
			},
			want: AutoPromoteDecision{
				Action:   AutoPromoteActionRework,
				Reason:   AutoPromoteReasonCINotGreen,
				CIStatus: "red",
			},
		},
		{
			name:  "red ci skips when configured",
			issue: autoPromoteTestIssue("issue-red-ci-skip", nil),
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				OptoutLabel:   "requires-human-review",
				Gate:          gate.Config{Kind: gate.KindCommand, CIFailureAction: gate.CIFailureActionSkip},
			},
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
			name: "workpad blocker awaits review",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-blocker", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Blockers\n- Gate A/B/C owner listening approval is still required before approved audio assets are copied and committed.",
				}}
				return issue
			}(),
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
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonWorkpadBlocker,
			},
		},
		{
			name: "workpad no blocker promotes",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-no-blocker", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Blockers\n- None currently.\n\n### Validation\n- make check passed.",
				}}
				return issue
			}(),
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
			name: "workpad none first clause promotes",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-none-first-clause", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Blockers\n- None. Dependency #1013 is closed with detent:done; branch rebased onto current origin/main before implementation.\n\n### Validation\n- make check passed.",
				}}
				return issue
			}(),
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
			name: "workpad removed blocked by prose promotes",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-removed-blocked-by", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Blockers\n- The previous dependency blocker regressions now pass locally on the rebased head,\n  so stale `Blocked by: #1462` / `Blocked by: #1463` lines were removed from the issue body.\n\n### Validation\n- make check passed.",
				}}
				return issue
			}(),
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
			name: "workpad resolved dependency prose promotes",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-resolved-dependency-prose", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Blockers\n- Dependency blocker #1462 merged via PR #1482 and issue #1462 is closed/Done; #1463 was already closed. Removed the stale `Blocked by: #1462` line from #1476.\n\n### Validation\n- make check passed.",
				}}
				return issue
			}(),
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
			name: "workpad closed blocked by line promotes",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-closed-blocked-by", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Blockers\n- Blocked by: #1462\n\n### Validation\n- make check passed.",
				}}
				issue.BlockedBy = []connector.BlockedRef{{
					Identifier: "digitaldrywood/detent#1462",
					State:      "Done",
				}}
				return issue
			}(),
			cfg: AutoPromoteConfig{
				Enabled:        true,
				QuietDuration:  10 * time.Minute,
				OptoutLabel:    "requires-human-review",
				TerminalStates: []string{"Done", "Cancelled"},
				Gate:           gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
			},
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				LastActivityAt: &oldActivity,
			},
			want: AutoPromoteDecision{
				Action:                  AutoPromoteActionPromote,
				Reason:                  AutoPromoteReasonReady,
				ResolvedWorkpadBlockers: []string{"digitaldrywood/detent#1462"},
			},
		},
		{
			name: "workpad open blocked by line awaits review",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-open-blocked-by", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Blockers\n- Blocked by: #1462\n\n### Validation\n- make check passed.",
				}}
				issue.BlockedBy = []connector.BlockedRef{{
					Identifier: "digitaldrywood/detent#1462",
					State:      "In Progress",
				}}
				return issue
			}(),
			cfg: AutoPromoteConfig{
				Enabled:        true,
				QuietDuration:  10 * time.Minute,
				OptoutLabel:    "requires-human-review",
				TerminalStates: []string{"Done", "Cancelled"},
				Gate:           gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
			},
			input: AutoPromoteSummary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				LastActivityAt: &oldActivity,
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonWorkpadBlocker,
			},
		},
		{
			name: "workpad blocker phrase awaits review",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-workpad-blocker-phrase", nil)
				issue.Comments = []connector.IssueComment{{
					Body: "## Codex Workpad\n\n### Notes\n- Owner listening approval is still required before approved audio assets are copied and committed.",
				}}
				return issue
			}(),
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
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonWorkpadBlocker,
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
		{
			name: "artifact gate promotes from summary without pull request",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-artifact-summary", nil)
				issue.PullRequest = nil
				return issue
			}(),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindArtifact},
			},
			input: AutoPromoteSummary{
				ArtifactStatus: "valid",
				MergeableState: "dirty",
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionPromote,
				Reason: AutoPromoteReasonReady,
			},
		},
		{
			name: "artifact gate promotes from configured work item field",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-artifact-field", nil)
				issue.Fields = map[string]string{"render_status": "Ready"}
				return issue
			}(),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate: gate.Config{
					Kind: gate.KindArtifact,
					Artifact: gate.ArtifactConfig{
						StatusField:    "render_status",
						PassStatuses:   []string{"ready"},
						WaitStatuses:   []string{"queued"},
						ReworkStatuses: []string{"recut"},
					},
				},
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionPromote,
				Reason: AutoPromoteReasonReady,
			},
		},
		{
			name: "artifact gate routes configured rework field over stale summary",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-artifact-configured-rework", nil)
				issue.Fields = map[string]string{"render_status": "recut"}
				return issue
			}(),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate: gate.Config{
					Kind: gate.KindArtifact,
					Artifact: gate.ArtifactConfig{
						StatusField:    "render_status",
						PassStatuses:   []string{"approved"},
						WaitStatuses:   []string{"pending_review"},
						ReworkStatuses: []string{"recut"},
					},
				},
			},
			input: AutoPromoteSummary{ArtifactStatus: "pending_review"},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionRework,
				Reason: AutoPromoteReasonArtifactStatusRework,
			},
		},
		{
			name: "artifact gate promotes configured pass field over stale summary",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-artifact-configured-pass", nil)
				issue.Fields = map[string]string{"render_status": "approved"}
				return issue
			}(),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate: gate.Config{
					Kind: gate.KindArtifact,
					Artifact: gate.ArtifactConfig{
						StatusField:    "render_status",
						PassStatuses:   []string{"approved"},
						WaitStatuses:   []string{"pending_review"},
						ReworkStatuses: []string{"recut"},
					},
				},
			},
			input: AutoPromoteSummary{ArtifactStatus: "pending_review"},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionPromote,
				Reason: AutoPromoteReasonReady,
			},
		},
		{
			name: "artifact gate routes rework from deliverable validation status",
			issue: func() connector.Issue {
				issue := autoPromoteTestIssue("issue-artifact-deliverable", nil)
				issue.Deliverable = &connector.Deliverable{ValidationStatus: "invalid"}
				return issue
			}(),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindArtifact},
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionRework,
				Reason: AutoPromoteReasonArtifactStatusRework,
			},
		},
		{
			name:  "artifact gate waits when status is missing",
			issue: autoPromoteTestIssue("issue-artifact-missing", nil),
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindArtifact},
			},
			want: AutoPromoteDecision{
				Action: AutoPromoteActionAwaitReview,
				Reason: AutoPromoteReasonArtifactStatusMissing,
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
			if len(got.ResolvedWorkpadBlockers) != len(tt.want.ResolvedWorkpadBlockers) {
				t.Fatalf("ResolvedWorkpadBlockers len = %d, want %d", len(got.ResolvedWorkpadBlockers), len(tt.want.ResolvedWorkpadBlockers))
			}
			for i := range tt.want.ResolvedWorkpadBlockers {
				if got.ResolvedWorkpadBlockers[i] != tt.want.ResolvedWorkpadBlockers[i] {
					t.Fatalf("ResolvedWorkpadBlockers[%d] = %q, want %q", i, got.ResolvedWorkpadBlockers[i], tt.want.ResolvedWorkpadBlockers[i])
				}
			}
			for i := range tt.want.Findings {
				if got.Findings[i] != tt.want.Findings[i] {
					t.Fatalf("Findings[%d] = %#v, want %#v", i, got.Findings[i], tt.want.Findings[i])
				}
			}
		})
	}
}

func TestAutoPromoteNormalizeWorkpadBlockerText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "none sentinel remains non-blocking",
			text: "- None",
		},
		{
			name: "none currently sentinel remains non-blocking",
			text: "- None currently.",
		},
		{
			name: "n/a sentinel remains non-blocking",
			text: "- N/A",
		},
		{
			name: "no blockers sentinel remains non-blocking",
			text: "- No blockers;",
		},
		{
			name: "none first clause is non-blocking",
			text: "- None. Dependency #123 is closed; rebased onto main.",
		},
		{
			name: "none currently first clause is non-blocking",
			text: "- None currently. Waiting on nothing.",
		},
		{
			name: "n/a first clause is non-blocking",
			text: "- N/A: covered by #456.",
		},
		{
			name: "none starting a real sentence is blocking",
			text: "- None of the tests pass.",
			want: "None of the tests pass.",
		},
		{
			name: "blocked first clause remains blocking",
			text: "- Blocked: needs schema decision.",
			want: "Blocked: needs schema decision.",
		},
		{
			name: "must be resolved remains blocking",
			text: "- Blocked by: #123 must be resolved before merge.",
			want: "Blocked by: #123 must be resolved before merge.",
		},
		{
			name: "removed stale blocked by prose is non-blocking",
			text: "- The previous dependency blocker regressions now pass locally on the rebased head,\n  so stale `Blocked by: #1462` / `Blocked by: #1463` lines were removed from the issue body.",
		},
		{
			name: "resolved dependency prose is non-blocking",
			text: "- Dependency blocker #1462 merged via PR #1482 and issue #1462 is closed/Done; #1463 was already closed. Removed the stale `Blocked by: #1462` line from #1476.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := autoPromoteNormalizeWorkpadBlockerText(tt.text)
			if got != tt.want {
				t.Fatalf("autoPromoteNormalizeWorkpadBlockerText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEvaluateAutoPromoteStructuredWorkpad(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	oldActivity := now.Add(-20 * time.Minute)
	ready := AutoPromoteSummary{
		PullRequestURL: "https://github.test/pull/42",
		CIStatus:       "green",
		LastActivityAt: &oldActivity,
	}
	cfg := AutoPromoteConfig{
		Enabled:        true,
		QuietDuration:  10 * time.Minute,
		OptoutLabel:    "requires-human-review",
		TerminalStates: []string{"Done", "Cancelled"},
		Gate:           gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
	}

	tests := []struct {
		name                  string
		body                  string
		cfg                   AutoPromoteConfig
		blockedBy             []connector.BlockedRef
		wantAction            AutoPromoteAction
		wantReason            AutoPromoteReason
		wantBlocker           string
		wantResolved          []string
		wantSource            string
		wantInvalid           string
		wantProseDisabled     bool
		wantVerificationParts []string
	}{
		{
			name: "open structured blocker holds",
			body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: \"#1462\"\n    reason: \"needs migration\"\nhuman_action: null\n```",
			blockedBy: []connector.BlockedRef{{
				Identifier: "digitaldrywood/detent#1462",
				State:      "In Progress",
			}},
			wantAction:  AutoPromoteActionAwaitReview,
			wantReason:  AutoPromoteReasonWorkpadBlocker,
			wantBlocker: "digitaldrywood/detent#1462: needs migration",
			wantSource:  "structured",
			wantVerificationParts: []string{
				"digitaldrywood/detent#1462",
				"In Progress",
				"active",
			},
		},
		{
			name: "resolved structured blocker releases",
			body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: \"#1462\"\n    reason: \"needs migration\"\nhuman_action: null\n```",
			blockedBy: []connector.BlockedRef{{
				Identifier: "digitaldrywood/detent#1462",
				State:      "Done",
			}},
			wantAction:   AutoPromoteActionPromote,
			wantReason:   AutoPromoteReasonReady,
			wantResolved: []string{"digitaldrywood/detent#1462"},
			wantSource:   "structured",
			wantVerificationParts: []string{
				"digitaldrywood/detent#1462",
				"Done",
				"resolved",
			},
		},
		{
			name:       "structured block suppresses prose",
			body:       "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```\n\n### Blockers\n- Blocked by: #999",
			wantAction: AutoPromoteActionPromote,
			wantReason: AutoPromoteReasonReady,
			wantSource: "structured",
		},
		{
			name:        "malformed structured block awaits review",
			body:        "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: null\n```",
			wantAction:  AutoPromoteActionAwaitReview,
			wantReason:  AutoPromoteReasonWorkpadStatusInvalid,
			wantSource:  "structured",
			wantInvalid: "status blocked requires",
		},
		{
			name:        "prose fallback preserved",
			body:        "## Codex Workpad\n\n### Blockers\n- Owner approval is still required before merge.",
			wantAction:  AutoPromoteActionAwaitReview,
			wantReason:  AutoPromoteReasonWorkpadBlocker,
			wantBlocker: "Owner approval is still required before merge.",
			wantSource:  "prose_section",
		},
		{
			name: "structured only disables prose fallback",
			body: "## Codex Workpad\n\n### Blockers\n- Owner approval is still required before merge.",
			cfg: func() AutoPromoteConfig {
				cfg := cfg
				cfg.WorkpadStructuredOnly = true
				return cfg
			}(),
			wantAction:        AutoPromoteActionPromote,
			wantReason:        AutoPromoteReasonReady,
			wantSource:        "prose_section",
			wantProseDisabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := autoPromoteTestIssue("issue-structured-workpad", nil)
			issue.Identifier = "digitaldrywood/detent#1069"
			issue.Comments = []connector.IssueComment{{
				Body: tt.body,
				URL:  "https://github.test/comment/structured",
			}}
			issue.BlockedBy = append([]connector.BlockedRef(nil), tt.blockedBy...)
			testCfg := cfg
			if tt.cfg.Enabled {
				testCfg = tt.cfg
			}

			got := EvaluateAutoPromote(issue, ready, testCfg, now)
			if got.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q", got.Action, tt.wantAction)
			}
			if got.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if got.WorkpadBlocker != tt.wantBlocker {
				t.Fatalf("WorkpadBlocker = %q, want %q", got.WorkpadBlocker, tt.wantBlocker)
			}
			if got.WorkpadSignalSource != tt.wantSource {
				t.Fatalf("WorkpadSignalSource = %q, want %q", got.WorkpadSignalSource, tt.wantSource)
			}
			if got.WorkpadCommentURL != "https://github.test/comment/structured" {
				t.Fatalf("WorkpadCommentURL = %q, want structured comment URL", got.WorkpadCommentURL)
			}
			if tt.wantInvalid != "" && !strings.Contains(got.WorkpadStatusInvalid, tt.wantInvalid) {
				t.Fatalf("WorkpadStatusInvalid = %q, want containing %q", got.WorkpadStatusInvalid, tt.wantInvalid)
			}
			if got.WorkpadProseFallbackDisabled != tt.wantProseDisabled {
				t.Fatalf("WorkpadProseFallbackDisabled = %v, want %v", got.WorkpadProseFallbackDisabled, tt.wantProseDisabled)
			}
			if len(got.ResolvedWorkpadBlockers) != len(tt.wantResolved) {
				t.Fatalf("ResolvedWorkpadBlockers = %#v, want %#v", got.ResolvedWorkpadBlockers, tt.wantResolved)
			}
			for index, want := range tt.wantResolved {
				if got.ResolvedWorkpadBlockers[index] != want {
					t.Fatalf("ResolvedWorkpadBlockers[%d] = %q, want %q", index, got.ResolvedWorkpadBlockers[index], want)
				}
			}
			verificationText := strings.Join(autoPromoteWorkpadBlockerVerificationStrings(got.WorkpadBlockerVerifications), " ")
			for _, want := range tt.wantVerificationParts {
				if !strings.Contains(verificationText, want) {
					t.Fatalf("verification text %q missing %q", verificationText, want)
				}
			}
		})
	}
}

func TestEvaluateAutoPromoteStructuredWorkpadStatusPolicy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 13, 45, 0, 0, time.UTC)
	oldActivity := now.Add(-20 * time.Minute)
	ready := AutoPromoteSummary{
		PullRequestURL: "https://github.test/pull/42",
		CIStatus:       "green",
		LastActivityAt: &oldActivity,
	}
	cfg := AutoPromoteConfig{
		Enabled:        true,
		QuietDuration:  10 * time.Minute,
		TerminalStates: []string{"Done", "Cancelled"},
		Gate:           gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
	}

	tests := []struct {
		name                string
		body                string
		completedFinalState string
		structuredOnly      bool
		wantAction          AutoPromoteAction
		wantReason          AutoPromoteReason
		wantBlocker         string
		wantSource          string
		wantInvalid         string
		wantProseDisabled   bool
	}{
		{
			name:       "valid complete promotes",
			body:       "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```",
			wantAction: AutoPromoteActionPromote,
			wantReason: AutoPromoteReasonReady,
			wantSource: "structured",
		},
		{
			name:                "invalid status after success routes to rework",
			body:                "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: human-review\nblockers: []\nhuman_action: null\n```",
			completedFinalState: FinalStateCompleted,
			wantAction:          AutoPromoteActionRework,
			wantReason:          AutoPromoteReasonWorkpadStatusInvalid,
			wantSource:          "structured",
			wantInvalid:         `status "human-review"`,
		},
		{
			name:                "invalid status after failure awaits review",
			body:                "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: human_review\nblockers: []\nhuman_action: null\n```",
			completedFinalState: "failed",
			wantAction:          AutoPromoteActionAwaitReview,
			wantReason:          AutoPromoteReasonWorkpadStatusInvalid,
			wantSource:          "structured",
			wantInvalid:         `status "human_review"`,
		},
		{
			name:        "missing structured block uses prose fallback",
			body:        "## Codex Workpad\n\n### Blockers\n- Owner approval is still required before merge.",
			wantAction:  AutoPromoteActionAwaitReview,
			wantReason:  AutoPromoteReasonWorkpadBlocker,
			wantBlocker: "Owner approval is still required before merge.",
			wantSource:  "prose_section",
		},
		{
			name:              "missing structured block with structured only ignores prose",
			body:              "## Codex Workpad\n\n### Blockers\n- Owner approval is still required before merge.",
			structuredOnly:    true,
			wantAction:        AutoPromoteActionPromote,
			wantReason:        AutoPromoteReasonReady,
			wantSource:        "prose_section",
			wantProseDisabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := autoPromoteTestIssue("issue-workpad-status-policy", nil)
			issue.Identifier = "digitaldrywood/detent#1107"
			issue.Comments = []connector.IssueComment{{
				Body: tt.body,
				URL:  "https://github.test/comment/workpad-status-policy",
			}}
			testCfg := cfg
			testCfg.WorkpadStructuredOnly = tt.structuredOnly
			summary := ready
			summary.CompletedFinalState = tt.completedFinalState

			got := EvaluateAutoPromote(issue, summary, testCfg, now)
			if got.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q", got.Action, tt.wantAction)
			}
			if got.Reason != tt.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if got.WorkpadBlocker != tt.wantBlocker {
				t.Fatalf("WorkpadBlocker = %q, want %q", got.WorkpadBlocker, tt.wantBlocker)
			}
			if got.WorkpadSignalSource != tt.wantSource {
				t.Fatalf("WorkpadSignalSource = %q, want %q", got.WorkpadSignalSource, tt.wantSource)
			}
			if tt.wantInvalid != "" && !strings.Contains(got.WorkpadStatusInvalid, tt.wantInvalid) {
				t.Fatalf("WorkpadStatusInvalid = %q, want containing %q", got.WorkpadStatusInvalid, tt.wantInvalid)
			}
			if got.WorkpadProseFallbackDisabled != tt.wantProseDisabled {
				t.Fatalf("WorkpadProseFallbackDisabled = %v, want %v", got.WorkpadProseFallbackDisabled, tt.wantProseDisabled)
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
