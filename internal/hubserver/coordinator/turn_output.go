package coordinator

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/runner"
)

const (
	DeltaFlushInterval = 150 * time.Millisecond
	deltaFlushBytes    = 2048
)

type TurnOutput struct {
	conversationID string
	events         TurnEventDeps
	mu             sync.Mutex
	assistant      conversations.ConversationMessageRecord
	model          string
	usage          runner.AgentTokenCounts
	normalizeUsage func(runner.AgentTokenUsage) runner.AgentTokenUsage
	buffered       strings.Builder
	firstBuffered  time.Time
	textBoundary   bool
	threadID       string
	toolMessages   map[string]conversations.ConversationMessageRecord
}

func (s *TurnOutput) Initialize(id, threadID string, assistant conversations.ConversationMessageRecord, events TurnEventDeps) {
	s.conversationID, s.threadID, s.assistant, s.events = id, threadID, assistant, events
	s.toolMessages = make(map[string]conversations.ConversationMessageRecord)
}

func (s *TurnOutput) ConfigureModel(model string, normalize func(runner.AgentTokenUsage) runner.AgentTokenUsage) {
	s.model, s.normalizeUsage = model, normalize
}

func (s *TurnOutput) Model() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

func (s *TurnOutput) Summary() (conversations.ConversationMessageRecord, string, runner.AgentTokenCounts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.assistant, s.model, s.usage
}

func (s *TurnOutput) AppendThreadMessage(ctx context.Context, id string, message conversations.ConversationMessageRecord, appendMessage func(context.Context, string, conversations.ConversationMessageRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	message.ThreadID = s.threadID
	return appendMessage(ctx, id, message)
}

func (s *TurnOutput) Finish(ctx context.Context, deps TurnFinishDeps, users []conversations.ConversationMessageRecord, budgetStopped bool, outcome conversation.Delivery, runErr error, usage *conversations.ConversationUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return deps.Finish(ctx, s.conversationID, s.threadID, &s.assistant, users, budgetStopped, outcome, runErr, usage)
}

func (s *TurnOutput) HandleUpdate(ctx context.Context, update runner.AgentUpdate) {
	if model := strings.TrimSpace(update.Model); model != "" {
		s.mu.Lock()
		s.model = model
		s.mu.Unlock()
	}
	if update.ThreadID != "" {
		s.ObserveThread(ctx, update.ThreadID)
	}
	switch update.Type {
	case runner.AgentUpdateMessageDelta:
		s.bufferDelta(ctx, update.Delta)
	case runner.AgentUpdateTokenUsage:
		s.mu.Lock()
		if s.normalizeUsage != nil {
			update.Tokens = s.normalizeUsage(update.Tokens)
		}
		s.usage = runner.AgentTokenCounts{
			InputTokens: update.Tokens.InputTokens, CachedInputTokens: update.Tokens.CachedInputTokens,
			OutputTokens: update.Tokens.OutputTokens, ReasoningOutputTokens: update.Tokens.ReasoningOutputTokens,
			TotalTokens: update.Tokens.TotalTokens,
		}
		s.mu.Unlock()
	case runner.AgentUpdateToolStarted, runner.AgentUpdateToolCompleted:
		s.EndTextSegment(ctx)
		s.recordTool(ctx, update)
	}
}

func (s *TurnOutput) ObserveThread(ctx context.Context, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threadID == "" || threadID == s.threadID {
		return
	}
	s.threadID = threadID
	err := PublishProviderThread(ctx, s.events.Write, s.conversationID, threadID)
	if err != nil {
		s.events.Logger.Warn("coordinator could not persist provider thread", "conversation_id", s.conversationID, "error", err)
	}
}

func (s *TurnOutput) bufferDelta(ctx context.Context, delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	if s.buffered.Len() == 0 {
		s.firstBuffered = time.Now()
	}
	if s.textBoundary {
		if s.assistant.Text != "" {
			s.buffered.WriteString("\n\n")
		}
		s.textBoundary = false
	}
	s.buffered.WriteString(delta)
	due := s.buffered.Len() >= deltaFlushBytes || time.Since(s.firstBuffered) >= DeltaFlushInterval
	s.mu.Unlock()
	if due {
		s.Flush(ctx)
	}
}

func (s *TurnOutput) EndTextSegment(ctx context.Context) {
	s.Flush(ctx)
	s.mu.Lock()
	s.textBoundary = true
	s.mu.Unlock()
}

func (s *TurnOutput) Flush(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buffered.Len() == 0 {
		return
	}
	text := s.buffered.String()
	s.buffered.Reset()
	updated, err := PublishDelta(ctx, s.events.Write, s.conversationID, s.assistant, text)
	if err == nil {
		s.assistant = updated
	}
	if err != nil {

		s.assistant.Text += text
		s.events.Logger.Warn("coordinator could not persist delta", "conversation_id", s.conversationID, "error", err)
	}
}

func (s *TurnOutput) recordTool(ctx context.Context, update runner.AgentUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.PublishToolUpdate(ctx, s.conversationID, s.threadID, s.toolMessages, update)
}
