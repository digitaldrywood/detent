package gate

import (
	"strings"
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
			want: Config{Kind: KindCommand, Run: DefaultCommand, ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(true), CIFailureAction: CIFailureActionRework},
		},
		{
			name: "command keeps custom run",
			cfg:  Config{Kind: " command ", Run: " make verify "},
			want: Config{Kind: KindCommand, Run: "make verify", ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(true), CIFailureAction: CIFailureActionRework},
		},
		{
			name: "command can disable automated review requirement",
			cfg:  Config{Kind: KindCommand, Run: "make verify", RequireAutomatedReview: new(false)},
			want: Config{Kind: KindCommand, Run: "make verify", ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(false), CIFailureAction: CIFailureActionRework},
		},
		{
			name: "command can explicitly skip failed ci",
			cfg:  Config{Kind: KindCommand, Run: "make verify", CIFailureAction: " skip "},
			want: Config{Kind: KindCommand, Run: "make verify", ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(true), CIFailureAction: CIFailureActionSkip},
		},
		{
			name: "command can route failed ci to rework",
			cfg:  Config{Kind: KindCommand, Run: "make verify", CIFailureAction: " Rework "},
			want: Config{Kind: KindCommand, Run: "make verify", ApprovalLabel: DefaultApprovalLabel, RequireAutomatedReview: new(true), CIFailureAction: CIFailureActionRework},
		},
		{
			name: "human review normalizes alias and approval label",
			cfg:  Config{Kind: "human-review", Run: "make check", ApprovalLabel: " Human-Approved "},
			want: Config{Kind: KindHumanReview, Run: "", ApprovalLabel: "human-approved", CIFailureAction: CIFailureActionSkip},
		},
		{
			name: "human review gets default approval label",
			cfg:  Config{Kind: KindHumanReview},
			want: Config{Kind: KindHumanReview, Run: "", ApprovalLabel: DefaultApprovalLabel, CIFailureAction: CIFailureActionSkip},
		},
		{
			name: "artifact gate gets status defaults and drops PR command settings",
			cfg: Config{
				Kind:                   "artifact-status",
				Run:                    "make check",
				RequireAutomatedReview: new(true),
			},
			want: Config{
				Kind:            KindArtifact,
				Run:             "",
				ApprovalLabel:   DefaultApprovalLabel,
				CIFailureAction: CIFailureActionSkip,
				Artifact: ArtifactConfig{
					StatusField:    DefaultArtifactStatusField,
					PassStatuses:   []string{"approved", "complete", "completed", "pass", "passed", "valid"},
					WaitStatuses:   []string{"pending", "review", "reviewing", "waiting"},
					ReworkStatuses: []string{"changes_requested", "failed", "invalid", "rework"},
				},
			},
		},
		{
			name: "validator defaults stay disabled with score threshold and p1 blocker",
			cfg:  Config{Kind: KindCommand, Validator: ValidatorConfig{}},
			want: Config{
				Kind:                   KindCommand,
				Run:                    DefaultCommand,
				ApprovalLabel:          DefaultApprovalLabel,
				RequireAutomatedReview: new(true),
				CIFailureAction:        CIFailureActionRework,
				Validator: ValidatorConfig{
					MinScore: DefaultValidatorMinScore,
					BlockOn:  []string{"p1"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Effective(tt.cfg)
			want := tt.want
			want.Validator = effectiveValidatorConfig(want.Validator)
			want.Artifact = effectiveArtifactConfig(want.Artifact)
			if !configsEqual(got, want) {
				t.Fatalf("Effective() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestEffectivePlanSelectsDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  PlanConfig
		want PlanConfig
	}{
		{
			name: "omitted plan config is disabled with default review stop",
			cfg:  PlanConfig{},
			want: PlanConfig{Enabled: false, Review: PlanReviewHuman, ApprovalLabel: DefaultPlanApprovalLabel, Stop: DefaultPlanStop},
		},
		{
			name: "enabled plan normalizes review and trims stop",
			cfg:  PlanConfig{Enabled: true, Review: " Both ", Stop: " Plan Review "},
			want: PlanConfig{Enabled: true, Review: PlanReviewBoth, ApprovalLabel: DefaultPlanApprovalLabel, Stop: DefaultPlanStop},
		},
		{
			name: "enabled plan defaults review and stop",
			cfg:  PlanConfig{Enabled: true},
			want: PlanConfig{Enabled: true, Review: PlanReviewHuman, ApprovalLabel: DefaultPlanApprovalLabel, Stop: DefaultPlanStop},
		},
		{
			name: "enabled plan normalizes approval label",
			cfg:  PlanConfig{Enabled: true, ApprovalLabel: " Plan-Approved "},
			want: PlanConfig{Enabled: true, Review: PlanReviewHuman, ApprovalLabel: DefaultPlanApprovalLabel, Stop: DefaultPlanStop},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := EffectivePlan(tt.cfg); got != tt.want {
				t.Fatalf("EffectivePlan() = %#v, want %#v", got, tt.want)
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
			name: "command gate reworks red ci by default",
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "red",
			},
			want: Decision{Action: ActionRework, Reason: ReasonCINotGreen, CIStatus: "red"},
		},
		{
			name: "command gate skips red ci when configured",
			cfg:  Config{Kind: KindCommand, CIFailureAction: CIFailureActionSkip},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "red",
			},
			want: Decision{Action: ActionSkip, Reason: ReasonCINotGreen, CIStatus: "red"},
		},
		{
			name: "command gate reworks red ci when configured",
			cfg:  Config{Kind: KindCommand, CIFailureAction: CIFailureActionRework},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "fail",
			},
			want: Decision{Action: ActionRework, Reason: ReasonCINotGreen, CIStatus: "red"},
		},
		{
			name: "command gate reworks cancelled ci when configured",
			cfg:  Config{Kind: KindCommand, CIFailureAction: CIFailureActionRework},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "cancelled",
			},
			want: Decision{Action: ActionRework, Reason: ReasonCINotGreen, CIStatus: "cancelled"},
		},
		{
			name: "command gate skips pending ci when rework is configured",
			cfg:  Config{Kind: KindCommand, CIFailureAction: CIFailureActionRework},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "pending",
			},
			want: Decision{Action: ActionSkip, Reason: ReasonCINotGreen, CIStatus: "pending"},
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
			name: "validator gate waits for missing validator result",
			cfg: Config{
				Kind:      KindCommand,
				Validator: ValidatorConfig{Enabled: true, MinScore: 0.8, BlockOn: []string{"p1"}},
			},
			input: readyCommand,
			opts:  EvaluationOptions{QuietDuration: 10 * time.Minute},
			want:  Decision{Action: ActionWait, Reason: ReasonValidatorMissing},
		},
		{
			name: "validator gate waits for quiet window before requesting validator",
			cfg: Config{
				Kind:      KindCommand,
				Validator: ValidatorConfig{Enabled: true, MinScore: 0.8, BlockOn: []string{"p1"}},
			},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "COMMENTED",
				LastActivityAt: &recentActivity,
			},
			opts: EvaluationOptions{QuietDuration: 10 * time.Minute},
			want: Decision{Action: ActionWait, Reason: ReasonAutomatedReviewNotQuiet, QuietRemaining: 570 * time.Second},
		},
		{
			name: "validator gate passes above score threshold",
			cfg: Config{
				Kind:      KindCommand,
				Validator: ValidatorConfig{Enabled: true, MinScore: 0.8, BlockOn: []string{"p1"}},
			},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "COMMENTED",
				LastActivityAt: &oldActivity,
				Validator: ValidatorResult{
					Submitted: true,
					Verdict:   ValidatorVerdictPass,
					Score:     0.91,
				},
			},
			opts: EvaluationOptions{QuietDuration: 10 * time.Minute},
			want: Decision{Action: ActionPass, Reason: ReasonReady},
		},
		{
			name: "validator gate reworks below score threshold",
			cfg: Config{
				Kind:      KindCommand,
				Validator: ValidatorConfig{Enabled: true, MinScore: 0.8, BlockOn: []string{"p1"}},
			},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "COMMENTED",
				LastActivityAt: &oldActivity,
				Validator: ValidatorResult{
					Submitted: true,
					Verdict:   ValidatorVerdictPass,
					Score:     0.72,
				},
			},
			want: Decision{Action: ActionRework, Reason: ReasonValidatorScoreBelowThreshold},
		},
		{
			name: "validator gate reworks blocked severity regardless of score",
			cfg: Config{
				Kind:      KindCommand,
				Validator: ValidatorConfig{Enabled: true, MinScore: 0.8, BlockOn: []string{"p1"}},
			},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "COMMENTED",
				LastActivityAt: &oldActivity,
				Validator: ValidatorResult{
					Submitted: true,
					Verdict:   ValidatorVerdictPass,
					Score:     0.98,
					Findings:  []Finding{{Severity: "P1", Body: "Acceptance criteria missed."}},
				},
			},
			want: Decision{
				Action:   ActionRework,
				Reason:   ReasonValidatorBlockedSeverity,
				Findings: []Finding{{Severity: "P1", Body: "Acceptance criteria missed."}},
			},
		},
		{
			name: "validator gate waits on wait verdict",
			cfg: Config{
				Kind:      KindCommand,
				Validator: ValidatorConfig{Enabled: true, MinScore: 0.8, BlockOn: []string{"p1"}},
			},
			input: Summary{
				PullRequestURL: "https://github.test/pull/42",
				CIStatus:       "green",
				ReviewState:    "COMMENTED",
				LastActivityAt: &oldActivity,
				Validator: ValidatorResult{
					Submitted: true,
					Verdict:   ValidatorVerdictWait,
					Score:     0.86,
				},
			},
			want: Decision{Action: ActionWait, Reason: ReasonValidatorWait},
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
		{
			name:  "artifact gate waits for missing status",
			cfg:   Config{Kind: KindArtifact},
			input: Summary{},
			want:  Decision{Action: ActionWait, Reason: ReasonArtifactStatusMissing},
		},
		{
			name:  "artifact gate waits for pending status without pull request",
			cfg:   Config{Kind: KindArtifact},
			input: Summary{ArtifactStatus: " Pending "},
			want:  Decision{Action: ActionWait, Reason: ReasonArtifactStatusWait},
		},
		{
			name:  "artifact gate routes rework status without pull request",
			cfg:   Config{Kind: KindArtifact},
			input: Summary{ArtifactStatus: "invalid"},
			want:  Decision{Action: ActionRework, Reason: ReasonArtifactStatusRework},
		},
		{
			name: "artifact gate passes custom valid status without pull request",
			cfg: Config{
				Kind: KindArtifact,
				Artifact: ArtifactConfig{
					StatusField:    "render_status",
					PassStatuses:   []string{"ready"},
					WaitStatuses:   []string{"queued"},
					ReworkStatuses: []string{"recut"},
				},
			},
			input: Summary{ArtifactStatus: "Ready"},
			want:  Decision{Action: ActionPass, Reason: ReasonReady},
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

func TestEvaluatePlan(t *testing.T) {
	t.Parallel()

	finding := Finding{Body: "Plan misses rollback steps.", URL: "https://github.test/comment/1"}

	tests := []struct {
		name   string
		cfg    PlanConfig
		labels []string
		input  Summary
		want   Decision
	}{
		{
			name: "disabled plan gate skips",
			cfg:  PlanConfig{},
			want: Decision{Action: ActionSkip, Reason: ReasonPlanDisabled},
		},
		{
			name:   "human plan gate passes with plan approval label",
			cfg:    PlanConfig{Enabled: true, Review: PlanReviewHuman},
			labels: []string{" Plan-Approved "},
			want:   Decision{Action: ActionPass, Reason: ReasonReady},
		},
		{
			name:   "human plan gate ignores final approval label",
			cfg:    PlanConfig{Enabled: true, Review: PlanReviewHuman, ApprovalLabel: "plan-approved"},
			labels: []string{"human-approved"},
			want:   Decision{Action: ActionWait, Reason: ReasonHumanApprovalMissing},
		},
		{
			name: "human plan gate waits without approval label",
			cfg:  PlanConfig{Enabled: true, Review: PlanReviewHuman},
			want: Decision{Action: ActionWait, Reason: ReasonHumanApprovalMissing},
		},
		{
			name:  "automated plan gate passes after automated review",
			cfg:   PlanConfig{Enabled: true, Review: PlanReviewAutomated},
			input: Summary{ReviewState: "COMMENTED"},
			want:  Decision{Action: ActionPass, Reason: ReasonReady},
		},
		{
			name: "automated plan gate waits before review",
			cfg:  PlanConfig{Enabled: true, Review: PlanReviewAutomated},
			want: Decision{Action: ActionWait, Reason: ReasonAutomatedReviewMissing},
		},
		{
			name:  "automated plan gate routes p1 findings to rework",
			cfg:   PlanConfig{Enabled: true, Review: PlanReviewAutomated},
			input: Summary{ReviewState: "P1", P1Findings: []Finding{finding}},
			want:  Decision{Action: ActionRework, Reason: ReasonP1Findings, Findings: []Finding{finding}},
		},
		{
			name:   "both plan gate accepts human approval",
			cfg:    PlanConfig{Enabled: true, Review: PlanReviewBoth},
			labels: []string{"plan-approved"},
			want:   Decision{Action: ActionPass, Reason: ReasonReady},
		},
		{
			name:  "both plan gate accepts automated approval",
			cfg:   PlanConfig{Enabled: true, Review: PlanReviewBoth},
			input: Summary{ReviewState: "APPROVED"},
			want:  Decision{Action: ActionPass, Reason: ReasonReady},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := EvaluatePlan(tt.cfg, tt.labels, tt.input)
			if got.Action != tt.want.Action || got.Reason != tt.want.Reason {
				t.Fatalf("EvaluatePlan() = %#v, want %#v", got, tt.want)
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

func TestInstructionsDescribeOptimizedMergingGate(t *testing.T) {
	t.Parallel()

	got := Instructions(Config{Kind: KindCommand, Run: "make check", RequireAutomatedReview: new(false)})
	for _, want := range []string{
		"Run `make check` from the workspace root before Human Review",
		"In Merging, run a focused rebase/smoke gate after a clean rebase when the PR already passed current-head validation",
		"Run full `make check` in Merging when code changes, conflicts are resolved, or validation state is stale or unknown",
		"REST check-runs",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Instructions() missing %q:\n%s", want, got)
		}
	}
}

func configsEqual(left Config, right Config) bool {
	return left.Kind == right.Kind &&
		left.Run == right.Run &&
		left.ApprovalLabel == right.ApprovalLabel &&
		left.CIFailureAction == right.CIFailureAction &&
		boolPointerEqual(left.RequireAutomatedReview, right.RequireAutomatedReview) &&
		validatorConfigsEqual(left.Validator, right.Validator) &&
		artifactConfigsEqual(left.Artifact, right.Artifact)
}

func boolPointerEqual(left *bool, right *bool) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func validatorConfigsEqual(left ValidatorConfig, right ValidatorConfig) bool {
	if left.Enabled != right.Enabled || left.Model != right.Model || left.MinScore != right.MinScore || len(left.BlockOn) != len(right.BlockOn) {
		return false
	}
	for i := range left.BlockOn {
		if left.BlockOn[i] != right.BlockOn[i] {
			return false
		}
	}
	return true
}

func artifactConfigsEqual(left ArtifactConfig, right ArtifactConfig) bool {
	return left.StatusField == right.StatusField &&
		stringSlicesEqual(left.PassStatuses, right.PassStatuses) &&
		stringSlicesEqual(left.WaitStatuses, right.WaitStatuses) &&
		stringSlicesEqual(left.ReworkStatuses, right.ReworkStatuses)
}

func stringSlicesEqual(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
