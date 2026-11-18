package conversations

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
)

// TestConversationStaleExecutionAttempts covers the attempt identifiers the
// stale_execution log line reads off a rejection.
func TestConversationStaleExecutionAttempts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		expected string
		current  string
		want     bool
	}{
		{name: "owner mismatch", err: ConversationStaleOwner("mismatch", "att_a", "att_b"), expected: "att_a", current: "att_b", want: true},
		{name: "expected only", err: ConversationStaleOwner("mismatch", "att_a", ""), expected: "att_a", want: true},
		{name: "plain stale", err: ConversationStale("no attempt"), want: true},
		{name: "translated stale", err: TranslateConversationError(conversation.ErrStaleExecution), want: true},
		{name: "other native error", err: ConversationQueueFull()},
		{name: "not native", err: errors.New("boom")},
		{name: "nil", err: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected, current, ok := StaleExecutionAttempts(test.err)
			if ok != test.want {
				t.Fatalf("ok = %t, want %t", ok, test.want)
			}
			if expected != test.expected || current != test.current {
				t.Fatalf("attempts = %q/%q, want %q/%q", expected, current, test.expected, test.current)
			}
		})
	}
}
