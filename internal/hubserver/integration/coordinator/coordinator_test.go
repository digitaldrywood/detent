package coordinator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

const lunaProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"Luna","version":"1"}}`

type contents struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (c *contents) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[ref.Key] = bytes.Clone(content)
	return ref, nil
}

func (c *contents) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	content, ok := c.objects[ref.Key]
	if !ok {
		return nil, errors.New("conversation content missing")
	}
	return bytes.Clone(content), nil
}

type toolBackend struct {
	calls   chan runner.AgentToolCall
	results chan runner.AgentToolResult
}

func newToolBackend() toolBackend {
	return toolBackend{calls: make(chan runner.AgentToolCall, 1), results: make(chan runner.AgentToolResult, 1)}
}

func (b toolBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return b.RunTurnWithTools(ctx, request, nil, nil, onUpdate)
}

func (b toolBackend) RunTurnWithTools(ctx context.Context, _ runner.AgentTurnRequest, _ []runner.AgentTool, handle runner.AgentToolHandler, _ runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	var call runner.AgentToolCall
	select {
	case call = <-b.calls:
	case <-ctx.Done():
		return runner.AgentTurnResult{}, ctx.Err()
	}
	result, err := handle(ctx, call)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	select {
	case b.results <- result:
	case <-ctx.Done():
		return runner.AgentTurnResult{}, ctx.Err()
	}
	return runner.AgentTurnResult{ThreadID: "thread-tools", TurnID: "turn-tools"}, nil
}

func (b toolBackend) open(t *testing.T) *hubfixture.Hub {
	t.Helper()
	return hubfixture.OpenWith(t, func(cfg *hubfixture.Config) {
		cfg.Conversation = &hubserver.ConversationConfig{Enabled: true, Backend: b, Workspace: t.TempDir(), ArchiveContents: &contents{objects: map[string][]byte{}}}
	})
}

func (b toolBackend) call(t *testing.T, name, arguments string) runner.AgentToolResult {
	t.Helper()
	b.calls <- runner.AgentToolCall{Name: name, Arguments: json.RawMessage(arguments)}
	select {
	case result := <-b.results:
		return result
	case <-t.Context().Done():
		t.Fatalf("coordinator never called %s", name)
	}
	return runner.AgentToolResult{}
}

type conversation struct {
	h       *hubfixture.Hub
	p       hubfixture.Native
	backend toolBackend
	id      string
	turns   int
}

func startConversation(t *testing.T, backend toolBackend, h *hubfixture.Hub, p hubfixture.Native, name, arguments string) (*conversation, runner.AgentToolResult) {
	t.Helper()
	c := &conversation{h: h, p: p, backend: backend}
	backend.calls <- runner.AgentToolCall{Name: name, Arguments: json.RawMessage(arguments)}
	var created struct {
		Data struct {
			Conversation struct {
				ID string `json:"id"`
			} `json:"conversation"`
		} `json:"data"`
	}
	raw := lunaCall(t, h, p.Token, "create_conversation", map[string]any{"request_id": "conversation-" + name, "project_id": string(p.Project.ID), "input": map[string]any{"title": "Example", "first_message": map[string]any{"text": "Example question"}}})
	if err := json.Unmarshal(raw, &created); err != nil || created.Data.Conversation.ID == "" {
		t.Fatalf("create_conversation = %s: %v", raw, err)
	}
	c.id = created.Data.Conversation.ID
	select {
	case result := <-backend.results:
		return c, result
	case <-t.Context().Done():
		t.Fatalf("coordinator never called %s", name)
	}
	return c, runner.AgentToolResult{}
}

func (c *conversation) ask(t *testing.T, name, arguments string) runner.AgentToolResult {
	t.Helper()
	c.turns++
	c.backend.calls <- runner.AgentToolCall{Name: name, Arguments: json.RawMessage(arguments)}
	lunaCall(t, c.h, c.p.Token, "post_conversation_command", map[string]any{"request_id": "turn-" + strconv.Itoa(c.turns), "project_id": string(c.p.Project.ID), "conversation_id": c.id, "input": map[string]any{"kind": "message", "text": "Example follow-up " + strconv.Itoa(c.turns)}})
	select {
	case result := <-c.backend.results:
		return result
	case <-t.Context().Done():
		t.Fatalf("coordinator never called %s", name)
	}
	return runner.AgentToolResult{}
}

func lunaCall(t *testing.T, h *hubfixture.Hub, token, name string, arguments any) json.RawMessage {
	t.Helper()
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"_meta": json.RawMessage(lunaProtocolMeta), "name": name, "arguments": arguments}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, hubfixture.MCPPath(h.Primary), strings.NewReader(string(frame))).WithContext(t.Context())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Mcp-Protocol-Version", hubfixture.MCPProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	var reply hubfixture.MCPReply
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
		t.Fatalf("protocol response: %s: %v", response.Body.Bytes(), err)
	}
	return hubfixture.MCPData(t, reply, false)
}
