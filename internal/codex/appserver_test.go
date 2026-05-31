package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAppServerRunTurnStartsLifecycleAndStreamsUpdates(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "item/agentMessage/delta", `{
			"threadId":"thread-1",
			"turnId":"turn-1",
			"itemId":"item-1",
			"delta":"hello"
		}`),
		notificationMessage(t, "thread/tokenUsage/updated", `{
			"threadId":"thread-1",
			"turnId":"turn-1",
			"tokenUsage":{
				"total":{
					"inputTokens":20,
					"cachedInputTokens":5,
					"outputTokens":7,
					"reasoningOutputTokens":3,
					"totalTokens":27
				},
				"last":{
					"inputTokens":2,
					"cachedInputTokens":1,
					"outputTokens":3,
					"reasoningOutputTokens":1,
					"totalTokens":5
				},
				"modelContextWindow":200000
			}
		}`),
		notificationMessage(t, "account/rateLimits/updated", `{
			"rateLimits":{
				"limitId":"codex-primary",
				"limitName":"Codex primary",
				"primary":{
					"usedPercent":12.5,
					"windowDurationMins":300,
					"resetsAt":1780000000
				},
				"secondary":null,
				"credits":{
					"hasCredits":true,
					"unlimited":false,
					"balance":"7.25"
				},
				"planType":null,
				"rateLimitReachedType":null
			}
		}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithClientInfo(ClientInfo{
			Name:    "symphony-test",
			Title:   "Symphony Test",
			Version: "0.1.0",
		}),
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	result, err := server.RunTurn(context.Background(), RunTurnRequest{
		Workspace:         "/tmp/symphony-workspace",
		Prompt:            "Ship issue #18",
		ApprovalPolicy:    json.RawMessage(`"never"`),
		ThreadSandbox:     "workspace-write",
		TurnSandboxPolicy: json.RawMessage(`{"type":"workspaceWrite","networkAccess":true}`),
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if result.ThreadID != "thread-1" || result.TurnID != "turn-1" || result.SessionID != "thread-1-turn-1" {
		t.Fatalf("RunTurn() result = %#v", result)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}

	assertRequest(t, sent[0], 1, "initialize")
	assertJSONContains(t, sent[0].Params, "clientInfo.name", "symphony-test")
	assertJSONContains(t, sent[0].Params, "capabilities.experimentalApi", true)

	if sent[1].Method != "initialized" || len(sent[1].ID) != 0 {
		t.Fatalf("sent[1] = %#v, want initialized notification", sent[1])
	}

	assertRequest(t, sent[2], 2, "thread/start")
	assertJSONContains(t, sent[2].Params, "cwd", "/tmp/symphony-workspace")
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[2].Params, "sandbox", "workspace-write")

	assertRequest(t, sent[3], 3, "turn/start")
	assertJSONContains(t, sent[3].Params, "threadId", "thread-1")
	assertJSONContains(t, sent[3].Params, "input.0.type", "text")
	assertJSONContains(t, sent[3].Params, "input.0.text", "Ship issue #18")
	assertJSONContains(t, sent[3].Params, "cwd", "/tmp/symphony-workspace")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
	assertJSONContains(t, sent[3].Params, "sandboxPolicy.type", "workspaceWrite")

	if len(updates) != 5 {
		t.Fatalf("updates = %d, want 5: %#v", len(updates), updates)
	}
	if updates[0].Type != UpdateTurnStarted || updates[0].ThreadID != "thread-1" || updates[0].TurnID != "turn-1" {
		t.Fatalf("updates[0] = %#v, want turn started", updates[0])
	}
	if updates[1].Type != UpdateAgentMessageDelta || updates[1].Delta != "hello" {
		t.Fatalf("updates[1] = %#v, want agent message delta", updates[1])
	}
	if updates[2].Type != UpdateTokenUsage || updates[2].Tokens.TotalTokens != 27 {
		t.Fatalf("updates[2] = %#v, want token usage total 27", updates[2])
	}
	if updates[2].Tokens.CachedInputTokens != 5 || updates[2].Tokens.ReasoningOutputTokens != 3 {
		t.Fatalf("updates[2].Tokens = %#v", updates[2].Tokens)
	}
	if updates[2].Tokens.ModelContextWindow == nil || *updates[2].Tokens.ModelContextWindow != 200000 {
		t.Fatalf("updates[2].Tokens.ModelContextWindow = %#v", updates[2].Tokens.ModelContextWindow)
	}
	if updates[3].Type != UpdateRateLimits || updates[3].RateLimits == nil {
		t.Fatalf("updates[3] = %#v, want rate limits", updates[3])
	}
	if updates[3].RateLimits.LimitID != "codex-primary" || updates[3].RateLimits.Primary == nil {
		t.Fatalf("updates[3].RateLimits = %#v", updates[3].RateLimits)
	}
	if updates[3].RateLimits.Primary.UsedPercent != 12.5 {
		t.Fatalf("Primary.UsedPercent = %f, want 12.5", updates[3].RateLimits.Primary.UsedPercent)
	}
	if updates[4].Type != UpdateTurnCompleted || updates[4].TurnID != "turn-1" {
		t.Fatalf("updates[4] = %#v, want turn completed", updates[4])
	}
}

func TestAppServerRunTurnRespondsToServerRequests(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.135.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"}}`),
		serverRequestMessage(t, 40, "item/tool/requestUserInput", `{"threadId":"thread-1","turnId":"turn-1"}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		serverRequestMessage(t, 41, "item/commandExecution/requestApproval", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 42, "item/fileChange/requestApproval", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 43, "item/permissions/requestApproval", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 44, "mcpServer/elicitation/request", `{"threadId":"thread-1","turnId":"turn-1"}`),
		serverRequestMessage(t, 45, "custom/request", `{"threadId":"thread-1","turnId":"turn-1"}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/symphony-workspace",
		Prompt:    "Ship issue #18",
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 10 {
		t.Fatalf("sent messages = %d, want 10: %#v", len(sent), sent)
	}

	assertResponseResultContains(t, sent[4], 40, "answers", map[string]any{})
	assertResponseResultContains(t, sent[5], 41, "decision", "decline")
	assertResponseResultContains(t, sent[6], 42, "decision", "decline")
	assertResponseResultContains(t, sent[7], 43, "permissions", map[string]any{})
	assertResponseResultContains(t, sent[8], 44, "action", "decline")
	assertResponseResultContains(t, sent[8], 44, "content", nil)
	assertErrorResponse(t, sent[9], 45, methodNotFoundCode, "unsupported server request: custom/request")
}

