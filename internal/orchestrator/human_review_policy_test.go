package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

func TestHumanReviewDisabledRouting(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		human  bool
		labels []string
		gate   string
		want   string
	}{
		{name: "default completion", want: "Rework"},
		{name: "legacy opt out label", labels: []string{"requires-human-review"}, want: "Human Review"},
		{name: "configured opt out label", labels: []string{"custom-review"}, want: "Human Review"},
		{name: "human review gate disabled", gate: gate.KindHumanReview, want: "Rework"},
		{name: "human review enabled", human: true, gate: gate.KindHumanReview, want: "Human Review"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := completionTransitionIssue("In Progress", "OPEN")
			issue.Labels = tt.labels
			cfg := AutoPromoteConfig{Enabled: true, HumanReview: &tt.human, OptoutLabel: "custom-review", Gate: gate.Config{Kind: tt.gate}}
			got := completedActiveReviewTargetState(issue, FinalStateCompleted, "", normalizedStates([]string{"In Progress"}), normalizedStates([]string{"Done"}), cfg)
			if got != tt.want {
				t.Fatalf("completion target = %q, want %q", got, tt.want)
			}
			if tt.labels != nil {
				decision := EvaluateAutoPromote(issue, AutoPromoteSummary{PullRequestPresent: true}, cfg, time.Now())
				if decision.Reason != AutoPromoteReasonOptoutLabel {
					t.Fatalf("decision = %#v, want optout_label", decision)
				}
			}
		})
	}
}
