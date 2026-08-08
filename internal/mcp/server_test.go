package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

const (
	initializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`
	initializedNotice = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
)

func TestInitializeNegotiatesProtocolVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested string
		want      string
	}{
		{name: "current", requested: "2025-11-25", want: "2025-11-25"},
		{name: "June 2025", requested: "2025-06-18", want: "2025-06-18"},
		{name: "March 2025", requested: "2025-03-26", want: "2025-03-26"},
		{name: "November 2024", requested: "2024-11-05", want: "2024-11-05"},
		{name: "unsupported", requested: "2099-01-01", want: ProtocolVersion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := strings.Replace(initializeRequest, ProtocolVersion, test.requested, 1)
			responses := exchange(t, nil, request)
			if len(responses) != 1 {
				t.Fatalf("responses = %d, want 1", len(responses))
			}
			var result struct {
				ProtocolVersion string `json:"protocolVersion"`
				Capabilities    struct {
					Tools map[string]any `json:"tools"`
				} `json:"capabilities"`
				ServerInfo struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"serverInfo"`
			}
			decodeResult(t, responses[0], &result)
			if result.ProtocolVersion != test.want || result.ServerInfo.Name != serverName || result.ServerInfo.Version != "test-version" || result.Capabilities.Tools == nil {
				t.Fatalf("initialize result = %#v, want protocol %q and tool capability", result, test.want)
			}
		})
	}
}

func TestListToolsUsesSharedCatalog(t *testing.T) {
	t.Parallel()

	responses := exchange(t, nil,
		initializeRequest,
		initializedNotice,
		`{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{}}`,
	)
	if len(responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(responses))
	}
	var result struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	decodeResult(t, responses[1], &result)
	definitions := operatortool.Catalog()
	if len(result.Tools) != len(definitions) {
		t.Fatalf("tools = %d, want %d", len(result.Tools), len(definitions))
	}
	for index, definition := range definitions {
		tool := result.Tools[index]
		if tool.Name != definition.Name || tool.Description != definition.Description || !jsonEqual(tool.InputSchema, definition.InputSchema) {
			t.Fatalf("tool[%d] = %#v, want %#v", index, tool, definition)
		}
	}
}

func TestProtocolErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []string
		wantCode int
		wantID   string
	}{
		{name: "malformed JSON", messages: []string{`{"jsonrpc":`}, wantCode: codeParseError, wantID: "null"},
		{name: "non-object request", messages: []string{`[]`}, wantCode: codeInvalidRequest, wantID: "null"},
		{name: "invalid request ID", messages: []string{`{"jsonrpc":"2.0","id":{},"method":"ping"}`}, wantCode: codeInvalidRequest, wantID: "null"},
		{name: "operation before initialized", messages: []string{`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`}, wantCode: codeNotInitialized, wantID: "2"},
		{name: "unknown method", messages: []string{initializeRequest, initializedNotice, `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`}, wantCode: codeMethodNotFound, wantID: "2"},
		{name: "malformed list parameters", messages: []string{initializeRequest, initializedNotice, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"cursor":"next"}}`}, wantCode: codeInvalidParams, wantID: "2"},
		{name: "arguments are not object", messages: []string{initializeRequest, initializedNotice, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"board_state","arguments":[]}}`}, wantCode: codeInvalidParams, wantID: "2"},
		{name: "unknown tool", messages: []string{initializeRequest, initializedNotice, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"missing","arguments":{}}}`}, wantCode: codeInvalidParams, wantID: "2"},
		{name: "mutation tool", messages: []string{initializeRequest, initializedNotice, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"propose_move_item","arguments":{}}}`}, wantCode: codeInvalidParams, wantID: "2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			executor := &countingExecutor{}
			responses := exchange(t, executor, test.messages...)
			response := responses[len(responses)-1]
			if response.Error == nil || response.Error.Code != test.wantCode || string(response.ID) != test.wantID {
				t.Fatalf("response = %#v, want code %d and ID %s", response, test.wantCode, test.wantID)
			}
			if (test.name == "unknown tool" || test.name == "mutation tool") && executor.calls.Load() != 0 {
				t.Fatalf("executor calls = %d, want 0", executor.calls.Load())
			}
		})
	}
}

