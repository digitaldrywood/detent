package runner

import (
	"testing"
	"time"
)

func TestPublishAgentActivityForwardsFullToolOutput(t *testing.T) {
	t.Parallel()

	var got AgentActivityUpdate
	req := RunRequest{OnActivityUpdate: func(update AgentActivityUpdate) error {
		got = update
		return nil
	}}
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	err := publishAgentActivity(req, 1156, AgentUpdate{
		Type:     AgentUpdateToolOutput,
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		ItemID:   "item-1",
		Tool:     "exec_command",
		Delta:    "complete command output",
	}, at)
	if err != nil {
		t.Fatalf("publishAgentActivity() error = %v", err)
	}
	if got.DetentSessionID != 1156 || got.ProviderSessionID != "thread-1" || got.Type != AgentUpdateToolOutput || got.Tool != "exec_command" || got.Content != "complete command output" || got.At != at {
		t.Fatalf("activity update = %#v", got)
	}
}
