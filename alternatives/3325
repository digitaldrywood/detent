package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
)

type rpcRecord struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type scanResult struct {
	record rpcRecord
	err    error
}

func scan(ctx context.Context, reader io.Reader, records chan<- scanResult) {
	defer close(records)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, bytes.TrimSuffix(data[:i], []byte{'\r'}), nil
		}
		if eof && len(data) > 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	send := func(item scanResult) bool {
		select {
		case records <- item:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for scanner.Scan() {
		var r rpcRecord
		err := json.Unmarshal(scanner.Bytes(), &r)
		if err == nil && (r.JSONRPC != "2.0" || (r.Method == "" && len(r.ID) == 0)) {
			err = errors.New("invalid ACP envelope")
		}
		if !send(scanResult{r, err}) || err != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		send(scanResult{err: err})
	}
}

type turnState struct {
	req       runner.AgentTurnRequest
	emit      runner.AgentUpdateHandler
	sessionID string
}

func (s *turnState) result() runner.AgentTurnResult {
	return runner.AgentTurnResult{ThreadID: s.sessionID, SessionID: s.sessionID, TurnID: s.sessionID}
}
func (s *turnState) update(u runner.AgentUpdate) error {
	u.ThreadID, u.TurnID, u.ProviderSessionID = s.sessionID, s.sessionID, s.sessionID
	return s.emit(u)
}

func (b *AgentBackend) exchange(ctx context.Context, input io.Writer, records <-chan scanResult, s *turnState) error {
	id := 0
	call := func(method string, params any) (json.RawMessage, error) {
		id++
		if err := encode(input, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			if method != "session/prompt" {
				return nil, &infrastructureError{err: errors.Join(ctx.Err(), err), startup: true}
			}
			return nil, err
		}
		var timer *time.Timer
		var stalled <-chan time.Time
		if b.options.StallTimeout > 0 {
			timer = time.NewTimer(b.options.StallTimeout)
			stalled = timer.C
			defer timer.Stop()
		}
		for {
			select {
			case <-ctx.Done():
				if method != "session/prompt" && errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil, &infrastructureError{err: ctx.Err(), startup: true}
				}
				return nil, ctx.Err()
			case <-stalled:
				return nil, &infrastructureError{err: errors.New("OpenCode ACP stream stalled"), startup: method != "session/prompt"}
			case item, ok := <-records:
				if !ok {
					return nil, &infrastructureError{err: io.ErrUnexpectedEOF, startup: method != "session/prompt"}
				}
				if item.err != nil {
					return nil, &infrastructureError{err: item.err, startup: method != "session/prompt"}
				}
				if timer != nil {
					timer.Reset(b.options.StallTimeout)
				}
				r := item.record
				if r.Method != "" {
					if len(r.ID) > 0 {
						if err := s.respond(input, r); err != nil {
							return nil, err
						}
					} else if r.Method == "session/update" {
						if err := s.event(r.Params); err != nil {
							return nil, err
						}
					}
					continue
				}
				if string(r.ID) != strconv.Itoa(id) {
					return nil, &infrastructureError{err: errors.New("unexpected ACP response ID")}
				}
				if r.Error != nil {
					return nil, providerRequestError(method, r.Error.Code, r.Error.Message)
				}
				if len(r.Result) == 0 {
					return nil, &infrastructureError{err: errors.New("ACP response omitted result")}
				}
				return r.Result, nil
			}
		}
	}
	raw, err := call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "detent", "version": "1"}})
	if err != nil {
		return err
	}
	var init struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			MCPCapabilities struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err = json.Unmarshal(raw, &init); err != nil || init.ProtocolVersion != 1 {
		return &infrastructureError{err: errors.New("OpenCode requires ACP version 1"), startup: true}
	}
	if len(b.toolServers) > 0 && !init.AgentCapabilities.MCPCapabilities.HTTP {
		return &infrastructureError{err: errors.New("OpenCode does not advertise HTTP MCP"), startup: true}
	}
	servers := b.toolServers
	if servers == nil {
		servers = []map[string]any{}
	}
	raw, err = call("session/new", map[string]any{"cwd": s.req.Workspace, "mcpServers": servers, "additionalDirectories": s.req.ExtraWritableRoots})
	if err != nil {
		return err
	}
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err = json.Unmarshal(raw, &session); err != nil || session.SessionID == "" {
		return &infrastructureError{err: errors.New("OpenCode omitted session identity"), startup: true}
	}
	s.sessionID = session.SessionID
	raw, err = call("session/set_config_option", map[string]string{"sessionId": s.sessionID, "configId": "model", "value": s.req.Model})
	if err != nil {
		return err
	}
	if !configSelected(raw, "model", s.req.Model) {
		return &infrastructureError{err: errors.New("OpenCode resolved a different model"), startup: true}
	}
	if s.req.ReasoningEffort != "" {
		raw, err = call("session/set_config_option", map[string]string{"sessionId": s.sessionID, "configId": "effort", "value": s.req.ReasoningEffort})
		if err != nil {
			return err
		}
		if !configSelected(raw, "effort", s.req.ReasoningEffort) {
			return &infrastructureError{err: errors.New("OpenCode resolved a different effort"), startup: true}
		}
	}
	provider, _, _ := strings.Cut(s.req.Model, "/")
	if err = s.update(runner.AgentUpdate{Type: runner.AgentUpdateRuntimeIdentity, Model: s.req.Model, RuntimeIdentity: agentidentity.RuntimeUpdate(s.req.Model, provider, s.req.ReasoningEffort, "", time.Time{})}); err != nil {
		return err
	}
	if err = s.update(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted}); err != nil {
		return err
	}
	prompt := s.req.Prompt
	if s.req.ToolInstructions != "" {
		prompt += "\n\n" + s.req.ToolInstructions
	}
	content := []map[string]string{{"type": "text", "text": prompt}}
	for _, image := range runner.AttachmentImages(s.req.Attachments) {
		content = append(content, map[string]string{"type": "image", "data": base64.StdEncoding.EncodeToString(image.Content), "mimeType": image.MIME})
	}
	raw, err = call("session/prompt", map[string]any{"sessionId": s.sessionID, "prompt": content})
	if err != nil {
		return err
	}
	var end struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens       int64 `json:"inputTokens"`
			OutputTokens      int64 `json:"outputTokens"`
			TotalTokens       int64 `json:"totalTokens"`
			CachedReadTokens  int64 `json:"cachedReadTokens"`
			CachedWriteTokens int64 `json:"cachedWriteTokens"`
			ThoughtTokens     int64 `json:"thoughtTokens"`
		} `json:"usage"`
	}
	if err = json.Unmarshal(raw, &end); err != nil {
		return &infrastructureError{err: err}
	}
	if end.Usage != nil {
		u := end.Usage
		if err = s.update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: u.InputTokens + u.CachedReadTokens + u.CachedWriteTokens, CachedInputTokens: u.CachedReadTokens, OutputTokens: u.OutputTokens + u.ThoughtTokens, ReasoningOutputTokens: u.ThoughtTokens, TotalTokens: u.TotalTokens}}); err != nil {
			return err
		}
	}
	switch end.StopReason {
	case "end_turn":
		return nil
	case "cancelled":
		return context.Canceled
	case "max_turn_requests":
		return runner.ErrSessionTurnLimitExceeded
	default:
		return errors.New("OpenCode stopped without completion")
	}
}

