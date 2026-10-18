package config

import "testing"

func TestHumanReviewProjectSetting(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, setting string
		want          bool
	}{
		{name: "default", want: false},
		{name: "explicit disabled", setting: "review:\n  human: false\n", want: false},
		{name: "explicit enabled", setting: "review:\n  human: true\n", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte("---\ntracker:\n  kind: memory\n" + tt.setting + "---\nWork.\n"))
			if err != nil {
				t.Fatal(err)
			}
			if got := workflow.Config.Review.Human; got != tt.want {
				t.Fatalf("review.human = %t, want %t", got, tt.want)
			}
			if err := workflow.Config.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