func TestAppServerRunTurnReportsResponseErrors(t *testing.T) {
	t.Parallel()

	transport := newFakeAppServerTransport([]Message{
		errorResponseMessage(t, 1, -32000, "initialize failed"),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport})
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: "/tmp/symphony-workspace",
		Prompt:    "Ship issue #18",
	}, nil)
	if err == nil {
		t.Fatal("RunTurn() error = nil, want response error")
	}
	if !errors.Is(err, ErrResponseError) {
		t.Fatalf("RunTurn() error = %v, want ErrResponseError", err)
	}
}

type staticTransportFactory struct {
	transport Transport
}

func (f staticTransportFactory) NewTransport(context.Context) (Transport, error) {
	return f.transport, nil
}

type fakeAppServerTransport struct {
	received []Message
	sent     []Message
}

func newFakeAppServerTransport(received []Message) *fakeAppServerTransport {
	return &fakeAppServerTransport{received: append([]Message(nil), received...)}
}

func (t *fakeAppServerTransport) Send(_ context.Context, msg Message) error {
	t.sent = append(t.sent, msg)
	return nil
}

func (t *fakeAppServerTransport) Receive(context.Context) (Message, error) {
	if len(t.received) == 0 {
		return Message{}, io.EOF
	}
	msg := t.received[0]
	t.received = t.received[1:]
	return msg, nil
}

