package runner

import (
	"testing"
	"time"
)

func TestPublishAgentActivityContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		update      AgentUpdate
		wantContent string
	}{
		{
			name: "forwards full tool output",
			update: AgentUpdate{
				Type:     AgentUpdateToolOutput,
				ThreadID: "thread-1",
				TurnID:   "turn-1",
				ItemID:   "item-1",
				Tool:     "exec_command",
				Delta:    "complete command output",
			},
			wantContent: "complete command output",
		},
		{
			name: "forwards failed tool backend error",
			update: AgentUpdate{
				Type:                AgentUpdateToolCompleted,
				ThreadID:            "thread-2",
				TurnID:              "turn-2",
				ItemID:              "item-2",
				Tool:                "codex_apps/github.create_pull_request",
				Delta:               "null",
				Status:              "failed",
				BackendErrorMessage: "user rejected MCP tool call",
			},
			wantContent: "user rejected MCP tool call",
		},
		{
			name: "preserves successful tool content",
			update: AgentUpdate{
				Type:                AgentUpdateToolCompleted,
				ThreadID:            "thread-3",
				TurnID:              "turn-3",
				ItemID:              "item-3",
				Tool:                "codex_apps/github.create_pull_request",
				Delta:               `{"url":"https://github.test/acme/widgets/pull/18"}`,
				Status:              "completed",
				BackendErrorMessage: "stale backend error",
			},
			wantContent: `{"url":"https://github.test/acme/widgets/pull/18"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got AgentActivityUpdate
			req := RunRequest{OnActivityUpdate: func(update AgentActivityUpdate) error {
				got = update
				return nil
			}}
			at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
			err := publishAgentActivity(req, 1156, tt.update, at)
			if err != nil {
				t.Fatalf("publishAgentActivity() error = %v", err)
			}
			if got.DetentSessionID != 1156 || got.ProviderSessionID != tt.update.ThreadID || got.Type != tt.update.Type || got.Tool != tt.update.Tool || got.Content != tt.wantContent || got.At != at {
				t.Fatalf("activity update = %#v, want content %q", got, tt.wantContent)
			}
		})
	}
}
