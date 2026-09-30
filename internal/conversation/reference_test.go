package conversation

import (
	"strings"
	"testing"
)

func TestExtractReferences(t *testing.T) {
	t.Parallel()
	conversationID := "conv_9f2c41a0b7d84e6fa1c35d92e4780b16"
	tests := []struct {
		name string
		text string
		want []Reference
	}{
		{name: "none", text: "Nothing to see here."},
		{
			name: "bare number",
			text: "This follows #123.",
			want: []Reference{{Kind: ReferenceIssue, Number: 123, Text: "#123"}},
		},
		{
			name: "project number",
			text: "See parable#3363 for the lock renewal.",
			want: []Reference{{Kind: ReferenceIssue, Project: "parable", Number: 3363, Text: "parable#3363"}},
		},
		{
			name: "organization project number",
			text: "threefold/parable#12 is the parent.",
			want: []Reference{{Kind: ReferenceIssue, Organization: "threefold", Project: "parable", Number: 12, Text: "threefold/parable#12"}},
		},
		{
			name: "conversation id",
			text: "Split out of " + conversationID + " earlier.",
			want: []Reference{{Kind: ReferenceConversation, ConversationID: conversationID, Text: conversationID}},
		},
		{
			name: "several in order without duplicates",
			text: "#7 and parable#7 and #7 again",
			want: []Reference{
				{Kind: ReferenceIssue, Number: 7, Text: "#7"},
				{Kind: ReferenceIssue, Project: "parable", Number: 7, Text: "parable#7"},
			},
		},
		{name: "hex colour is not a reference", text: "The accent is #ff00aa."},
		{name: "zero is not an issue number", text: "Issue #0 does not exist."},
		{name: "short conversation id is not a reference", text: "conv_abc is not an id."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractReferences(test.text)
			if len(got) != len(test.want) {
				t.Fatalf("ExtractReferences(%q) = %#v, want %#v", test.text, got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("ExtractReferences(%q)[%d] = %#v, want %#v", test.text, i, got[i], test.want[i])
				}
			}
		})
	}
}

// TestExtractReferencesBounded proves one message can never carry more than
// MaxReferences references, however many tokens its text holds.
func TestExtractReferencesBounded(t *testing.T) {
	t.Parallel()
	var text strings.Builder
	for i := 1; i <= MaxReferences*3; i++ {
		text.WriteString(" #")
		text.WriteString(strings.TrimSpace(itoa(i)))
	}
	got := ExtractReferences(text.String())
	if len(got) != MaxReferences {
		t.Fatalf("ExtractReferences() returned %d references, want %d", len(got), MaxReferences)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}
