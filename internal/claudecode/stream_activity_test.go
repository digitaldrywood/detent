package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestClaudeStreamActivity(t *testing.T) {
	const init = `{"type":"system","subtype":"init","session_id":"session-1"}`
	const keepalive = `{"type":"system","subtype":"keepalive","session_id":"session-1"}`
	const foreign = `{"type":"assistant","session_id":"session-2","message":{"id":"foreign","content":[{"type":"text","text":"other session"}]}}`
	const replay = `{"type":"assistant","uuid":"event-1","session_id":"session-1","message":{"id":"message-1","content":[{"type":"text","text":"already seen"}]}}`
	const messageStart = `{"type":"stream_event","session_id":"session-1","event":{"type":"message_start","message":{"id":"partial-1"}}}`
	const textDelta = `{"type":"stream_event","session_id":"session-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"working "}}}`
	const identifiedDelta = `{"type":"stream_event","uuid":"delta-1","session_id":"session-1","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"working "}}}`
	const command = `{"type":"assistant","session_id":"session-1","message":{"id":"message-2","content":[{"type":"tool_use","id":"command-1","name":"Bash","input":{"command":"make test"}}]}}`
	for _, tc := range []struct {
		name           string
		initial, later string
		want           error
		limit          time.Duration
		kind           sessiontimeout.Kind
	}{
		{name: "keepalive", later: keepalive, want: ErrStreamStalled},
		{name: "unrelated session", later: foreign, want: ErrStreamStalled},
		{name: "replay", initial: replay, later: replay, want: ErrStreamStalled},
		{name: "partial output replay", initial: messageStart + "\n" + identifiedDelta, later: identifiedDelta, want: ErrStreamStalled},
		{name: "repeated partial output", initial: messageStart + "\n" + textDelta, later: textDelta},
		{name: "quiet command", initial: command},
		{name: "quiet command exceeds deadline", initial: command, limit: 6 * time.Minute, kind: sessiontimeout.CommandDeadline, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				backend := newTestBackend(t, Options{StallTimeout: 5 * time.Minute, IncludePartialMessages: true})
				stdout, writer := io.Pipe()
				defer stdout.Close()
				go func() {
					defer writer.Close()
					_, _ = io.WriteString(writer, init+"\n"+tc.initial+"\n")
					for range 2 {
						<-time.After(4 * time.Minute)
						_, _ = io.WriteString(writer, tc.later+"\n")
					}
				}()
				lifetime := backend.startSession(t.Context(), runner.AgentTurnRequest{TurnTimeout: tc.limit})
				defer lifetime.Close()
				_, err := backend.consumeStream(lifetime, nil, 0, stdout, "", func(update runner.AgentUpdate) error {
					if tc.later == textDelta && update.Type == runner.AgentUpdateMessageDelta && update.NonActivity {
						t.Error("repeated partial output was omitted from runtime activity")
					}
					return nil
				})
				err = lifetime.Result(err)
				if !errors.Is(err, tc.want) {
					t.Fatalf("stream error = %v, want %v", err, tc.want)
				}
				if tc.kind != "" {
					var timeout *sessiontimeout.Error
					if !errors.As(err, &timeout) || timeout.Kind != tc.kind {
						t.Fatalf("timeout = %v, want kind %s", err, tc.kind)
					}
				}
			})
		})
	}
}

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
	var failed runner.AgentUpdate
	if err := state.emitContentBlock(contentBlock{Type: "tool_result", ToolUseID: "failed", IsError: true, Content: json.RawMessage(`"private command failure"`)}, "", func(u runner.AgentUpdate) error { failed = u; return nil }); err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" {
		t.Fatalf("failed result status=%s", failed.Status)
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

func TestTurnStatePreservesShellCommands(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, tool, input, want string
		partial                 bool
	}{
		{name: "complete", tool: "Bash", input: `{"command":"go test ./..."}`, want: "go test ./..."},
		{name: "partial input completes", tool: "Bash", input: `{"command":"go test ./..."}`, want: "go test ./...", partial: true},
		{name: "non-command tool", tool: "Read", input: `{"command":"not a shell invocation"}`},
		{name: "missing command", tool: "Bash", input: `{}`},
		{name: "invalid command", tool: "Bash", input: `{"command":42}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := turnState{sessionID: "session"}
			block := contentBlock{ID: "tool-1", Type: "tool_use", Name: tt.tool, Input: json.RawMessage(tt.input)}
			var commands []string
			handler := func(update runner.AgentUpdate) error {
				if update.Command != "" {
					commands = append(commands, update.Command)
				}
				return nil
			}
			if tt.partial {
				start := block
				start.Input = json.RawMessage(`{}`)
				if err := state.apply(claudeEvent{Type: "stream_event", StreamEvent: &streamEvent{ContentBlock: &start}}, true, handler); err != nil {
					t.Fatal(err)
				}
			}
			if err := state.apply(claudeEvent{Type: "assistant", Message: &claudeMessage{Content: []contentBlock{block}}}, tt.partial, handler); err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(commands) != 0 {
					t.Fatalf("unexpected commands: %v", commands)
				}
			} else if len(commands) != 1 || commands[0] != tt.want {
				t.Fatalf("commands = %v, want %q once", commands, tt.want)
			}
		})
	}
}

func TestTurnStateResultCost(t *testing.T) {
	t.Parallel()
	zero, paid := int64(0), int64(123_456)
	for _, test := range []struct {
		name, raw string
		want      *int64
	}{
		{name: "missing is unknown", raw: `{"type":"result"}`},
		{name: "explicit zero is known", raw: `{"type":"result","total_cost_usd":0}`, want: &zero},
		{name: "provider amount preserves micros", raw: `{"type":"result","total_cost_usd":0.123456}`, want: &paid},
		{name: "negative amount is unavailable", raw: `{"type":"result","total_cost_usd":-1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var event claudeEvent
			if err := json.Unmarshal([]byte(test.raw), &event); err != nil {
				t.Fatal(err)
			}
			state := turnState{}
			if err := state.applyResult(event, nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(state.reportedCostMicros, test.want) {
				t.Fatalf("reported cost: %v, want %v", state.reportedCostMicros, test.want)
			}
		})
	}
}

func TestClaudeUsageWindows(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, kind, utilization string
		want                    *float64
	}{
		{"five hour", "five_hour", `,"utilization":0.85`, floatPointer(85)},
		{"model window", "seven_day_sonnet", `,"utilization":0.4`, floatPointer(40)},
		{"zero", "five_hour", `,"utilization":0`, floatPointer(0)},
		{"unknown", "weekly", "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var event claudeEvent
			raw := `{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"` + test.kind + `"` + test.utilization + `}}`
			if err := json.Unmarshal([]byte(raw), &event); err != nil {
				t.Fatal(err)
			}
			usage := &runner.ProviderUsage{}
			err := (&turnState{}).apply(event, false, func(update runner.AgentUpdate) error { usage.Observe("claude", update.RateLimits); return nil })
			if err != nil {
				t.Fatal(err)
			}
			windows := usage.Windows("claude")
			if len(windows) != 1 || windows[0].Kind != test.kind || windows[0].ObservedAt.IsZero() || !reflect.DeepEqual(windows[0].PercentUsed, test.want) {
				t.Fatalf("windows = %+v", windows)
			}
		})
	}
}

func floatPointer(value float64) *float64 { return &value }
