package gate

import (
	"testing"
	"time"
)

func TestEffectiveSelectsGateDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want Config
	}{
		{
			name: "omitted gate defaults to make check command",
			cfg:  Config{},
			want: Config{Kind: KindCommand, Run: DefaultCommand, ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(true)},
		},
		{
			name: "command keeps custom run",
			cfg:  Config{Kind: " command ", Run: " make verify "},
			want: Config{Kind: KindCommand, Run: "make verify", ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(true)},
		},
		{
			name: "command can disable automated review requirement",
			cfg:  Config{Kind: KindCommand, Run: "make verify", RequireAutomatedReview: new(false)},
			want: Config{Kind: KindCommand, Run: "make verify", ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(false)},
		},
		{
			name: "human review normalizes alias and approval label",
			cfg:  Config{Kind: "human-review", Run: "make check", ApprovalLabel: " Human-Approved "},
			want: Config{Kind: KindHumanReview, Run: "", ApprovalLabel: "human-approved"},
		},
		{
			name: "human review gets default approval label",
			cfg:  Config{Kind: KindHumanReview},
			want: Config{Kind: KindHumanReview, Run: "", ApprovalLabel: DefaultApprovalLabel},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Effective(tt.cfg)
			if !configsEqual(got, tt.want) {
				t.Fatalf("Effective() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	oldActivity := now.Add(-20 * time.Minute)
	recentActivity := now.Add(-30 * time.Second)
	finding := Finding{Body: "P1 finding", URL: "https://example.test/comment", Path: "main.go", Line: 7}
	readyCommand := Summary{
		PullRequestURL: "https://github.test/pull/42",
		CIStatus:       "green",
		ReviewState:    "COMMENTED",
		LastActivityAt: &oldActivity,
	}

	tests := []struct {
		name   string
		cfg    Config
		labels []string
		input  Summary
		opts   EvaluationOptions
		want   Decision
	}{
		{
			name:  "command gate skips without pull request",
			input: Summary{},
			want:  Decision{Action: ActionSkip, Reason: ReasonMissingPullRequest},
		},
		{
			name: "command gate skips red ci",
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "red",
			},
			want: Decision{Action: ActionSkip, Reason: ReasonCINotGreen, CIStatus: "red"},
		},
		{
			name: "command gate waits for automated review",
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
			},
			want: Decision{Action: ActionWait, Reason: ReasonAutomatedReviewMissing},
		},
		{
			name: "command gate passes without automated review when disabled",
			cfg:  Config{Kind: KindCommand, RequireAutomatedReview: new(false)},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				LastActivityAt: &oldActivity,
			},
			opts: EvaluationOptions{QuietDuration: 10 * time.Minute},
			want: Decision{Action: ActionPass, Reason: ReasonReady},
		},
		{
			name: "command gate requests rework for p1 findings",
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "success",
				ReviewState:    "APPROVED",
				P1Findings:     []Finding{finding},
				LastActivityAt: &oldActivity,
			},
			want: Decision{Action: ActionRework, Reason: ReasonP1Findings, Findings: []Finding{finding}},
		},
		{
			name: "command gate requests rework for p1 review state",
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "P1",
				LastActivityAt: &oldActivity,
			},
			want: Decision{Action: ActionRework, Reason: ReasonP1Findings},
		},
		{
			name:  "command gate waits for quiet window",
			input: Summary{PullRequestURL: "https://github.test/pull/42", CIStatus: "green", ReviewState: "P2", LastActivityAt: &recentActivity},
			opts:  EvaluationOptions{QuietDuration: 10 * time.Minute},
			want:  Decision{Action: ActionWait, Reason: ReasonAutomatedReviewNotQuiet, QuietRemaining: 570 * time.Second},
		},
		{
			name:  "command gate passes when ci and automated review pass",
			input: readyCommand,
			opts:  EvaluationOptions{QuietDuration: 10 * time.Minute},
			want:  Decision{Action: ActionPass, Reason: ReasonReady},
		},
		{
			name:  "human review gate waits for approval label",
			cfg:   Config{Kind: KindHumanReview, ApprovalLabel: "approved-by-human"},
			input: Summary{PullRequestPresent: true},
			labels: []string{
				"enhancement",
			},
			want: Decision{Action: ActionWait, Reason: ReasonHumanApprovalMissing},
		},
		{
			name:   "human review gate passes with approval label",
			cfg:    Config{Kind: KindHumanReview, ApprovalLabel: "approved-by-human"},
			input:  Summary{PullRequestPresent: true},
			labels: []string{" Approved-By-Human "},
			want:   Decision{Action: ActionPass, Reason: ReasonReady},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Evaluate(tt.cfg, tt.labels, tt.input, now, tt.opts)
			if got.Action != tt.want.Action ||
				got.Reason != tt.want.Reason ||
				got.CIStatus != tt.want.CIStatus ||
				got.QuietRemaining != tt.want.QuietRemaining {
				t.Fatalf("Evaluate() = %#v, want %#v", got, tt.want)
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

func configsEqual(left Config, right Config) bool {
	return left.Kind == right.Kind &&
		left.Run == right.Run &&
		left.ApprovalLabel == right.ApprovalLabel &&
		boolPointerEqual(left.RequireAutomatedReview, right.RequireAutomatedReview)
}

func boolPointerEqual(left *bool, right *bool) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
