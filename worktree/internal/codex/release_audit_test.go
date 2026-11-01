package codex

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/releaseaudit"
)

func TestReleaseAuditProviderRestrictsToolsAndOwnsScratch(t *testing.T) {
	t.Parallel()
	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.157.0"}`),
		responseMessage(t, 5, `{"config":{"mcp_servers":{"host-files":{"command":"file-reader"}}}}`),
		responseMessage(t, 2, `{"thread":{"id":"audit-thread","model":"selected-model"}}`),
		responseMessage(t, 3, `{"turn":{"id":"audit-turn"}}`),
		notificationMessage(t, "item/agentMessage/delta", `{"threadId":"audit-thread","turnId":"audit-turn","itemId":"message","delta":"[]"}`),
		notificationMessage(t, "thread/tokenUsage/updated", `{"threadId":"audit-thread","turnId":"audit-turn","tokenUsage":{"total":{"inputTokens":10,"outputTokens":3,"cachedInputTokens":4,"totalTokens":13}}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"audit-thread","turn":{"id":"audit-turn","status":"completed"}}`),
	})
	factory := &workerTempCapturingTransportFactory{transport: transport}
	server, err := NewAppServer(factory, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewReleaseAuditProvider(server, "selected-model")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	var usage releaseaudit.Usage
	result, err := provider.Invoke(t.Context(), releaseaudit.Invocation{Scratch: scratch, Prompt: "Audit the supplied source."}, func(update releaseaudit.Usage) error { usage = update; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "audit-thread" || string(result.Output) != "[]" || usage != (releaseaudit.Usage{Input: 10, Output: 3, Cached: 4}) {
		t.Fatalf("result=%+v usage=%+v", result, usage)
	}
	if factory.tempDir != scratch || factory.workspace != scratch || factory.variables["CODEX_HOME"] != filepath.Join(scratch, "codex") || factory.variables["HOME"] != scratch {
		t.Fatalf("provider escaped job scratch: %+v", factory)
	}
	sent := transport.sentMessages()
	assertRequest(t, sent[2], 5, "config/read")
	assertJSONContains(t, sent[3].Params, "permissions", "detent-text-only")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[3].Params, "config.mcp_servers.host-files.enabled", false)
	var request struct {
		Tools  []DynamicTool  `json:"dynamicTools"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(sent[3].Params, &request); err != nil {
		t.Fatal(err)
	}
	for _, feature := range []string{"shell_tool", "unified_exec", "code_mode", "multi_agent", "hooks", "plugins", "memories"} {
		if value, ok := request.Config["features."+feature]; !ok || value != false {
			t.Fatalf("native tool remains available: %s=%v", feature, value)
		}
	}
	if len(request.Tools) != 3 || request.Tools[0].Name != "source_paths" || request.Tools[1].Name != "read_source" || request.Tools[2].Name != "read_instruction" {
		t.Fatalf("unexpected audit tools without an OS sandbox: %+v", request.Tools)
	}
}
