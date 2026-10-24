package runner

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
)

func TestHumanReviewPolicyPrompt(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, label string
		human       bool
		want        []string
		absent      string
	}{
		{name: "default disabled", want: []string{"review.human: false", "requires-human-review"}},
		{name: "legacy workaround", label: "detent-optout-disabled", want: []string{"requires-human-review", "detent-optout-disabled"}},
		{name: "enabled", human: true, absent: "Do not add the opt-out label"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Review.Human = tt.human
			cfg.Agent.AutoPromote.OptoutLabel = tt.label
			got := appendHumanReviewPolicyBlock("prompt", cfg)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("prompt = %q, want %q", got, want)
				}
			}
			if tt.absent != "" && strings.Contains(got, tt.absent) {
				t.Fatalf("prompt = %q, unexpectedly contains %q", got, tt.absent)
			}
		})
	}
}
