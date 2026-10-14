package genkitbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/core/status"
	"github.com/firebase/genkit/go/genkit"

	"github.com/digitaldrywood/detent/internal/runner"
)

const Model = "gpt-6-luna"

type Backend struct {
	genkit *genkit.Genkit
	model  ai.Model
}

type modelPlugin struct {
	name      string
	modelID   string
	modelFunc ai.ModelFunc
}

func (p *modelPlugin) Name() string { return p.name }

func (p *modelPlugin) Init(context.Context) []api.Action {
	model := ai.NewModelAction[any](p.name+"/"+p.modelID, &ai.ModelOptions{
		Label:    "OpenAI Luna",
		Supports: &ai.ModelSupports{Multiturn: true, SystemRole: true, Tools: true},
	}, func(ctx context.Context, request *ai.ModelRequest, _ any, stream ai.ModelStreamCallback) (*ai.ModelResponse, error) {
		response, err := p.modelFunc(ctx, request, stream)
		if response != nil && response.Usage != nil {
			if report, ok := ctx.Value(usageContextKey{}).(usageReporter); ok {
				if reportErr := report(response.Usage); reportErr != nil {
					return nil, reportErr
				}
			}
		}
		return response, err
	})
	return []api.Action{model}
}

type usageContextKey struct{}

type usageReporter func(*ai.GenerationUsage) error

func newBackend(modelFunc ai.ModelFunc) *Backend {
	g := genkit.Init(context.Background(), genkit.WithPlugins(&modelPlugin{name: "openai", modelID: Model, modelFunc: modelFunc}))
	model := genkit.LookupModel(g, "openai/"+Model)
	return &Backend{genkit: g, model: model}
}

func (b *Backend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return b.RunTurnWithTools(ctx, request, nil, nil, onUpdate)
}

func (b *Backend) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handle runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if model := strings.TrimSpace(request.Model); model != "" && model != Model {
		return runner.AgentTurnResult{}, fmt.Errorf("unsupported coordinator model %q", model)
	}
	if effort := strings.TrimSpace(request.ReasoningEffort); effort != "" && effort != "low" && effort != "medium" {
		return runner.AgentTurnResult{}, fmt.Errorf("unsupported coordinator effort %q", effort)
	}
	if onUpdate == nil {
		onUpdate = func(runner.AgentUpdate) error { return nil }
	}
	if len(tools) > 0 && handle == nil {
		return runner.AgentTurnResult{}, errors.New("coordinator tools require a handler")
	}
	registered := make([]ai.ToolRef, 0, len(tools))
	for _, tool := range tools {
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			return runner.AgentTurnResult{}, fmt.Errorf("decode %s tool schema: %w", tool.Name, err)
		}
		tool := tool
		registered = append(registered, ai.NewTool(tool.Name, tool.Description, func(_ *ai.ToolContext, input any) (any, error) {
			arguments, err := json.Marshal(input)
			if err != nil {
				return nil, err
			}
			result, err := handle(ctx, runner.AgentToolCall{Name: tool.Name, Arguments: arguments})
			if err != nil {
				return nil, err
			}
			var output any
			if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
				return nil, fmt.Errorf("decode %s tool result: %w", tool.Name, err)
			}
			return output, nil
		}, ai.WithInputSchema(schema)))
	}
	usage := runner.AgentTokenCounts{}
	ctx = context.WithValue(ctx, usageContextKey{}, usageReporter(func(value *ai.GenerationUsage) error {
		usage.InputTokens += int64(value.InputTokens)
		usage.CachedInputTokens += int64(value.CachedContentTokens)
		usage.OutputTokens += int64(value.OutputTokens)
		usage.ReasoningOutputTokens += int64(value.ThoughtsTokens)
		usage.TotalTokens += int64(value.TotalTokens)
		last := &runner.AgentTokenCounts{
			InputTokens: int64(value.InputTokens), CachedInputTokens: int64(value.CachedContentTokens),
			OutputTokens: int64(value.OutputTokens), ReasoningOutputTokens: int64(value.ThoughtsTokens), TotalTokens: int64(value.TotalTokens),
		}
		return onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Model: Model, Tokens: runner.AgentTokenUsage{
			InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
			OutputTokens: usage.OutputTokens, ReasoningOutputTokens: usage.ReasoningOutputTokens,
			TotalTokens: usage.TotalTokens, Last: last, ThreadTotal: &usage,
		}})
	}))
	options := []ai.GenerateOption{
		ai.WithModel(b.model), ai.WithSystem(request.ToolInstructions), ai.WithPrompt(request.Prompt),
		ai.WithMaxTurns(8), ai.WithTools(registered...),
		ai.WithUse(ai.MiddlewareFunc(func(context.Context) (*ai.Hooks, error) {
			return &ai.Hooks{WrapTool: func(ctx context.Context, params *ai.ToolParams, next ai.ToolNext) (*ai.MultipartToolResponse, error) {
				response, err := next(ctx, params)
				if errors.Is(err, status.ErrInvalidInput) {
					return &ai.MultipartToolResponse{Output: map[string]any{"error": err.Error()}}, nil
				}
				return response, err
			}}, nil
		})),
		ai.WithConfig(map[string]any{"reasoning_effort": defaultEffort(request.ReasoningEffort)}),
		ai.WithStreaming(func(_ context.Context, chunk *ai.ModelResponseChunk) error {
			for _, part := range chunk.Content {
				if part.IsText() && part.Text != "" {
					if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: part.Text}); err != nil {
						return err
					}
				}
			}
			return nil
		}),
	}
	response, err := genkit.Generate(ctx, b.genkit, options...)
	if err != nil {
		if errors.Is(err, ai.ErrMaxTurnsExceeded) {
			if message := lastToolError(response); message != "" {
				return runner.AgentTurnResult{}, fmt.Errorf("%w: %s", err, message)
			}
		}
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{}, nil
}

func lastToolError(response *ai.ModelResponse) string {
	if response == nil || response.Request == nil {
		return ""
	}
	for _, message := range slices.Backward(response.Request.Messages) {
		for _, part := range slices.Backward(message.Content) {
			if part.IsToolResponse() {
				if output, ok := part.ToolResponse.Output.(map[string]any); ok {
					if message, ok := output["error"].(string); ok && message != "" {
						return message
					}
				}
			}
		}
	}
	return ""
}

func defaultEffort(effort string) string {
	if effort == "" {
		return "low"
	}
	return effort
}
