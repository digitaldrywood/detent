package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
)

type streamRecord struct {
	Event          string      `json:"event"`
	ConversationID string      `json:"conversation_id"`
	StepUpdate     *stepUpdate `json:"step_update"`
	Result         *result     `json:"result"`
}

type stepUpdate struct {
	ConversationID string `json:"conversation_id"`
	StepIndex      int    `json:"step_index"`
	State          string `json:"state"`
	StepType       string `json:"step_type"`
	ToolName       string `json:"tool_name"`
	TextDelta      string `json:"text_delta"`
	ToolInfo       struct {
		Parameters map[string]any `json:"parameters"`
		Output     string         `json:"output"`
	} `json:"tool_info"`
	Usage *usage `json:"usage"`
}

type result struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Error          string `json:"error"`
}

type usage struct {
	Input     int64 `json:"input_tokens"`
	Output    int64 `json:"output_tokens"`
	Thinking  int64 `json:"thinking_tokens"`
	CacheRead int64 `json:"cache_read_tokens"`
}

type turnState struct {
	req           runner.AgentTurnRequest
	emit          runner.AgentUpdateHandler
	provider      string
	effort        string
	sessionID     string
	turns         int
	tokens        runner.AgentTokenCounts
	toolsStarted  map[int]bool
	usageCounted  map[int]bool
	promptWritten bool
}

func (s *turnState) result() runner.AgentTurnResult {
	return runner.AgentTurnResult{ThreadID: s.sessionID, TurnID: s.sessionID, SessionID: s.sessionID}
}

func (s *turnState) update(u runner.AgentUpdate) error {
	u.ThreadID, u.TurnID, u.ProviderSessionID = s.sessionID, s.sessionID, s.sessionID
	return s.emit(u)
}

type scanResult struct {
	record streamRecord
	err    error
}

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
		var record streamRecord
		err := json.Unmarshal(scanner.Bytes(), &record)
		if err == nil && record.Event == "" {
			err = errors.New("stream record has no event")
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

func (b *AgentBackend) exchange(ctx context.Context, records <-chan scanResult, writeDone <-chan error, s *turnState) error {
	started := false
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
			if !started {
				return &infrastructureError{err: ctx.Err(), startup: true}
			}
			return ctx.Err()
		case err := <-writeDone:
			s.promptWritten = true
			writeDone = nil
			if err != nil {
				return err
			}
		case <-stall:
			return &infrastructureError{err: errors.New("antigravity stream stalled"), startup: !started}
		case item, ok := <-records:
			if !ok {
				return &infrastructureError{err: fmt.Errorf("antigravity exited before a result: %w", io.EOF), startup: !started}
			}
			if timer != nil {
				timer.Reset(b.options.StallTimeout)
			}
			if item.err != nil {
				return &infrastructureError{err: fmt.Errorf("invalid Antigravity stream record: %w", item.err), startup: !started}
			}
			r := item.record
			switch r.Event {
			case "init":
				if started {
					return &infrastructureError{err: errors.New("antigravity stream repeated init")}
				}
				if r.ConversationID == "" {
					return &infrastructureError{err: errors.New("antigravity init omitted conversation identity"), startup: true}
				}
				started = true
				if err := s.start(r.ConversationID); err != nil {
					return err
				}
			case "step_update":
				if !started || r.StepUpdate == nil {
					return &infrastructureError{err: errors.New("antigravity step update outside a conversation"), startup: !started}
				}
				if err := s.step(*r.StepUpdate); err != nil {
					return err
				}
			case "result":
				if r.Result == nil {
					return &infrastructureError{err: errors.New("antigravity result omitted its payload"), startup: !started}
				}
				if r.Result.Status != "SUCCESS" {
					return &infrastructureError{err: fmt.Errorf("antigravity turn %s: %s", r.Result.Status, r.Result.Error), startup: !started}
				}
				if !started {
					return &infrastructureError{err: errors.New("antigravity succeeded without a conversation"), startup: true}
				}
				if r.Result.ConversationID != "" && r.Result.ConversationID != s.sessionID {
					return &infrastructureError{err: errors.New("antigravity result belongs to a different conversation")}
				}
				return nil
			}
		}
	}
}