func configSelected(raw json.RawMessage, id, value string) bool {
	var response struct {
		ConfigOptions []struct {
			ID           string `json:"id"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return false
	}
	for _, option := range response.ConfigOptions {
		if option.ID == id {
			return option.CurrentValue == value
		}
	}
	return false
}

func (s *turnState) respond(input io.Writer, r rpcRecord) error {
	if r.Method != "session/request_permission" {
		return encode(input, map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32601, "message": "Client method unavailable"}})
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Options   []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return &infrastructureError{err: err}
	}
	outcome := map[string]string{"outcome": "cancelled"}
	if s.sessionID != "" && p.SessionID == s.sessionID && !s.req.ReadOnly && !s.req.TextOnly {
		for _, option := range p.Options {
			if option.Kind == "allow_once" {
				outcome = map[string]string{"outcome": "selected", "optionId": option.OptionID}
				break
			}
		}
	}
	return encode(input, map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": map[string]any{"outcome": outcome}})
}

func (s *turnState) event(raw json.RawMessage) error {
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return &infrastructureError{err: err}
	}
	if s.sessionID == "" {
		return nil
	}
	if p.SessionID != s.sessionID {
		return &infrastructureError{err: errors.New("ACP update belongs to another session")}
	}
	u := p.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		var text struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(u.Content, &text); err != nil {
			return &infrastructureError{err: err}
		}
		if text.Type == "text" {
			return s.update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: text.Text})
		}
	case "tool_call", "tool_call_update":
		typ := runner.AgentUpdateToolStarted
		if u.SessionUpdate == "tool_call_update" {
			typ = runner.AgentUpdateToolOutput
		}
		if u.Status == "completed" || u.Status == "failed" {
			typ = runner.AgentUpdateToolCompleted
		}
		return s.update(runner.AgentUpdate{Type: typ, ItemID: u.ToolCallID, Tool: u.Kind, Command: u.Title, Status: u.Status})
	}
	return nil
}
