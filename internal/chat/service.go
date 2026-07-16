package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	defaultSessionLimit = 128
	defaultSessionTTL   = 24 * time.Hour
	maxMessageLength    = 8 * 1024
)

var (
	ErrUnavailable        = errors.New("chat provider unavailable")
	ErrEmptyMessage       = errors.New("chat message is required")
	ErrMessageTooLong     = errors.New("chat message is too long")
	ErrActionNotFound     = errors.New("chat action not found")
	ErrActionNotPending   = errors.New("chat action is not pending")
	ErrEmptyProviderReply = errors.New("chat provider returned an empty reply")
)

type Service struct {
	provider     Provider
	tools        ToolExecutor
	actions      ActionExecutor
	now          func() time.Time
	newID        func() (string, error)
	sessionLimit int
	sessionTTL   time.Duration
	mu           sync.Mutex
	sessions     map[string]*session
}

type session struct {
	mu         sync.Mutex
	threadID   string
	messages   []Message
	actions    []Action
	lastUsedAt time.Time
}

type Option func(*Service)

func WithClock(now func() time.Time) Option {
	return func(service *Service) {
		if now != nil {
			service.now = now
		}
	}
}

func WithIDGenerator(generator func() (string, error)) Option {
	return func(service *Service) {
		if generator != nil {
			service.newID = generator
		}
	}
}