func (s *turnState) start(conversationID string) error {
	s.sessionID = conversationID
	identity := agentidentity.RuntimeUpdate(s.req.Model, s.provider, s.effort, "", time.Time{})
	if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateRuntimeIdentity, Model: s.req.Model, RuntimeIdentity: identity}); err != nil {
		return err
	}
	return s.update(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted})
}

func (s *turnState) step(u stepUpdate) error {
	if u.ConversationID != "" && u.ConversationID != s.sessionID {
		return &infrastructureError{err: errors.New("antigravity step belongs to a different conversation")}
	}
	itemID := strconv.Itoa(u.StepIndex)
	switch u.StepType {
	case "agent_response":
		if u.TextDelta != "" {
			if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ItemID: itemID, Delta: u.TextDelta}); err != nil {
				return err
			}
		}
	case "tool":
		if s.toolsStarted == nil {
			s.toolsStarted = make(map[int]bool)
		}
		if !s.toolsStarted[u.StepIndex] {
			s.toolsStarted[u.StepIndex] = true
			command, ok := u.ToolInfo.Parameters["CommandLine"].(string)
			if !ok {
				command = ""
			}
			if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateToolStarted, ItemID: itemID, Tool: u.ToolName, Command: command}); err != nil {
				return err
			}
		}
		if u.State != "ACTIVE" {
			if u.ToolInfo.Output != "" {
				if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateToolOutput, ItemID: itemID, Tool: u.ToolName, Delta: u.ToolInfo.Output}); err != nil {
					return err
				}
			}
			status := runner.FinalStateCompleted
			if u.State != "DONE" {
				status = runner.FinalStateFailed
			}
			if err := s.update(runner.AgentUpdate{Type: runner.AgentUpdateToolCompleted, ItemID: itemID, Tool: u.ToolName, Status: status}); err != nil {
				return err
			}
		}
	}
	if u.Usage == nil || u.State == "ACTIVE" || s.usageCounted[u.StepIndex] {
		return nil
	}
	if s.usageCounted == nil {
		s.usageCounted = make(map[int]bool)
	}
	s.usageCounted[u.StepIndex] = true
	s.turns++
	if err := s.reportUsage(*u.Usage); err != nil {
		return err
	}
	if s.req.MaxTurns > 0 && s.turns > s.req.MaxTurns {
		return runner.ErrSessionTurnLimitExceeded
	}
	return nil
}

func (s *turnState) reportUsage(u usage) error {
	last := runner.AgentTokenCounts{
		InputTokens:           u.Input + u.CacheRead,
		CachedInputTokens:     u.CacheRead,
		OutputTokens:          u.Output + u.Thinking,
		ReasoningOutputTokens: u.Thinking,
		TotalTokens:           u.Input + u.CacheRead + u.Output + u.Thinking,
	}
	s.tokens = runner.AgentTokenCounts{
		InputTokens:           s.tokens.InputTokens + last.InputTokens,
		CachedInputTokens:     s.tokens.CachedInputTokens + last.CachedInputTokens,
		OutputTokens:          s.tokens.OutputTokens + last.OutputTokens,
		ReasoningOutputTokens: s.tokens.ReasoningOutputTokens + last.ReasoningOutputTokens,
		TotalTokens:           s.tokens.TotalTokens + last.TotalTokens,
	}
	total := s.tokens
	return s.update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: total.InputTokens, CachedInputTokens: total.CachedInputTokens, OutputTokens: total.OutputTokens, ReasoningOutputTokens: total.ReasoningOutputTokens, TotalTokens: total.TotalTokens, ThreadTotal: &total, Last: &last}})
}
