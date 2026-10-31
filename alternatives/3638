package workpad

import "testing"

func TestSignalFromWorkpad(t *testing.T) {
	t.Parallel()
	complete := "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "body section", body: "## Codex Workpad\n" + complete, want: true},
		{name: "nested validation section", body: "## Codex Workpad\n### Validation\n" + complete, want: true},
		{name: "protocol example only", body: "## Protocol\n" + complete},
		{name: "example after workpad", body: "## Codex Workpad\nPlan in progress.\n## Protocol\n" + complete},
		{name: "fenced workpad heading", body: "```markdown\n## Codex Workpad\n```\n" + complete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signal, found := SignalFromWorkpad(tc.body, "issue-url", "owner/repo")
			if found != tc.want {
				t.Fatalf("found = %t, want %t", found, tc.want)
			}
			if found && (signal.Invalid != nil || signal.Status != StatusComplete || signal.CommentURL != "issue-url") {
				t.Fatalf("signal = %#v", signal)
			}
		})
	}
}
