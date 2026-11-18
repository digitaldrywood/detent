package genkitbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestBackendWithFakeModel(t *testing.T) {
	tests := []struct {
		name      string
		model     ai.ModelFunc
		tools     []runner.AgentTool
		wantText  string
		wantUsage runner.AgentTokenCounts
		wantError string
		wantCalls int
	}{
		{
			name: "streaming and usage",
			model: func(ctx context.Context, request *ai.ModelRequest, stream ai.ModelStreamCallback) (*ai.ModelResponse, error) {
				if err := stream(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{ai.NewTextPart("Hello, ")}}); err != nil {
					return nil, err
				}
				if err := stream(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{ai.NewTextPart("world.")}}); err != nil {
					return nil, err
				}
				return &ai.ModelResponse{Message: ai.NewModelTextMessage("Hello, world."), FinishReason: ai.FinishReasonStop,
					Usage: &ai.GenerationUsage{InputTokens: 12, CachedContentTokens: 3, OutputTokens: 7, ThoughtsTokens: 2, TotalTokens: 19}}, nil
			},
			wantText: "Hello, world.", wantUsage: runner.AgentTokenCounts{InputTokens: 12, CachedInputTokens: 3, OutputTokens: 7, ReasoningOutputTokens: 2, TotalTokens: 19},
		},
		{
			name: "provider error",
			model: func(context.Context, *ai.ModelRequest, ai.ModelStreamCallback) (*ai.ModelResponse, error) {
				return nil, errors.New("provider unavailable")
			},
			wantError: "provider unavailable",
		},
		{
			name: "cancellation",
			model: func(ctx context.Context, _ *ai.ModelRequest, _ ai.ModelStreamCallback) (*ai.ModelResponse, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			wantError: "context canceled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newBackend(test.model)
			ctx := t.Context()
			if test.name == "cancellation" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var text strings.Builder
			var usage runner.AgentTokenCounts
			_, err := backend.RunTurn(ctx, runner.AgentTurnRequest{Prompt: "Hello", ToolInstructions: "Be concise", Model: Model}, func(update runner.AgentUpdate) error {
				switch update.Type {
				case runner.AgentUpdateMessageDelta:
					text.WriteString(update.Delta)
				case runner.AgentUpdateTokenUsage:
					usage = *update.Tokens.ThreadTotal
				}
				return nil
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || text.String() != test.wantText || usage != test.wantUsage {
				t.Fatalf("turn = %q, usage %+v, error %v", text.String(), usage, err)
			}
		})
	}
}

