package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

const ProtocolVersion = "2025-11-25"

const (
	maxFrameBytes      = operatortool.MaxArgumentBytes + 64*1024
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeNotInitialized = -32002
	serverName         = "detent"
	serverTitle        = "Detent"
)

type Executor interface {
	Execute(context.Context, operatortool.Call) (operatortool.Result, error)
}

type Server struct {
	executor Executor
	version  string
}

func NewServer(executor Executor, version string) *Server {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	return &Server{executor: executor, version: version}
}

func (s *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if input == nil {
		return errors.New("mcp stdin is not configured")
	}
	if output == nil {
		return errors.New("mcp stdout is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	session := &session{
		done:     runContext.Done(),
		cancel:   cancel,
		executor: s.executor,
		version:  s.version,
		output:   output,
		state:    stateNew,
		active:   make(map[string]*activeRequest),
	}

	readerDone := make(chan struct{})
	var closeWatcher sync.WaitGroup
	if closer, ok := input.(io.ReadCloser); ok {
		closeWatcher.Add(1)
		go func() {
			defer closeWatcher.Done()
			select {
			case <-runContext.Done():
				_ = closer.Close()
			case <-readerDone:
			}
		}()
	}

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4*1024), maxFrameBytes)
	for scanner.Scan() {
		if err := session.handle(runContext, scanner.Bytes()); err != nil {
			cancel()
			break
		}
		if session.writeFailure() != nil {
			break
		}
	}
	close(readerDone)
	closeWatcher.Wait()

	scanErr := scanner.Err()
	session.cancelActive()
	session.calls.Wait()
	if err := session.writeFailure(); err != nil {
		return err
	}
	if scanErr != nil && ctx.Err() == nil {
		return fmt.Errorf("read MCP stdio: %w", scanErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

type lifecycleState uint8

const (
	stateNew lifecycleState = iota
	stateInitialized
	stateReady
)

type session struct {
	done     <-chan struct{}
	cancel   context.CancelFunc
	executor Executor
	version  string
	output   io.Writer

	mu              sync.Mutex
	state           lifecycleState
	protocolVersion string
	active          map[string]*activeRequest
	calls           sync.WaitGroup
	writeMu         sync.Mutex
	writeErr        error
}

type activeRequest struct {
	cancel     context.CancelFunc
	suppressed bool
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *session) handle(ctx context.Context, line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if !json.Valid(trimmed) {
		return s.writeError(nil, codeParseError, "Parse error", nil)
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return s.writeError(nil, codeInvalidRequest, "Invalid Request", nil)
	}

	var message request
	if err := json.Unmarshal(trimmed, &message); err != nil {
		return s.writeError(nil, codeInvalidRequest, "Invalid Request", nil)
	}
	isNotification := len(message.ID) == 0
	if message.JSONRPC != "2.0" || strings.TrimSpace(message.Method) == "" {
		if isNotification {
			return nil
		}
		return s.writeError(nil, codeInvalidRequest, "Invalid Request", nil)
	}
	if isNotification {
		s.handleNotification(message)
		return nil
	}

	key, ok := requestIDKey(message.ID)
	if !ok {
		return s.writeError(nil, codeInvalidRequest, "Invalid Request", nil)
	}
	switch message.Method {
	case "initialize":
		return s.initialize(message)
	case "ping":
		return s.writeResult(message.ID, map[string]any{})
	case "tools/list":
		if !s.ready() {
			return s.writeError(message.ID, codeNotInitialized, "Server not initialized", nil)
		}
		return s.listTools(message)
	case "tools/call":
		if !s.ready() {
			return s.writeError(message.ID, codeNotInitialized, "Server not initialized", nil)
		}
		return s.startToolCall(ctx, key, message)
	default:
		return s.writeError(message.ID, codeMethodNotFound, "Method not found", nil)
	}
}

func (s *session) initialize(message request) error {
	var params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if err := decodeObject(message.Params, &params, false); err != nil || strings.TrimSpace(params.ProtocolVersion) == "" || !isJSONObject(params.Capabilities) || strings.TrimSpace(params.ClientInfo.Name) == "" || strings.TrimSpace(params.ClientInfo.Version) == "" {
		return s.writeError(message.ID, codeInvalidParams, "Invalid initialize parameters", nil)
	}

	negotiated := negotiateVersion(params.ProtocolVersion)
	s.mu.Lock()
	if s.state != stateNew {
		s.mu.Unlock()
		return s.writeError(message.ID, codeInvalidRequest, "Server is already initialized", nil)
	}
	s.state = stateInitialized
	s.protocolVersion = negotiated
	s.mu.Unlock()

	return s.writeResult(message.ID, map[string]any{
		"protocolVersion": negotiated,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"title":   serverTitle,
			"version": s.version,
		},
		"instructions": "Read-only access to the Detent operator catalog through the running daemon.",
	})
}

func negotiateVersion(requested string) string {
	switch requested {
	case ProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05":
		return requested
	default:
		return ProtocolVersion
	}
}

func (s *session) handleNotification(message request) {
	switch message.Method {
	case "notifications/initialized":
		s.mu.Lock()
		if s.state == stateInitialized {
			s.state = stateReady
		}
		s.mu.Unlock()
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason,omitempty"`
			Meta      json.RawMessage `json:"_meta,omitempty"`
		}
		if err := decodeObject(message.Params, &params, true); err != nil {
			return
		}
		key, ok := requestIDKey(params.RequestID)
		if !ok {
			return
		}
		s.cancelRequest(key)
	}
}