func TestToolCallReturnsStructuredContentOnce(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 8, 2, 30, 0, 0, time.UTC)
	executor := &staticExecutor{result: operatortool.Result{Content: json.RawMessage(fmt.Sprintf(`{"generated_at":%q,"freshness":"live","items":[]}`, observedAt.Format(time.RFC3339)))}}
	client := startLiveServer(t, executor)
	client.write(initializeRequest)
	client.read()
	client.write(initializedNotice)
	client.write(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"board_state","arguments":{"limit":1}}}`)
	response := client.read()
	client.close()

	var result struct {
		Content           []json.RawMessage          `json:"content"`
		StructuredContent map[string]json.RawMessage `json:"structuredContent"`
		IsError           bool                       `json:"isError"`
	}
	decodeResult(t, response, &result)
	if result.IsError || len(result.Content) != 0 || string(result.StructuredContent["freshness"]) != `"live"` {
		t.Fatalf("tool result = %#v", result)
	}
	if _, wrapped := result.StructuredContent["content"]; wrapped {
		t.Fatalf("structured result was wrapped: %#v", result.StructuredContent)
	}
}

func TestToolCallReturnsTextContentForLegacyVersions(t *testing.T) {
	t.Parallel()

	content := json.RawMessage(`{"generated_at":"2026-08-08T02:30:00Z","freshness":"live","items":[]}`)
	tests := []struct {
		name    string
		version string
	}{
		{name: "March 2025", version: "2025-03-26"},
		{name: "November 2024", version: "2024-11-05"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			executor := &staticExecutor{result: operatortool.Result{Content: content}}
			client := startLiveServer(t, executor)
			client.write(strings.Replace(initializeRequest, ProtocolVersion, test.version, 1))
			client.read()
			client.write(initializedNotice)
			client.write(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"board_state","arguments":{"limit":1}}}`)
			response := client.read()
			client.close()

			var result struct {
				Content           []textContent   `json:"content"`
				StructuredContent json.RawMessage `json:"structuredContent"`
			}
			decodeResult(t, response, &result)
			if len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text != string(content) || len(result.StructuredContent) != 0 {
				t.Fatalf("tool result = %#v, want one JSON text content block", result)
			}
		})
	}
}

func TestToolExecutionErrorIsDistinctFromEmptyResult(t *testing.T) {
	t.Parallel()

	executor := &staticExecutor{err: errors.New("dashboard API is unreachable")}
	client := startLiveServer(t, executor)
	client.write(initializeRequest)
	client.read()
	client.write(initializedNotice)
	client.write(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_health","arguments":{}}}`)
	response := client.read()
	client.close()

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	decodeResult(t, response, &result)
	if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != "dashboard API is unreachable" || len(result.StructuredContent) != 0 {
		t.Fatalf("tool error result = %#v", result)
	}
}

func TestToolCallReturnsArgumentValidationError(t *testing.T) {
	t.Parallel()

	client := startLiveServer(t, operatortool.NewExecutor(operatortool.Dependencies{}))
	client.write(initializeRequest)
	client.read()
	client.write(initializedNotice)
	client.write(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"board_state","arguments":{"limit":"many"}}}`)
	response := client.read()
	client.close()

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	decodeResult(t, response, &result)
	if !result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, operatortool.ErrInvalidArguments.Error()) {
		t.Fatalf("tool argument result = %#v", result)
	}
}

func TestDuplicateRequestIDCancelsOriginal(t *testing.T) {
	t.Parallel()

	executor := newBlockingExecutor()
	client := startLiveServer(t, executor)
	client.write(initializeRequest)
	client.read()
	client.write(initializedNotice)
	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"board_state","arguments":{}}}`
	client.write(call)
	executor.waitStarted(t)
	client.write(call)
	response := client.read()
	if response.Error == nil || response.Error.Code != codeInvalidRequest || response.Error.Message != "Duplicate request ID" {
		t.Fatalf("duplicate response = %#v", response)
	}
	executor.waitCancelled(t)
	client.close()
	if executor.calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls.Load())
	}
	client.requireNoFrames()
}