func TestBackendToolRoundTrip(t *testing.T) {
	attention := runner.AgentTool{Name: "list_attention", Description: "List attention", InputSchema: json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string"}}}`)}
	split := runner.AgentTool{Name: "propose_issue_split", Description: "Propose split", InputSchema: json.RawMessage(`{"type":"object","required":["children"],"properties":{"children":{"type":"array","items":{"type":"object","required":["title","state"],"properties":{"title":{"type":"string"},"state":{"type":"string"}}}}}}`)}
	valid := map[string]any{"children": []any{
		map[string]any{"title": "Storage", "state": "Todo"}, map[string]any{"title": "API", "state": "Todo"},
		map[string]any{"title": "Runner", "state": "Todo"}, map[string]any{"title": "Tests", "state": "Todo"},
		map[string]any{"title": "Docs", "state": "Todo"}, map[string]any{"title": "Acceptance", "state": "Todo"},
	}}
	invalid := map[string]any{"children": []any{
		map[string]any{"title": "Storage", "state": "Todo"}, map[string]any{"title": "API", "state": "Todo"},
		map[string]any{"title": "Runner", "state": "Todo"}, map[string]any{"title": "Tests", "state": "Todo"},
		map[string]any{"title": "Docs"}, map[string]any{"title": "Acceptance"},
	}}
	for _, test := range []struct {
		name        string
		tool        runner.AgentTool
		inputs      []any
		toolError   string
		handlerErr  error
		wantCalls   int
		wantHandled int
		wantError   string
		wantResults []string
		resultCall  int
	}{
		{name: "valid call", tool: attention, inputs: []any{map[string]any{"scope": "project"}}, wantCalls: 2, wantHandled: 1},
		{name: "schema invalid then valid", tool: split, inputs: []any{invalid, valid}, wantCalls: 3, wantHandled: 1, wantResults: []string{"children.4: state is required", "children.5: state is required"}, resultCall: 2},
		{name: "schema invalid exhausts limit", tool: split, inputs: []any{map[string]any{"children": []any{map[string]any{"state": "Todo"}}}, invalid}, wantCalls: 9, wantError: "children.5: state is required", wantResults: []string{"children.4: state is required", "children.5: state is required"}, resultCall: 3},
		{name: "handler validation then valid", tool: split, inputs: []any{valid, valid}, toolError: "Each child requires a bounded title, description and target state", wantCalls: 3, wantHandled: 2},
		{name: "handler validation exhausts limit", tool: split, inputs: []any{valid}, toolError: "Each child requires a bounded title, description and target state", wantCalls: 9, wantHandled: 8, wantError: "Each child requires a bounded title, description and target state"},
		{name: "backend error ends turn", tool: attention, inputs: []any{map[string]any{"scope": "project"}}, handlerErr: errors.New("database unavailable"), wantCalls: 1, wantHandled: 1, wantError: "database unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, handled := 0, 0
			backend := newBackend(func(ctx context.Context, request *ai.ModelRequest, stream ai.ModelStreamCallback) (*ai.ModelResponse, error) {
				calls++
				if calls == 1 {
					if len(request.Tools) != 1 || request.Tools[0].Name != test.tool.Name {
						t.Fatalf("tools = %+v", request.Tools)
					}
				} else {
					last := request.Messages[len(request.Messages)-1]
					if len(last.Content) != 1 || !last.Content[0].IsToolResponse() || last.Content[0].ToolResponse.Ref != fmt.Sprintf("call-%d", calls-1) {
						t.Fatalf("tool result absent from model call %d: %+v", calls, last)
					}
					output, err := json.Marshal(last.Content[0].ToolResponse.Output)
					if err != nil {
						t.Fatal(err)
					}
					if calls == test.resultCall {
						for _, message := range test.wantResults {
							if !strings.Contains(string(output), message) {
								t.Fatalf("tool result = %s, want %q", output, message)
							}
						}
					}
					if test.toolError != "" && (calls == 2 || test.wantError != "") && !strings.Contains(string(output), test.toolError) {
						t.Fatalf("tool result = %s, want %q", output, test.toolError)
					}
				}
				if calls <= len(test.inputs) || test.wantError != "" {
					input := test.inputs[min(calls-1, len(test.inputs)-1)]
					return &ai.ModelResponse{Message: ai.NewModelMessage(ai.NewToolRequestPart(&ai.ToolRequest{Name: test.tool.Name, Ref: fmt.Sprintf("call-%d", calls), Input: input})),
						FinishReason: ai.FinishReasonStop, Usage: &ai.GenerationUsage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}}, nil
				}
				if err := stream(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{ai.NewTextPart("One issue.")}}); err != nil {
					return nil, err
				}
				return &ai.ModelResponse{Message: ai.NewModelTextMessage("One issue."), FinishReason: ai.FinishReasonStop,
					Usage: &ai.GenerationUsage{InputTokens: 20, CachedContentTokens: 5, OutputTokens: 4, ThoughtsTokens: 1, TotalTokens: 24}}, nil
			})
			var usage runner.AgentTokenCounts
			var text strings.Builder
			_, err := backend.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Prompt: "What needs attention?"}, []runner.AgentTool{test.tool},
				func(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
					handled++
					if call.Name != test.tool.Name {
						t.Fatalf("tool call = %+v", call)
					}
					if test.handlerErr != nil {
						return runner.AgentToolResult{}, test.handlerErr
					}
					if test.toolError != "" && (handled == 1 || test.wantError != "") {
						output, err := json.Marshal(map[string]string{"error": test.toolError})
						return runner.AgentToolResult{Content: string(output)}, err
					}
					return runner.AgentToolResult{Content: `{"issues":["one"]}`, Success: true}, nil
				}, func(update runner.AgentUpdate) error {
					if update.Type == runner.AgentUpdateMessageDelta {
						text.WriteString(update.Delta)
					}
					if update.Type == runner.AgentUpdateTokenUsage {
						usage = *update.Tokens.ThreadTotal
					}
					return nil
				})
			if calls != test.wantCalls || handled != test.wantHandled {
				t.Fatalf("model calls=%d handled=%d, want %d/%d, error=%v", calls, handled, test.wantCalls, test.wantHandled, err)
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				if test.handlerErr == nil && !errors.Is(err, ai.ErrMaxTurnsExceeded) {
					t.Fatalf("error = %v, want tool-turn limit", err)
				}
				return
			}
			if err != nil || text.String() != "One issue." || usage.InputTokens != int64(10*(calls-1)+20) || usage.CachedInputTokens != 5 || usage.OutputTokens != int64(3*(calls-1)+4) || usage.ReasoningOutputTokens != 1 {
				t.Fatalf("turn calls=%d text=%q usage=%+v error=%v", calls, text.String(), usage, err)
			}
		})
	}
}

func TestNewOpenAIRequiresKey(t *testing.T) {
	if _, err := NewOpenAI(" "); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := NewProvider("unknown", "test-key"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
