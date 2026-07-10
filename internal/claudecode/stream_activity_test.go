package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestTurnStateEmitsToolContent(t *testing.T) {
	t.Parallel()

	state := turnState{sessionID: "session-1", model: "claude-sonnet-4-5"}
	events := []claudeEvent{
		{
			Type: "assistant",
			Message: &claudeMessage{ID: "message-1", Content: []contentBlock{{
				ID:    "tool-1",
				Type:  "tool_use",
				Name:  "Bash",
				Input: json.RawMessage(`{"command":"go test ./..."}`),
			}}},
		},
		{
			Type: "user",
			Message: &claudeMessage{ID: "message-2", Content: []contentBlock{{
				Type:      "tool_result",
				ToolUseID: "tool-1",
				Content:   json.RawMessage(`"ok package"`),
			}}},
		},
	}

	var updates []runner.AgentUpdate
	for _, event := range events {
		if err := state.apply(event, false, func(update runner.AgentUpdate) error {
			updates = append(updates, update)
			return nil
		}); err != nil {
			t.Fatalf("apply() error = %v", err)
		}
	}
	if len(updates) != 2 {
		t.Fatalf("updates = %#v, want tool start and output", updates)
	}
	if updates[0].Type != runner.AgentUpdateToolStarted || updates[0].Tool != "Bash" || updates[0].Delta != `{"command":"go test ./..."}` {
		t.Fatalf("tool start = %#v", updates[0])
	}
	if updates[1].Type != runner.AgentUpdateToolOutput || updates[1].ItemID != "tool-1" || updates[1].Delta != "ok package" {
		t.Fatalf("tool output = %#v", updates[1])
	}
}
