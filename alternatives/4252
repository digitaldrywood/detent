package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestOpenCodeLive(t *testing.T) {
	model := os.Getenv("DETENT_OPENCODE_SMOKE_MODEL")
	if model == "" {
		t.Fatal("explicit smoke model required")
	}
	b, _ := NewAgentBackend(Options{TurnTimeout: 2 * time.Minute})
	var output strings.Builder
	var calls int
	tools := []runner.AgentTool{{Name: "pilot_value", Description: "Read the required value for this test", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}
	result, err := b.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Workspace: t.TempDir(), TempDir: t.TempDir(), Model: model, SupplementalTools: true, Prompt: "Call the pilot_value MCP tool. Reply with its returned value exactly. Do not read files, run commands, or create child sessions."}, tools, func(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		if call.Name != "pilot_value" {
			return runner.AgentToolResult{}, errors.New("unexpected tool")
		}
		calls++
		return runner.AgentToolResult{Content: `{"value":"detent-opencode-connected"}`, Success: true}, nil
	}, func(u runner.AgentUpdate) error {
		if u.Type == runner.AgentUpdateMessageDelta {
			output.WriteString(u.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(output.String(), "detent-opencode-connected") || result.SessionID == "" {
		t.Fatalf("calls=%d output=%q session=%q", calls, output.String(), result.SessionID)
	}
	t.Logf("model=%s scoped_tool_calls=%d session=%s", model, calls, result.SessionID)
}
