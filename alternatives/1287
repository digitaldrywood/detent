package genkitbackend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/firebase/genkit/go/ai"
)

const openAIResponsesURL = "https://api.openai.com/v1/responses"

type openAIResponses struct {
	client *http.Client
	key    string
	url    string
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	InputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type responseOutput struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type completedResponse struct {
	Status string           `json:"status"`
	Output []responseOutput `json:"output"`
	Usage  responseUsage    `json:"usage"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func NewOpenAI(key string) (*Backend, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("OPENAI_API_KEY is required")
	}
	provider := &openAIResponses{client: http.DefaultClient, key: key, url: openAIResponsesURL}
	return newBackend(provider.generate), nil
}

func NewProvider(name, key string) (*Backend, error) {
	switch name {
	case "openai":
		return NewOpenAI(key)
	default:
		return nil, fmt.Errorf("unsupported coordinator provider %q", name)
	}
}

func (p *openAIResponses) generate(ctx context.Context, request *ai.ModelRequest, stream ai.ModelStreamCallback) (*ai.ModelResponse, error) {
	input, instructions, err := responsesInput(request.Messages)
	if err != nil {
		return nil, err
	}
	config, ok := request.Config.(map[string]any)
	if !ok {
		return nil, errors.New("invalid OpenAI model configuration")
	}
	effort, ok := config["reasoning_effort"].(string)
	if !ok {
		return nil, errors.New("invalid OpenAI reasoning effort")
	}
	body := map[string]any{
		"model": Model, "input": input, "instructions": instructions,
		"reasoning": map[string]string{"effort": defaultEffort(effort)},
		"stream":    true, "store": false,
	}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{
				"type": "function", "name": tool.Name, "description": tool.Description,
				"parameters": tool.InputSchema, "strict": false,
			})
		}
		body["tools"] = tools
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("create OpenAI request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+p.key)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := p.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("OpenAI Responses request: %w", err)
	}
	defer func() { _ = httpResponse.Body.Close() }()
	if httpResponse.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI Responses returned HTTP %d", httpResponse.StatusCode)
	}
	return readResponsesStream(ctx, httpResponse.Body, stream)
}

func responsesInput(messages []*ai.Message) ([]any, string, error) {
	input := make([]any, 0, len(messages))
	var instructions strings.Builder
	for _, message := range messages {
		if message == nil {
			continue
		}
		if message.Role == ai.RoleSystem {
			if instructions.Len() > 0 {
				instructions.WriteString("\n\n")
			}
			instructions.WriteString(message.Text())
			continue
		}
		for _, part := range message.Content {
			switch {
			case part.IsText():
				role := "user"
				if message.Role == ai.RoleModel {
					role = "assistant"
				}
				input = append(input, map[string]any{"role": role, "content": part.Text})
			case part.IsToolRequest():
				arguments, err := json.Marshal(part.ToolRequest.Input)
				if err != nil {
					return nil, "", fmt.Errorf("encode tool call: %w", err)
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": part.ToolRequest.Ref,
					"name": part.ToolRequest.Name, "arguments": string(arguments)})
			case part.IsToolResponse():
				output, err := json.Marshal(part.ToolResponse.Output)
				if err != nil {
					return nil, "", fmt.Errorf("encode tool result: %w", err)
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": part.ToolResponse.Ref,
					"output": string(output)})
			}
		}
	}
	return input, instructions.String(), nil
}

func readResponsesStream(ctx context.Context, reader io.Reader, stream ai.ModelStreamCallback) (*ai.ModelResponse, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var data strings.Builder
	var completed *completedResponse
	var streamedText strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data: "))
			continue
		}
		if line != "" || data.Len() == 0 {
			continue
		}
		var event struct {
			Type     string             `json:"type"`
			Delta    string             `json:"delta"`
			Response *completedResponse `json:"response"`
			Message  string             `json:"message"`
		}
		if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
			return nil, fmt.Errorf("decode OpenAI stream: %w", err)
		}
		data.Reset()
		switch event.Type {
		case "response.output_text.delta":
			streamedText.WriteString(event.Delta)
			if stream != nil && event.Delta != "" {
				if err := stream(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{ai.NewTextPart(event.Delta)}}); err != nil {
					return nil, err
				}
			}
		case "response.completed":
			completed = event.Response
		case "response.failed", "error":
			return nil, errors.New("OpenAI Responses generation failed")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read OpenAI stream: %w", err)
	}
	if completed == nil || completed.Status != "completed" {
		return nil, errors.New("OpenAI Responses stream ended without a completed response")
	}
	parts := make([]*ai.Part, 0, len(completed.Output))
	for _, output := range completed.Output {
		switch output.Type {
		case "message":
			for _, content := range output.Content {
				if content.Type == "output_text" && content.Text != "" {
					parts = append(parts, ai.NewTextPart(content.Text))
				}
			}
		case "function_call":
			var input map[string]any
			if err := json.Unmarshal([]byte(output.Arguments), &input); err != nil {
				return nil, fmt.Errorf("decode OpenAI tool call: %w", err)
			}
			parts = append(parts, ai.NewToolRequestPart(&ai.ToolRequest{Name: output.Name, Ref: output.CallID, Input: input}))
		}
	}
	if stream != nil && streamedText.Len() == 0 {
		for _, part := range parts {
			if part.IsText() {
				if err := stream(ctx, &ai.ModelResponseChunk{Role: ai.RoleModel, Content: []*ai.Part{part}}); err != nil {
					return nil, err
				}
			}
		}
	}
	return &ai.ModelResponse{Message: ai.NewModelMessage(parts...), FinishReason: ai.FinishReasonStop,
		Usage: &ai.GenerationUsage{InputTokens: completed.Usage.InputTokens, CachedContentTokens: completed.Usage.InputDetails.CachedTokens,
			OutputTokens: completed.Usage.OutputTokens, ThoughtsTokens: completed.Usage.OutputDetails.ReasoningTokens,
			TotalTokens: completed.Usage.TotalTokens}}, nil
}