func NewService(provider Provider, tools ToolExecutor, actions ActionExecutor, options ...Option) *Service {
	service := &Service{
		provider:     provider,
		tools:        tools,
		actions:      actions,
		now:          time.Now,
		newID:        randomID,
		sessionLimit: defaultSessionLimit,
		sessionTTL:   defaultSessionTTL,
		sessions:     make(map[string]*session),
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Conversation(sessionID string) Conversation {
	current := s.session(sessionID)
	current.mu.Lock()
	defer current.mu.Unlock()
	return s.conversation(current)
}

func (s *Service) Action(sessionID string, actionID string) (Action, bool) {
	current := s.session(sessionID)
	current.mu.Lock()
	defer current.mu.Unlock()
	index := actionIndex(current.actions, actionID)
	if index < 0 {
		return Action{}, false
	}
	return cloneActions(current.actions[index : index+1])[0], true
}

func (s *Service) Send(ctx context.Context, sessionID string, content string) (Conversation, error) {
	current := s.session(sessionID)
	current.mu.Lock()
	defer current.mu.Unlock()

	content = strings.TrimSpace(content)
	if content == "" {
		return s.conversation(current), ErrEmptyMessage
	}
	if len(content) > maxMessageLength {
		return s.conversation(current), ErrMessageTooLong
	}
	if s.provider == nil {
		return s.providerFailure(current, content, ErrUnavailable)
	}

	messageID, err := s.newID()
	if err != nil {
		return s.conversation(current), fmt.Errorf("create user message id: %w", err)
	}
	current.messages = append(current.messages, Message{ID: messageID, Role: RoleUser, Content: content, At: s.now().UTC()})

	actionsBefore := len(current.actions)
	response, err := s.provider.Reply(ctx, TurnRequest{
		ThreadID: current.threadID,
		Prompt:   content,
		Tools:    Tools(),
		Handle: func(ctx context.Context, call ToolCall) (ToolResult, error) {
			if s.tools == nil {
				return ToolResult{}, errors.New("chat tools unavailable")
			}
			result, err := s.tools.ExecuteTool(ctx, call)
			if err != nil || result.Proposal == nil {
				return result, err
			}
			proposal := *result.Proposal
			proposal.ID, err = s.newID()
			if err != nil {
				return ToolResult{}, fmt.Errorf("create proposal id: %w", err)
			}
			proposal.Status = ActionPending
			proposal.CreatedAt = s.now().UTC()
			current.actions = append(current.actions, proposal)
			result.Proposal = &proposal
			payload, err := json.Marshal(map[string]any{
				"proposal_id": proposal.ID,
				"action":      proposal.Kind,
				"summary":     ActionSummary(proposal),
				"status":      proposal.Status,
			})
			if err != nil {
				return ToolResult{}, fmt.Errorf("encode proposal: %w", err)
			}
			result.Content = string(payload)
			return result, nil
		},
	})
	if err != nil {
		current.actions = current.actions[:actionsBefore]
		return s.providerFailureAfterUser(current, err)
	}
	response.Content = strings.TrimSpace(response.Content)
	if response.Content == "" {
		current.actions = current.actions[:actionsBefore]
		return s.providerFailureAfterUser(current, ErrEmptyProviderReply)
	}
	if threadID := strings.TrimSpace(response.ThreadID); threadID != "" {
		current.threadID = threadID
	}
	assistantID, err := s.newID()
	if err != nil {
		return s.conversation(current), fmt.Errorf("create assistant message id: %w", err)
	}
	current.messages = append(current.messages, Message{ID: assistantID, Role: RoleAssistant, Content: response.Content, At: s.now().UTC()})
	return s.conversation(current), nil
}

func (s *Service) Confirm(ctx context.Context, sessionID string, actionID string) (Conversation, error) {
	current := s.session(sessionID)
	current.mu.Lock()
	defer current.mu.Unlock()
	index := actionIndex(current.actions, actionID)
	if index < 0 {
		return s.conversation(current), ErrActionNotFound
	}
	if current.actions[index].Status != ActionPending {
		return s.conversation(current), ErrActionNotPending
	}
	if s.actions == nil {
		return s.resolveAction(current, index, "Action execution is unavailable.", ErrUnavailable)
	}
	result, err := s.actions.ExecuteAction(ctx, current.actions[index])
	return s.resolveAction(current, index, result, err)
}

func (s *Service) Reject(sessionID string, actionID string) (Conversation, error) {
	current := s.session(sessionID)
	current.mu.Lock()
	defer current.mu.Unlock()
	index := actionIndex(current.actions, actionID)
	if index < 0 {
		return s.conversation(current), ErrActionNotFound
	}
	if current.actions[index].Status != ActionPending {
		return s.conversation(current), ErrActionNotPending
	}
	now := s.now().UTC()
	current.actions[index].Status = ActionRejected
	current.actions[index].Result = "Cancelled by the operator."
	current.actions[index].ResolvedAt = &now
	s.appendAssistant(current, "Cancelled the proposed action.", false)
	return s.conversation(current), nil
}

func (s *Service) providerFailure(current *session, content string, err error) (Conversation, error) {
	messageID, idErr := s.newID()
	if idErr != nil {
		return s.conversation(current), errors.Join(err, idErr)
	}
	current.messages = append(current.messages, Message{ID: messageID, Role: RoleUser, Content: content, At: s.now().UTC()})
	return s.providerFailureAfterUser(current, err)
}

func (s *Service) providerFailureAfterUser(current *session, err error) (Conversation, error) {
	s.appendAssistant(current, "Chat is temporarily unavailable. The board was not changed.", true)
	return s.conversation(current), err
}

func (s *Service) resolveAction(current *session, index int, result string, actionErr error) (Conversation, error) {
	now := s.now().UTC()
	current.actions[index].ResolvedAt = &now
	current.actions[index].Result = strings.TrimSpace(result)
	if actionErr != nil {
		current.actions[index].Status = ActionFailed
		message := "The action failed. The board may not have changed."
		if current.actions[index].Result != "" {
			message = current.actions[index].Result
		}
		s.appendAssistant(current, message, true)
		return s.conversation(current), actionErr
	}
	current.actions[index].Status = ActionSucceeded
	message := current.actions[index].Result
	if message == "" {
		message = "Action completed."
	}
	s.appendAssistant(current, message, false)
	return s.conversation(current), nil
}

func (s *Service) appendAssistant(current *session, content string, isError bool) {
	messageID, err := s.newID()
	if err != nil {
		messageID = fmt.Sprintf("message-%d", s.now().UnixNano())
	}
	current.messages = append(current.messages, Message{ID: messageID, Role: RoleAssistant, Content: content, At: s.now().UTC(), Error: isError})
}

func (s *Service) session(sessionID string) *session {
	sessionID = strings.TrimSpace(sessionID)
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	current := s.sessions[sessionID]
	if current == nil {
		current = &session{lastUsedAt: now}
		s.sessions[sessionID] = current
	} else {
		current.lastUsedAt = now
	}
	return current
}

func (s *Service) prune(now time.Time) {
	for id, current := range s.sessions {
		if now.Sub(current.lastUsedAt) > s.sessionTTL {
			delete(s.sessions, id)
		}
	}
	for len(s.sessions) >= s.sessionLimit {
		oldestID := ""
		var oldest time.Time
		for id, current := range s.sessions {
			if oldestID == "" || current.lastUsedAt.Before(oldest) {
				oldestID = id
				oldest = current.lastUsedAt
			}
		}
		delete(s.sessions, oldestID)
	}
}

func (s *Service) conversation(current *session) Conversation {
	return Conversation{
		Messages:    append([]Message(nil), current.messages...),
		Actions:     cloneActions(current.actions),
		Unavailable: s.provider == nil,
	}
}

func cloneActions(actions []Action) []Action {
	out := make([]Action, len(actions))
	copy(out, actions)
	for index := range out {
		out[index].Labels = append([]string(nil), out[index].Labels...)
	}
	return out
}

func actionIndex(actions []Action, id string) int {
	id = strings.TrimSpace(id)
	for index := range actions {
		if actions[index].ID == id {
			return index
		}
	}
	return -1
}

func randomID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
