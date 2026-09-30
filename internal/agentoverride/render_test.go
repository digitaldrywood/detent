package agentoverride

import "testing"

func TestRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		override Override
		want     string
	}{
		{name: "empty renders nothing", override: Override{}},
		{name: "model only", override: Override{Model: "gpt-6-astra"}, want: "```detent-agent\nschema: 1\nmodel: gpt-6-astra\n```"},
		{name: "effort only", override: Override{Effort: "high"}, want: "```detent-agent\nschema: 1\neffort: high\n```"},
		{
			name:     "model and effort",
			override: Override{Model: "gpt-6-astra", Effort: "low"},
			want:     "```detent-agent\nschema: 1\nmodel: gpt-6-astra\neffort: low\n```",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Render(test.override); got != test.want {
				t.Fatalf("Render() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestApplyToIssueBody(t *testing.T) {
	t.Parallel()
	block := "```detent-agent\nschema: 1\nmodel: gpt-6-astra\neffort: high\n```"
	tests := []struct {
		name     string
		body     string
		override Override
		want     string
	}{
		{
			name:     "appends to a body without a block",
			body:     "Fix the renewal.\n\nConversation: conv_1",
			override: Override{Model: "gpt-6-astra", Effort: "high"},
			want:     "Fix the renewal.\n\nConversation: conv_1\n\n" + block,
		},
		{
			name:     "appends to an empty body",
			body:     "",
			override: Override{Effort: "high"},
			want:     "```detent-agent\nschema: 1\neffort: high\n```",
		},
		{
			name:     "replaces the existing block in place",
			body:     "Fix it.\n\n```detent-agent\nschema: 1\neffort: low\n```\n\nTrailing note.",
			override: Override{Model: "gpt-6-astra", Effort: "high"},
			want:     "Fix it.\n\n" + block + "\n\nTrailing note.",
		},
		{
			name:     "removes the block when nothing is overridden",
			body:     "Fix it.\n\n```detent-agent\nschema: 1\neffort: low\n```\n\nTrailing note.",
			override: Override{},
			want:     "Fix it.\n\nTrailing note.",
		},
		{
			name:     "an empty override leaves a body without a block alone",
			body:     "Fix it.",
			override: Override{},
			want:     "Fix it.",
		},
		{
			name:     "replaces a tilde fenced block",
			body:     "Fix it.\n\n~~~detent-agent\nschema: 1\neffort: low\n~~~",
			override: Override{Effort: "high"},
			want:     "Fix it.\n\n```detent-agent\nschema: 1\neffort: high\n```",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ApplyToIssueBody(test.body, test.override)
			if got != test.want {
				t.Fatalf("ApplyToIssueBody() = %q, want %q", got, test.want)
			}
			// The result must round-trip: what the body says is what the
			// runner reads back.
			parsed, found, err := FromIssueBody(got)
			if err != nil {
				t.Fatalf("FromIssueBody() error = %v", err)
			}
			wantFound := Render(test.override) != ""
			if found != wantFound {
				t.Fatalf("FromIssueBody() found = %t, want %t", found, wantFound)
			}
			if found && (parsed.Model != test.override.Model || parsed.Effort != test.override.Effort) {
				t.Fatalf("FromIssueBody() = %#v, want model %q effort %q", parsed, test.override.Model, test.override.Effort)
			}
			// Applying the same override again changes nothing.
			if again := ApplyToIssueBody(got, test.override); again != got {
				t.Fatalf("ApplyToIssueBody() is not idempotent: %q then %q", got, again)
			}
		})
	}
}