func TestCancellationSuppressesResponse(t *testing.T) {
	t.Parallel()

	executor := newBlockingExecutor()
	client := startLiveServer(t, executor)
	client.write(initializeRequest)
	client.read()
	client.write(initializedNotice)
	client.write(`{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{"name":"recent_activity","arguments":{}}}`)
	executor.waitStarted(t)
	client.write(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"slow","reason":"test"}}`)
	executor.waitCancelled(t)
	client.close()
	client.requireNoFrames()
}

func TestConcurrentCallsAreRaceSafe(t *testing.T) {
	t.Parallel()

	const callCount = 12
	executor := newConcurrentExecutor(callCount)
	client := startLiveServer(t, executor)
	client.write(initializeRequest)
	client.read()
	client.write(initializedNotice)
	for id := 1; id <= callCount; id++ {
		client.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"fleet_health","arguments":{}}}`, id))
	}
	executor.waitAllStarted(t)
	if executor.maximum.Load() < 2 {
		t.Fatalf("maximum concurrency = %d, want at least 2", executor.maximum.Load())
	}
	close(executor.release)
	ids := make([]int, 0, callCount)
	for range callCount {
		response := client.read()
		var id int
		if err := json.Unmarshal(response.ID, &id); err != nil {
			t.Fatalf("decode response ID: %v", err)
		}
		ids = append(ids, id)
	}
	client.close()
	sort.Ints(ids)
	want := make([]int, callCount)
	for index := range want {
		want[index] = index + 1
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("response IDs = %v, want %v", ids, want)
	}
}

