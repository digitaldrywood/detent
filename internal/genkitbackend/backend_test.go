package genkitbackend

import (
	"context"
	"encoding/json"
	"errors"
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
	calls := 0
	backend := newBackend(func(ctx context.Context, request *ai.ModelRequest, stream ai.ModelStreamCallback) (*ai.ModelResponse, error) {
		calls++
		if calls == 1 {
			if len(request.Tools) != 1 || request.Tools[0].Name != "list_attention" {
				t.Fatalf("tools = %+v", request.Tools)
			}
			return &ai.ModelResponse{Message: ai.NewModelMessage(ai.NewToolRequestPart(&ai.ToolRequest{Name: "list_attention", Ref: "call-1", Input: map[string]any{"scope": "project"}})),
				FinishReason: ai.FinishReasonStop, Usage: &ai.GenerationUsage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}}, nil
		}
		found := false
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if part.IsToolResponse() && part.ToolResponse.Ref == "call-1" {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("tool result absent from second model call")
		}
		if err := stream(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{ai.NewTextPart("One issue.")}}); err != nil {
			return nil, err
		}
		return &ai.ModelResponse{Message: ai.NewModelTextMessage("One issue."), FinishReason: ai.FinishReasonStop,
			Usage: &ai.GenerationUsage{InputTokens: 20, CachedContentTokens: 5, OutputTokens: 4, ThoughtsTokens: 1, TotalTokens: 24}}, nil
	})
	tool := runner.AgentTool{Name: "list_attention", Description: "List attention", InputSchema: json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string"}}}`)}
	var usage runner.AgentTokenCounts
	var text strings.Builder
	_, err := backend.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Prompt: "What needs attention?"}, []runner.AgentTool{tool},
		func(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
			if call.Name != "list_attention" || !strings.Contains(string(call.Arguments), "project") {
				t.Fatalf("tool call = %+v", call)
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
	if err != nil || calls != 2 || text.String() != "One issue." || usage.InputTokens != 30 || usage.CachedInputTokens != 5 || usage.OutputTokens != 7 || usage.ReasoningOutputTokens != 1 {
		t.Fatalf("turn calls=%d text=%q usage=%+v error=%v", calls, text.String(), usage, err)
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