func (t *fakeAppServerTransport) Close(context.Context) error {
	return nil
}

func (t *fakeAppServerTransport) sentMessages() []Message {
	return append([]Message(nil), t.sent...)
}

func responseMessage(t *testing.T, id int, result string) Message {
	t.Helper()

	return Message{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(mustMarshalJSON(t, id)),
		Result:  json.RawMessage(result),
	}
}

func errorResponseMessage(t *testing.T, id int, code int, message string) Message {
	t.Helper()

	return Message{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(mustMarshalJSON(t, id)),
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
}

func notificationMessage(t *testing.T, method string, params string) Message {
	t.Helper()

	return Message{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  json.RawMessage(params),
	}
}

func serverRequestMessage(t *testing.T, id int, method string, params string) Message {
	t.Helper()

	msg := notificationMessage(t, method, params)
	msg.ID = json.RawMessage(mustMarshalJSON(t, id))
	return msg
}

func assertRequest(t *testing.T, msg Message, id int, method string) {
	t.Helper()

	if msg.Method != method {
		t.Fatalf("Method = %q, want %q", msg.Method, method)
	}
	if string(msg.ID) != mustMarshalJSON(t, id) {
		t.Fatalf("ID = %s, want %d", msg.ID, id)
	}
	if len(msg.Params) == 0 {
		t.Fatalf("Params empty for %s", method)
	}
}

func assertResponseResultContains(t *testing.T, msg Message, id int, path string, want any) {
	t.Helper()

	assertResponseID(t, msg, id)
	if msg.Error != nil {
		t.Fatalf("Error = %#v, want result", msg.Error)
	}
	if len(msg.Result) == 0 {
		t.Fatalf("Result empty for response %d", id)
	}
	assertJSONContains(t, msg.Result, path, want)
}

func assertErrorResponse(t *testing.T, msg Message, id int, code int, message string) {
	t.Helper()

	assertResponseID(t, msg, id)
	if msg.Error == nil {
		t.Fatalf("Error = nil, want code %d", code)
	}
	if msg.Error.Code != code || msg.Error.Message != message {
		t.Fatalf("Error = %#v, want code %d message %q", msg.Error, code, message)
	}
}

func assertResponseID(t *testing.T, msg Message, id int) {
	t.Helper()

	if msg.Method != "" {
		t.Fatalf("Method = %q, want response", msg.Method)
	}
	if string(msg.ID) != mustMarshalJSON(t, id) {
		t.Fatalf("ID = %s, want %d", msg.ID, id)
	}
}

func assertJSONContains(t *testing.T, data json.RawMessage, path string, want any) {
	t.Helper()

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}

	got := lookupJSONPath(t, decoded, path)
	if !jsonValuesEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v in %s", path, got, want, string(data))
	}
}

func lookupJSONPath(t *testing.T, value any, path string) any {
	t.Helper()

	parts := strings.Split(path, ".")
	current := value
	for _, part := range parts {
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[part]
			if !ok {
				t.Fatalf("path %q missing key %q in %#v", path, part, value)
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				t.Fatalf("path %q invalid index %q in %#v", path, part, value)
			}
			current = node[index]
		default:
			t.Fatalf("path %q hit non-container %#v", path, current)
		}
	}

	return current
}

func jsonValuesEqual(got any, want any) bool {
	gotData, err := json.Marshal(got)
	if err != nil {
		return false
	}
	wantData, err := json.Marshal(want)
	if err != nil {
		return false
	}
	var gotCanonical any
	if err := json.Unmarshal(gotData, &gotCanonical); err != nil {
		return false
	}
	var wantCanonical any
	if err := json.Unmarshal(wantData, &wantCanonical); err != nil {
		return false
	}
	return reflect.DeepEqual(gotCanonical, wantCanonical)
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}
