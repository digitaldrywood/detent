package conversations

import (
	"testing"
	"time"
)

// TestConversationDefaultSettleWindow proves the configuration default is the
// 24 hours.
func TestConversationDefaultSettleWindow(t *testing.T) {
	t.Parallel()
	if got := (ConversationConfig{Enabled: true}).Normalized().SettleWindow; got != 24*time.Hour {
		t.Fatalf("SettleWindow = %s, want 24h", got)
	}
}
