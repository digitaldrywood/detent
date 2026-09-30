package piagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
)

type rpcRecord struct {
	Type           string          `json:"type"`
	ID             string          `json:"id"`
	Command        string          `json:"command"`
	Success        *bool           `json:"success"`
	Error          string          `json:"error"`
	Data           json.RawMessage `json:"data"`
	Message        *message        `json:"message"`
	Usage          *usage          `json:"usage"`
	AssistantEvent struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Args       struct {
		Command string `json:"command"`
	} `json:"args"`
	PartialResult toolResult `json:"partialResult"`
	Result        toolResult `json:"result"`
	IsError       bool       `json:"isError"`
}
type message struct {
	Role          string          `json:"role"`
	Model         string          `json:"model"`
	ResponseModel string          `json:"responseModel"`
	Provider      string          `json:"provider"`
	ThinkingLevel string          `json:"providerThinkingLevel"`
	StopReason    string          `json:"stopReason"`
	ErrorMessage  string          `json:"errorMessage"`
	Content       json.RawMessage `json:"content"`
	Usage         usage           `json:"usage"`
}
type usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"totalTokens"`
}
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (r toolResult) text() string {
	var out strings.Builder
	for _, block := range r.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

type turnState struct {
	req        runner.AgentTurnRequest
	emit       runner.AgentUpdateHandler
	sessionID  string
	identity   agentidentity.Identity
	turns      int
	messageID  int
	streamed   bool
	lastError  error
	tokens     runner.AgentTokenCounts
	toolOutput map[string]string
}

func (s *turnState) result() runner.AgentTurnResult {
	return runner.AgentTurnResult{ThreadID: s.sessionID, TurnID: s.sessionID, SessionID: s.sessionID}
}
func (s *turnState) update(u runner.AgentUpdate) error {
	u.ThreadID, u.TurnID, u.ProviderSessionID = s.sessionID, s.sessionID, s.sessionID
	return s.emit(u)
}

type scanResult struct {
	record rpcRecord
	err    error
}

// LF framing rejects an unterminated final record and preserves U+2028/U+2029.
func splitRecord(data []byte, atEOF bool) (int, []byte, error) {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return index + 1, bytes.TrimSuffix(data[:index], []byte{'\r'}), nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return 0, nil, nil
}
func scan(ctx context.Context, reader io.Reader, records chan<- scanResult) {
	defer close(records)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	scanner.Split(splitRecord)
	for scanner.Scan() {
		var record rpcRecord
		err := json.Unmarshal(scanner.Bytes(), &record)
		if err == nil && record.Type == "" {
			err = errors.New("RPC record has no type")
		}
		select {
		case records <- scanResult{record: record, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case records <- scanResult{err: err}:
		case <-ctx.Done():
		}
	}
}

func (b *AgentBackend) exchange(ctx context.Context, input io.Writer, records <-chan scanResult, s *turnState) error {
	if err := encode(input, map[string]any{"id": "state", "type": "get_state"}); err != nil {
		return err
	}
	ready, accepted, settled := false, false, false
	var timer *time.Timer
	var stall <-chan time.Time
	if b.options.StallTimeout > 0 {
		timer = time.NewTimer(b.options.StallTimeout)
		stall = timer.C
		defer timer.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			if !ready {
				return &infrastructureError{err: ctx.Err(), startup: true}
			}
			return ctx.Err()
		case <-stall:
			return &infrastructureError{err: errors.New("Pi RPC stream stalled"), startup: !ready}
		case item, ok := <-records:
			if !ok {
				return &infrastructureError{err: fmt.Errorf("Pi RPC exited before completion: %w", io.EOF), startup: !ready}
			}
			if timer != nil {
				timer.Reset(b.options.StallTimeout)
			}
			if item.err != nil {
				return &infrastructureError{err: fmt.Errorf("invalid Pi RPC record: %w", item.err), startup: !ready}
			}
			r := item.record
			if r.Type == "response" {
				expectedID, expectedCommand := "state", "get_state"
				if ready {
					expectedID, expectedCommand = "prompt", "prompt"
				}
				if r.ID != expectedID || r.Command != expectedCommand || r.Success == nil || accepted {
					return &infrastructureError{err: fmt.Errorf("unexpected Pi RPC response %q for %q", r.ID, r.Command), startup: !ready}
				}
				if !*r.Success {
					err := fmt.Errorf("Pi %s rejected: %s", r.Command, r.Error)
					if !ready {
						return &infrastructureError{err: err, startup: true}
					}
					return err
				}
				if !ready {
					if err := s.setState(r.Data); err != nil {
						return &infrastructureError{err: err, startup: true}
					}
					ready = true
					prompt := map[string]any{"id": "prompt", "type": "prompt", "message": s.req.Prompt}
					if s.req.ToolInstructions != "" {
						prompt["message"] = s.req.Prompt + "\n\n" + s.req.ToolInstructions
					}
					images := runner.AttachmentImages(s.req.Attachments)
					if len(images) > 0 {
						var content []map[string]string
						for _, image := range images {
							content = append(content, map[string]string{"type": "image", "data": base64.StdEncoding.EncodeToString(image.Content), "mimeType": image.MIME})
						}
						prompt["images"] = content
					}
					if err := encode(input, prompt); err != nil {
						return err
					}
				} else {
					var data struct {
						Disposition string `json:"disposition"`
					}
					if err := json.Unmarshal(r.Data, &data); err != nil {
						return &infrastructureError{err: fmt.Errorf("invalid Pi prompt response: %w", err)}
					}
					if data.Disposition != "started" {
						return &infrastructureError{err: fmt.Errorf("Pi prompt did not start a run: %q", data.Disposition)}
					}
					accepted = true
				}
			} else if r.Type == "agent_settled" {
				if !ready {
					return &infrastructureError{err: errors.New("Pi settled before prompt dispatch"), startup: true}
				}
				settled = true
			} else if ready {
				if err := s.event(r); err != nil {
					return err
				}
			}
			if settled && accepted {
				return s.lastError
			}
		}
	}
}

func (s *turnState) setState(data json.RawMessage) error {
	var state struct {
		SessionID string `json:"sessionId"`
		Model     struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"model"`
		ThinkingLevel string `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("invalid Pi state: %w", err)
	}
	if state.SessionID == "" || state.Model.ID == "" {
		return errors.New("Pi state omitted session or model identity")
	}
	// Do not silently execute with a different provider/model than the route.
	if s.req.Model != "" && state.Model.ID != s.req.Model {
		return errors.New("Pi resolved model differs from requested route model")
	}
	if s.req.ModelProvider != "" && state.Model.Provider != s.req.ModelProvider {
		return errors.New("Pi resolved provider differs from requested provider")
	}
	s.sessionID = state.SessionID
	s.identity = agentidentity.RuntimeUpdate(state.Model.ID, state.Model.Provider, state.ThinkingLevel, "", time.Time{})
	return s.update(runner.AgentUpdate{Type: runner.AgentUpdateRuntimeIdentity, Model: state.Model.ID, RuntimeIdentity: s.identity})
}

func (s *turnState) event(r rpcRecord) error {
	switch r.Type {
	case "agent_start":
		return s.update(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted})
	case "turn_start":
		s.turns++
		if s.req.MaxTurns > 0 && s.turns > s.req.MaxTurns {
			return runner.ErrSessionTurnLimitExceeded
		}
	case "message_start":
		if r.Message != nil && r.Message.Role == "assistant" {
			s.messageID++
			s.streamed = false
		}
	case "message_update":
		if r.AssistantEvent.Type == "text_delta" {
			s.streamed = true
			if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ItemID: strconv.Itoa(s.messageID), Delta: r.AssistantEvent.Delta}); err != nil {
				return err
			}
		}
		if r.Usage != nil {
			return s.reportUsage(*r.Usage, false)
		}
	case "message_end":
		if r.Message == nil || r.Message.Role != "assistant" {
			return nil
		}
		m := r.Message
		s.lastError = nil
		if m.StopReason == "error" || m.StopReason == "aborted" {
			s.lastError = fmt.Errorf("Pi assistant %s: %s", m.StopReason, m.ErrorMessage)
		}
		model := m.Model
		if m.ResponseModel != "" {
			model = m.ResponseModel
		}
		s.identity = s.identity.Merge(agentidentity.RuntimeUpdate(model, m.Provider, m.ThinkingLevel, "", time.Time{}))
		if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateModelUpdated, Model: model, RuntimeIdentity: s.identity}); err != nil {
			return err
		}
		if !s.streamed {
			var content toolResult
			if err := json.Unmarshal(m.Content, &content.Content); err != nil {
				return &infrastructureError{err: fmt.Errorf("invalid Pi assistant content: %w", err)}
			}
			if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ItemID: strconv.Itoa(s.messageID), Delta: content.text()}); err != nil {
				return err
			}
		}
		return s.reportUsage(m.Usage, true)
	case "tool_execution_start":
		return s.update(runner.AgentUpdate{Type: runner.AgentUpdateToolStarted, ItemID: r.ToolCallID, Tool: r.ToolName, Command: r.Args.Command})
	case "tool_execution_update", "tool_execution_end":
		output := r.PartialResult.text()
		if r.Type == "tool_execution_end" {
			output = r.Result.text()
		}
		if s.toolOutput == nil {
			s.toolOutput = make(map[string]string)
		}
		previous := s.toolOutput[r.ToolCallID]
		s.toolOutput[r.ToolCallID] = output
		delta := strings.TrimPrefix(output, previous)
		if delta != "" {
			if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateToolOutput, ItemID: r.ToolCallID, Tool: r.ToolName, Delta: delta}); err != nil {
				return err
			}
		}
		if r.Type == "tool_execution_end" {
			status := runner.FinalStateCompleted
			if r.IsError {
				status = runner.FinalStateFailed
			}
			delete(s.toolOutput, r.ToolCallID)
			return s.update(runner.AgentUpdate{Type: runner.AgentUpdateToolCompleted, ItemID: r.ToolCallID, Tool: r.ToolName, Status: status})
		}
	case "extension_ui_request":
		return &infrastructureError{err: errors.New("Pi extension UI is unsupported; extensions must remain disabled")}
	}
	return nil
}

func (s *turnState) reportUsage(u usage, final bool) error {
	// Pi input excludes cache reads/writes. Reasoning is already in output.
	last := runner.AgentTokenCounts{InputTokens: u.Input + u.CacheRead + u.CacheWrite, CachedInputTokens: u.CacheRead, OutputTokens: u.Output, ReasoningOutputTokens: u.Reasoning, TotalTokens: u.Total}
	total := runner.AgentTokenCounts{InputTokens: s.tokens.InputTokens + last.InputTokens, CachedInputTokens: s.tokens.CachedInputTokens + last.CachedInputTokens, OutputTokens: s.tokens.OutputTokens + last.OutputTokens, ReasoningOutputTokens: s.tokens.ReasoningOutputTokens + last.ReasoningOutputTokens, TotalTokens: s.tokens.TotalTokens + last.TotalTokens}
	// Stream usage is a snapshot of the current response. Commit it only on
	// message_end so repeated snapshots and turn_end never inflate totals.
	if final {
		s.tokens = total
	}
	return s.update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: total.InputTokens, CachedInputTokens: total.CachedInputTokens, OutputTokens: total.OutputTokens, ReasoningOutputTokens: total.ReasoningOutputTokens, TotalTokens: total.TotalTokens, ThreadTotal: &total, Last: &last}})
}
