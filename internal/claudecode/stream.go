package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
)

type streamItem struct {
	event claudeEvent
	err   error
}

type claudeEvent struct {
	Type         string         `json:"type"`
	Subtype      string         `json:"subtype"`
	SessionID    string         `json:"session_id"`
	Model        string         `json:"model"`
	Message      *claudeMessage `json:"message"`
	Usage        *claudeUsage   `json:"usage"`
	StreamEvent  *streamEvent   `json:"event"`
	InputTokens  int64          `json:"input_tokens"`
	OutputTokens int64          `json:"output_tokens"`
	TotalTokens  int64          `json:"total_tokens"`
	IsError      bool           `json:"is_error"`
	DurationMS   int64          `json:"duration_ms"`
	TotalCostUSD float64        `json:"total_cost_usd"`
}

type claudeMessage struct {
	ID      string         `json:"id"`
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []contentBlock `json:"content"`
	Usage   *claudeUsage   `json:"usage"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type streamEvent struct {
	Type         string         `json:"type"`
	Message      *claudeMessage `json:"message"`
	ContentBlock *contentBlock  `json:"content_block"`
	Delta        *streamDelta   `json:"delta"`
	Usage        *claudeUsage   `json:"usage"`
}

type streamDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CachedInputTokens        int64 `json:"cached_input_tokens"`
	ReasoningOutputTokens    int64 `json:"reasoning_output_tokens"`
}

type turnState struct {
	sessionID       string
	model           string
	partialItemID   string
	usage           runner.AgentTokenUsage
	sawResult       bool
	resultSubtype   string
	resultIsError   bool
	turnStartedSent bool
}

func scanClaudeStream(ctx context.Context, r io.Reader, maxTokenSize int) <-chan streamItem {
	out := make(chan streamItem, 64)
	go func() {
		defer close(out)

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)
		for scanner.Scan() {
			line := strings.TrimSpace(string(scanner.Bytes()))
			if line == "" {
				continue
			}
			var event claudeEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				continue
			}
			select {
			case out <- streamItem{event: event}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case out <- streamItem{err: fmt.Errorf("scan claude stream: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()
	return out
}

func (s *turnState) apply(event claudeEvent, includePartialMessages bool, onUpdate runner.AgentUpdateHandler) error {
	if event.SessionID != "" {
		s.sessionID = event.SessionID
	}
	previousModel := s.model
	s.observeModel(event)

	switch event.Type {
	case "system", "init":
		return s.applyInit(event, onUpdate)
	case "assistant":
		if err := s.emitModelChange(previousModel, onUpdate); err != nil {
			return err
		}
		return s.applyAssistant(event, includePartialMessages, onUpdate)
	case "stream_event":
		if err := s.emitModelChange(previousModel, onUpdate); err != nil {
			return err
		}
		return s.applyStreamEvent(event, includePartialMessages, onUpdate)
	case "result":
		if err := s.emitModelChange(previousModel, onUpdate); err != nil {
			return err
		}
		return s.applyResult(event, onUpdate)
	default:
		return nil
	}
}

func (s *turnState) applyInit(event claudeEvent, onUpdate runner.AgentUpdateHandler) error {
	if event.Type == "system" && strings.TrimSpace(event.Subtype) != "init" {
		return nil
	}
	if event.SessionID == "" || s.turnStartedSent {
		return nil
	}
	s.turnStartedSent = true
	return emitUpdate(onUpdate, runner.AgentUpdate{
		Type:            runner.AgentUpdateTurnStarted,
		ThreadID:        event.SessionID,
		TurnID:          event.SessionID,
		Model:           s.model,
		RuntimeIdentity: agentidentity.RuntimeUpdate(s.model, "", "", "", time.Time{}),
	})
}

func (s *turnState) emitModelChange(previousModel string, onUpdate runner.AgentUpdateHandler) error {
	model := strings.TrimSpace(s.model)
	if model == "" || model == strings.TrimSpace(previousModel) {
		return nil
	}
	return emitUpdate(onUpdate, runner.AgentUpdate{
		Type:            runner.AgentUpdateModelUpdated,
		ThreadID:        s.sessionID,
		TurnID:          s.sessionID,
		Model:           model,
		RuntimeIdentity: agentidentity.RuntimeUpdate(model, "", "", "", time.Time{}),
	})
}

func (s *turnState) applyAssistant(
	event claudeEvent,
	includePartialMessages bool,
	onUpdate runner.AgentUpdateHandler,
) error {
	if event.Message == nil {
		return nil
	}
	if event.Message.ID != "" {
		s.partialItemID = event.Message.ID
	}
	if !includePartialMessages {
		for _, block := range event.Message.Content {
			if block.Type != "text" || block.Text == "" {
				continue
			}
			if err := emitUpdate(onUpdate, runner.AgentUpdate{
				Type:     runner.AgentUpdateMessageDelta,
				ThreadID: s.sessionID,
				TurnID:   s.sessionID,
				ItemID:   event.Message.ID,
				Delta:    block.Text,
				Model:    s.model,
			}); err != nil {
				return err
			}
		}
	}
	if event.Message.Usage != nil && !event.Message.Usage.empty() {
		addUsage(&s.usage, *event.Message.Usage)
		return s.emitUsage(onUpdate)
	}
	return nil
}

func (s *turnState) applyStreamEvent(
	event claudeEvent,
	includePartialMessages bool,
	onUpdate runner.AgentUpdateHandler,
) error {
	if event.StreamEvent == nil {
		return nil
	}
	if event.StreamEvent.Message != nil && event.StreamEvent.Message.ID != "" {
		s.partialItemID = event.StreamEvent.Message.ID
	}
	if includePartialMessages && event.StreamEvent.Delta != nil &&
		event.StreamEvent.Delta.Type == "text_delta" &&
		event.StreamEvent.Delta.Text != "" {
		if err := emitUpdate(onUpdate, runner.AgentUpdate{
			Type:     runner.AgentUpdateMessageDelta,
			ThreadID: s.sessionID,
			TurnID:   s.sessionID,
			ItemID:   s.partialItemID,
			Delta:    event.StreamEvent.Delta.Text,
			Model:    s.model,
		}); err != nil {
			return err
		}
	}
	if event.StreamEvent.Usage != nil && !event.StreamEvent.Usage.empty() {
		addUsage(&s.usage, *event.StreamEvent.Usage)
		return s.emitUsage(onUpdate)
	}
	return nil
}

func (s *turnState) applyResult(event claudeEvent, onUpdate runner.AgentUpdateHandler) error {
	s.sawResult = true
	s.resultSubtype = event.Subtype
	s.resultIsError = event.IsError
	// Non-goals: --resume continuity, rate-limit telemetry, and total_cost_usd budget ingest.
	if event.Usage != nil && !event.Usage.empty() {
		s.usage = event.Usage.agentUsage()
		return s.emitUsage(onUpdate)
	}
	if usage := event.topLevelUsage(); !usage.empty() {
		s.usage = usage.agentUsage()
		return s.emitUsage(onUpdate)
	}
	return nil
}

func (s *turnState) emitUsage(onUpdate runner.AgentUpdateHandler) error {
	return emitUpdate(onUpdate, runner.AgentUpdate{
		Type:     runner.AgentUpdateTokenUsage,
		ThreadID: s.sessionID,
		TurnID:   s.sessionID,
		Model:    s.model,
		Tokens:   s.usage,
	})
}

func (s *turnState) observeModel(event claudeEvent) {
	if model := event.modelName(); model != "" {
		s.model = model
	}
}

func (e claudeEvent) modelName() string {
	if model := strings.TrimSpace(e.Model); model != "" {
		return model
	}
	if e.Message != nil {
		if model := strings.TrimSpace(e.Message.Model); model != "" {
			return model
		}
	}
	if e.StreamEvent != nil && e.StreamEvent.Message != nil {
		if model := strings.TrimSpace(e.StreamEvent.Message.Model); model != "" {
			return model
		}
	}
	return ""
}

func (e claudeEvent) topLevelUsage() claudeUsage {
	return claudeUsage{
		InputTokens:  e.InputTokens,
		OutputTokens: e.OutputTokens,
		TotalTokens:  e.TotalTokens,
	}
}

func (u claudeUsage) empty() bool {
	return u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.TotalTokens == 0 &&
		u.CacheCreationInputTokens == 0 &&
		u.CacheReadInputTokens == 0 &&
		u.CachedInputTokens == 0 &&
		u.ReasoningOutputTokens == 0
}

func (u claudeUsage) agentUsage() runner.AgentTokenUsage {
	cached := u.CacheReadInputTokens + u.CachedInputTokens
	input := u.InputTokens + u.CacheCreationInputTokens + cached
	total := u.TotalTokens
	if minimumTotal := input + u.OutputTokens; total < minimumTotal {
		total = minimumTotal
	}
	return runner.AgentTokenUsage{
		InputTokens:           input,
		CachedInputTokens:     cached,
		OutputTokens:          u.OutputTokens,
		ReasoningOutputTokens: u.ReasoningOutputTokens,
		TotalTokens:           total,
	}
}

func addUsage(t *runner.AgentTokenUsage, u claudeUsage) {
	next := u.agentUsage()
	t.InputTokens += next.InputTokens
	t.CachedInputTokens += next.CachedInputTokens
	t.OutputTokens += next.OutputTokens
	t.ReasoningOutputTokens += next.ReasoningOutputTokens
	t.TotalTokens += next.TotalTokens
}
