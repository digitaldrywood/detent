package genkitbackend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"
)

func TestOpenAIResponsesStream(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("authorization header missing")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{`"model":"gpt-6-luna"`, `"stream":true`, `"store":false`, `"effort":"low"`, `"name":"list_attention"`} {
			if !strings.Contains(string(body), value) {
				t.Errorf("request missing %s", value)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]},{\"type\":\"function_call\",\"name\":\"list_attention\",\"call_id\":\"call-1\",\"arguments\":\"{\\\"scope\\\":\\\"project\\\"}\"}],\"usage\":{\"input_tokens\":11,\"output_tokens\":4,\"total_tokens\":15,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n")
	}))
	defer server.Close()
	provider := &openAIResponses{client: server.Client(), key: "test-key", url: server.URL}
	request := &ai.ModelRequest{
		Config:   map[string]any{"reasoning_effort": "low"},
		Messages: []*ai.Message{ai.NewSystemTextMessage("Be concise"), ai.NewUserTextMessage("Hello")},
		Tools:    []*ai.ToolDefinition{{Name: "list_attention", Description: "List issues", InputSchema: map[string]any{"type": "object"}}},
	}
	var streamed strings.Builder
	response, err := provider.generate(t.Context(), request, func(_ context.Context, chunk *ai.ModelResponseChunk) error {
		for _, part := range chunk.Content {
			streamed.WriteString(part.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hello" || response.Text() != "Hello" || response.Usage.CachedContentTokens != 3 || response.Usage.ThoughtsTokens != 2 || len(response.Message.Content) != 2 {
		t.Fatalf("stream = %q, response = %+v", streamed.String(), response)
	}
	call := response.Message.Content[1].ToolRequest
	if call == nil || call.Name != "list_attention" || call.Ref != "call-1" {
		t.Fatalf("tool call = %+v", call)
	}
}

func TestOpenAIResponsesErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "HTTP failure", status: http.StatusUnauthorized},
		{name: "incomplete stream", status: http.StatusOK, body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"},
		{name: "provider failure", status: http.StatusOK, body: "data: {\"type\":\"response.failed\"}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			provider := &openAIResponses{client: server.Client(), key: "test-key", url: server.URL}
			_, err := provider.generate(t.Context(), &ai.ModelRequest{Config: map[string]any{}, Messages: []*ai.Message{ai.NewUserTextMessage("Hello")}}, nil)
			if err == nil {
				t.Fatal("expected provider error")
			}
		})
	}
}
