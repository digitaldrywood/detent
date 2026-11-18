package api

import (
	"testing"
	"time"
)

func TestConversationPollTimeoutAddsSlackToWait(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wait time.Duration
		want time.Duration
	}{
		{name: "no wait keeps the client default", wait: 0, want: 0},
		{name: "negative wait keeps the client default", wait: -time.Second, want: 0},
		{name: "production wait gets slack", wait: 25 * time.Second, want: 25*time.Second + conversationPollSlack},
		{name: "short wait gets slack", wait: time.Second, want: time.Second + conversationPollSlack},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := conversationPollTimeout(test.wait); got != test.want {
				t.Fatalf("conversationPollTimeout(%v) = %v, want %v", test.wait, got, test.want)
			}
		})
	}
}