func (s *session) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == stateReady
}

type listedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (s *session) listTools(message request) error {
	var params struct {
		Cursor string          `json:"cursor,omitempty"`
		Meta   json.RawMessage `json:"_meta,omitempty"`
	}
	if err := decodeObject(message.Params, &params, true); err != nil || strings.TrimSpace(params.Cursor) != "" {
		return s.writeError(message.ID, codeInvalidParams, "Invalid tools/list parameters", nil)
	}
	definitions := operatortool.Catalog()
	tools := make([]listedTool, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, listedTool{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: definition.InputSchema,
		})
	}
	return s.writeResult(message.ID, map[string]any{"tools": tools})
}

func (s *session) startToolCall(parent context.Context, key string, message request) error {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Meta      json.RawMessage `json:"_meta,omitempty"`
	}
	if err := decodeObject(message.Params, &params, true); err != nil {
		return s.writeError(message.ID, codeInvalidParams, "Invalid tools/call parameters", nil)
	}
	params.Name = strings.TrimSpace(params.Name)
	if _, ok := operatortool.Lookup(params.Name); !ok {
		return s.writeError(message.ID, codeInvalidParams, "Unknown tool: "+params.Name, nil)
	}
	if len(params.Arguments) == 0 {
		params.Arguments = json.RawMessage(`{}`)
	}
	if !isJSONObject(params.Arguments) || len(params.Arguments) > operatortool.MaxArgumentBytes {
		return s.writeError(message.ID, codeInvalidParams, "Tool arguments must be a JSON object within the size limit", nil)
	}

	callContext, cancel := context.WithCancel(parent)
	active := &activeRequest{cancel: cancel}
	s.mu.Lock()
	if existing := s.active[key]; existing != nil {
		existing.suppressed = true
		existing.cancel()
		s.mu.Unlock()
		cancel()
		return s.writeError(message.ID, codeInvalidRequest, "Duplicate request ID", nil)
	}
	s.active[key] = active
	s.calls.Add(1)
	protocolVersion := s.protocolVersion
	s.mu.Unlock()

	call := operatortool.Call{Name: params.Name, Arguments: params.Arguments}
	go func() {
		defer s.calls.Done()
		defer cancel()
		result, err := s.execute(callContext, call)
		if !s.completeRequest(key, active) {
			return
		}
		if err != nil {
			if writeErr := s.writeResult(message.ID, toolCallResult{
				Content: []textContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			}); writeErr != nil {
				return
			}
			return
		}
		if writeErr := s.writeResult(message.ID, successfulToolCallResult(protocolVersion, result.Content)); writeErr != nil {
			return
		}
	}()
	return nil
}

func (s *session) execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if s.executor == nil {
		return operatortool.Result{}, errors.New("detent daemon bridge is unavailable")
	}
	result, err := s.executor.Execute(ctx, call)
	if err != nil {
		return operatortool.Result{}, err
	}
	if len(result.Content) > operatortool.MaxResultBytes {
		return operatortool.Result{}, fmt.Errorf("operator tool result exceeds %d bytes", operatortool.MaxResultBytes)
	}
	if !isJSONObject(result.Content) {
		return operatortool.Result{}, errors.New("operator tool result is not a JSON object")
	}
	return result, nil
}

type toolCallResult struct {
	Content           []textContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func successfulToolCallResult(protocolVersion string, content json.RawMessage) toolCallResult {
	if protocolVersion == "2024-11-05" || protocolVersion == "2025-03-26" {
		return toolCallResult{Content: []textContent{{Type: "text", Text: string(content)}}}
	}
	return toolCallResult{Content: []textContent{}, StructuredContent: content}
}

func (s *session) completeRequest(key string, active *activeRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[key] != active {
		return false
	}
	delete(s.active, key)
	if active.suppressed {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *session) cancelRequest(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active := s.active[key]; active != nil {
		active.suppressed = true
		active.cancel()
	}
}

func (s *session) cancelActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, active := range s.active {
		active.suppressed = true
		active.cancel()
	}
}

func (s *session) writeResult(id json.RawMessage, result any) error {
	return s.write(response{JSONRPC: "2.0", ID: responseID(id), Result: result})
}

func (s *session) writeError(id json.RawMessage, code int, message string, data any) error {
	return s.write(response{
		JSONRPC: "2.0",
		ID:      responseID(id),
		Error:   &rpcError{Code: code, Message: message, Data: data},
	})
}

func responseID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage(`null`)
	}
	return id
}

func (s *session) write(message response) error {
	frame, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode MCP response: %w", err)
	}
	frame = append(frame, '\n')

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	if _, err := s.output.Write(frame); err != nil {
		s.writeErr = fmt.Errorf("write MCP stdout: %w", err)
		s.cancel()
		return s.writeErr
	}
	return nil
}

func (s *session) writeFailure() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeErr
}

func decodeObject(raw json.RawMessage, target any, strict bool) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if !isJSONObject(raw) {
		return errors.New("parameters must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func requestIDKey(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	switch value := value.(type) {
	case string:
		return "string:" + value, true
	case json.Number:
		return "number:" + value.String(), true
	default:
		return "", false
	}
}
