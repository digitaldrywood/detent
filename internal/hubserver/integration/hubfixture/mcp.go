package hubfixture

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	MCPProtocolVersion = "2026-07-28"
	MCPProtocolMeta    = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`
)

var mcpLeakSentinels = []string{"csrf_token", "form_token", "session_owner", "refresh_token", "token_hash", "credential-sensitive-value-sentinel", "private-worktree-sentinel"}

type MCPReply struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func MCPPath(organization tracker.OrganizationID) string {
	return "/api/v2/organizations/" + string(organization) + "/mcp"
}

func MCP(t *testing.T, handler http.Handler, path, token string) func(method, name string, args any) MCPReply {
	t.Helper()
	return func(method, name string, args any) MCPReply {
		t.Helper()
		params := map[string]any{"_meta": json.RawMessage(MCPProtocolMeta)}
		if method == "tools/call" {
			params["name"], params["arguments"] = name, args
		} else if args != nil {
			params["cursor"] = args
		}
		frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(frame))).WithContext(t.Context())
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Mcp-Protocol-Version", MCPProtocolVersion)
		request.Header.Set("Mcp-Method", method)
		if method == "tools/call" && name != "" {
			request.Header.Set("Mcp-Name", name)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		raw := response.Body.Bytes()
		for _, secret := range mcpLeakSentinels {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("protocol leaked %s", secret)
			}
		}
		var reply MCPReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("protocol response: %s: %v", raw, err)
		}
		return reply
	}
}

func MCPData(t *testing.T, reply MCPReply, denied bool) json.RawMessage {
	t.Helper()
	var result struct {
		IsError bool            `json:"isError"`
		Data    json.RawMessage `json:"structuredContent"`
	}
	if len(reply.Result) > 0 {
		if err := json.Unmarshal(reply.Result, &result); err != nil {
			t.Fatal(err)
		}
	}
	failed := result.IsError || len(reply.Error) > 0
	refusal := len(result.Data) == 0
	if denied && !refusal {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(result.Data, &body); err == nil {
			_, hasCode := body["code"]
			_, hasMessage := body["message"]
			refusal = hasCode && hasMessage && len(body) == 2
		}
	}
	if failed != denied || denied && !refusal || !denied && len(result.Data) == 0 {
		t.Fatalf("denied=%t response=%s %s", denied, reply.Result, reply.Error)
	}
	return result.Data
}
