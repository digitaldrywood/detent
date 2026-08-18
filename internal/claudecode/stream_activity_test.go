package claudecode

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestTurnStateEmitsStructuredRateLimit(t *testing.T) {
	t.Parallel()

	var event claudeEvent
	if err := json.Unmarshal([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1786828200,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"out_of_credits","isUsingOverage":false},"session_id":"session-capacity"}`), &event); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	var updates []runner.AgentUpdate
	if err := (&turnState{}).apply(event, false, func(update runner.AgentUpdate) error {
		updates = append(updates, update)
		return nil
	}); err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if len(updates) != 1 || updates[0].Type != runner.AgentUpdateRateLimits || updates[0].RateLimits == nil {
		t.Fatalf("updates = %#v, want one rate limit update", updates)
	}
	limits := updates[0].RateLimits
	wantReset := time.Unix(1786828200, 0).UTC()
	if limits.ReachedType != "five_hour" || limits.Primary == nil || limits.Primary.Status != telemetry.RateLimitStatusExhausted || limits.Primary.ResetAt == nil || !limits.Primary.ResetAt.Equal(wantReset) {
		t.Fatalf("RateLimits = %#v, want exhausted five-hour limit reset at %s", limits, wantReset)
	}
	if limits.Primary.ObservedAt == nil {
		t.Fatalf("Primary.ObservedAt = nil, want observation timestamp")
	}
}

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

func TestTurnStateDoesNotRepeatPartialToolStart(t *testing.T) {
	t.Parallel()

	block := contentBlock{ID: "tool-1", Type: "tool_use", Name: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`)}
	state := turnState{sessionID: "session-1", model: "claude-sonnet-4-5"}
	events := []claudeEvent{
		{Type: "stream_event", StreamEvent: &streamEvent{ContentBlock: &block}},
		{Type: "assistant", Message: &claudeMessage{ID: "message-1", Content: []contentBlock{block}}},
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
		if err := state.apply(event, true, func(update runner.AgentUpdate) error {
			updates = append(updates, update)
			return nil
		}); err != nil {
			t.Fatalf("apply() error = %v", err)
		}
	}
	if len(updates) != 2 || updates[0].Type != runner.AgentUpdateToolStarted || updates[1].Type != runner.AgentUpdateToolOutput {
		t.Fatalf("updates = %#v, want one tool start and one result", updates)
	}
}