func TestEOFAndGracefulShutdown(t *testing.T) {
	t.Parallel()

	t.Run("empty EOF", func(t *testing.T) {
		t.Parallel()
		var output strings.Builder
		if err := NewServer(nil, "test-version").Serve(t.Context(), strings.NewReader(""), &output); err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
		if output.Len() != 0 {
			t.Fatalf("output = %q, want empty", output.String())
		}
	})

	t.Run("EOF cancels active call", func(t *testing.T) {
		t.Parallel()
		executor := newBlockingExecutor()
		client := startLiveServer(t, executor)
		client.write(initializeRequest)
		client.read()
		client.write(initializedNotice)
		client.write(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"telemetry_usage","arguments":{}}}`)
		executor.waitStarted(t)
		client.closeInput()
		executor.waitCancelled(t)
		client.wait()
		client.requireNoFrames()
	})
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func exchange(t *testing.T, executor Executor, messages ...string) []rpcResponse {
	t.Helper()
	var output strings.Builder
	input := strings.Join(messages, "\n")
	if len(messages) > 0 {
		input += "\n"
	}
	if err := NewServer(executor, "test-version").Serve(t.Context(), strings.NewReader(input), &output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if output.Len() == 0 {
		return nil
	}
	responses := make([]rpcResponse, 0, len(lines))
	for _, line := range lines {
		var response rpcResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		responses = append(responses, response)
	}
	return responses
}

func decodeResult(t *testing.T, response rpcResponse, target any) {
	t.Helper()
	if response.Error != nil {
		t.Fatalf("response error = %#v", response.Error)
	}
	if err := json.Unmarshal(response.Result, target); err != nil {
		t.Fatalf("decode result: %v; result = %s", err, response.Result)
	}
}

func jsonEqual(left json.RawMessage, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}

type countingExecutor struct {
	calls atomic.Int64
}

func (e *countingExecutor) Execute(context.Context, operatortool.Call) (operatortool.Result, error) {
	e.calls.Add(1)
	return operatortool.Result{Content: json.RawMessage(`{}`)}, nil
}

type staticExecutor struct {
	result operatortool.Result
	err    error
}

func (e *staticExecutor) Execute(context.Context, operatortool.Call) (operatortool.Result, error) {
	return e.result, e.err
}

type blockingExecutor struct {
	started    chan struct{}
	cancelled  chan struct{}
	calls      atomic.Int64
	startOnce  sync.Once
	cancelOnce sync.Once
}

func newBlockingExecutor() *blockingExecutor {
	return &blockingExecutor{started: make(chan struct{}), cancelled: make(chan struct{})}
}

func (e *blockingExecutor) Execute(ctx context.Context, _ operatortool.Call) (operatortool.Result, error) {
	e.calls.Add(1)
	e.startOnce.Do(func() { close(e.started) })
	<-ctx.Done()
	e.cancelOnce.Do(func() { close(e.cancelled) })
	return operatortool.Result{}, ctx.Err()
}

func (e *blockingExecutor) waitStarted(t *testing.T) {
	t.Helper()
	waitChannel(t, e.started, "executor start")
}

func (e *blockingExecutor) waitCancelled(t *testing.T) {
	t.Helper()
	waitChannel(t, e.cancelled, "executor cancellation")
}

type concurrentExecutor struct {
	started chan struct{}
	release chan struct{}
	current atomic.Int64
	maximum atomic.Int64
}

func newConcurrentExecutor(calls int) *concurrentExecutor {
	return &concurrentExecutor{started: make(chan struct{}, calls), release: make(chan struct{})}
}

func (e *concurrentExecutor) Execute(ctx context.Context, _ operatortool.Call) (operatortool.Result, error) {
	current := e.current.Add(1)
	defer e.current.Add(-1)
	for {
		maximum := e.maximum.Load()
		if current <= maximum || e.maximum.CompareAndSwap(maximum, current) {
			break
		}
	}
	e.started <- struct{}{}
	select {
	case <-e.release:
		return operatortool.Result{Content: json.RawMessage(`{"generated_at":"2026-08-08T02:30:00Z"}`)}, nil
	case <-ctx.Done():
		return operatortool.Result{}, ctx.Err()
	}
}

func (e *concurrentExecutor) waitAllStarted(t *testing.T) {
	t.Helper()
	for range cap(e.started) {
		waitChannel(t, e.started, "concurrent executor start")
	}
}

type liveClient struct {
	t      *testing.T
	input  *io.PipeWriter
	frames chan []byte
	done   chan error
}

func startLiveServer(t *testing.T, executor Executor) *liveClient {
	t.Helper()
	reader, writer := io.Pipe()
	frames := make(chan []byte, 64)
	done := make(chan error, 1)
	go func() {
		done <- NewServer(executor, "test-version").Serve(t.Context(), reader, frameWriter{frames: frames})
	}()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})
	return &liveClient{t: t, input: writer, frames: frames, done: done}
}

func (c *liveClient) write(message string) {
	c.t.Helper()
	if _, err := io.WriteString(c.input, message+"\n"); err != nil {
		c.t.Fatalf("write request: %v", err)
	}
}

func (c *liveClient) read() rpcResponse {
	c.t.Helper()
	select {
	case frame := <-c.frames:
		var response rpcResponse
		if err := json.Unmarshal(bytesTrimSpace(frame), &response); err != nil {
			c.t.Fatalf("decode response %q: %v", frame, err)
		}
		return response
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for MCP response")
		return rpcResponse{}
	}
}

func (c *liveClient) closeInput() {
	c.t.Helper()
	if err := c.input.Close(); err != nil {
		c.t.Fatalf("close input: %v", err)
	}
}

func (c *liveClient) wait() {
	c.t.Helper()
	select {
	case err := <-c.done:
		if err != nil {
			c.t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for MCP shutdown")
	}
}

func (c *liveClient) close() {
	c.t.Helper()
	c.closeInput()
	c.wait()
}

func (c *liveClient) requireNoFrames() {
	c.t.Helper()
	select {
	case frame := <-c.frames:
		c.t.Fatalf("unexpected MCP frame: %s", frame)
	default:
	}
}

type frameWriter struct {
	frames chan<- []byte
}

func (w frameWriter) Write(frame []byte) (int, error) {
	w.frames <- append([]byte(nil), frame...)
	return len(frame), nil
}

func waitChannel(t *testing.T, channel <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}
