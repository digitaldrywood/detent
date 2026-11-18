package config

import "testing"

func TestResolveProgressReview(t *testing.T) {
	for _, test := range []struct {
		name, prompt, want string
		invalid            bool
	}{
		{name: "absent", prompt: "## Review\nOther guidance."},
		{name: "project guidance", prompt: "## Review\nOther guidance.\n## Progress Review\nRead the failing assertion.\n### Evidence\nCompare receipts.\n## Release\nIgnore.", want: "Read the failing assertion.\n### Evidence\nCompare receipts."},
		{name: "fenced example ignored", prompt: "```md\n## Progress Review\nExample\n```"},
		{name: "duplicate refused", prompt: "## Progress Review\nFirst\n## Progress Review\nSecond", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveProgressReview(test.prompt)
			if (err != nil) != test.invalid || got != test.want {
				t.Fatalf("guidance=%q err=%v, want %q invalid=%t", got, err, test.want, test.invalid)
			}
		})
	}
}
