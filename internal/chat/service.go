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

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
	store        SessionStore
	resolve      func(context.Context, operatortool.Identity) (operatortool.Authority, error)
}

type session struct {
	connection *operatortool.Connection
	mode       ConnectionMode
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

	tools := Tools()
	if current.connection != nil {
		tools = tools[:len(operatortool.Catalog())]
	}
	actionsBefore := len(current.actions)
	response, err := s.provider.Reply(ctx, TurnRequest{
		ThreadID: current.threadID,
		Prompt:   content,
		Tools:    tools,
		Handle: func(ctx context.Context, call ToolCall) (ToolResult, error) {
			if s.tools == nil {
				return ToolResult{}, errors.New("chat tools unavailable")
			}
			if current.connection != nil {
				allowed := false
				for _, definition := range operatortool.Catalog() {
					if definition.Name == call.Name {
						allowed = true
					}
				}
				if !allowed {
					return ToolResult{}, operatortool.ErrAccessDenied
				}
				var selector struct {
					ProjectID string `json:"project_id"`
				}
				if json.Unmarshal(call.Arguments, &selector) != nil {
					return ToolResult{}, operatortool.ErrInvalidArguments
				}
				var err error
				ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: selector.ProjectID})
				if err != nil {
					return ToolResult{}, err
				}
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
	if current.connection != nil {
		if err := authorizeHuman(ctx, current.connection.Identity.OrganizationID); err != nil {
			return s.conversation(current), err
		}
		var err error
		ctx, err = authorizeAction(ctx, *current.connection, current.actions[index])
		if err != nil {
			s.auditAction(ctx, current.actions[index], "denied")
			return s.resolveAction(ctx, current, index, "Operator access is unavailable.", err)
		}
		previous := current.actions[index]
		current.actions[index].Mode = current.mode
		current.actions[index].Mutation.Mode = string(current.mode)
		current.actions[index].Mutation.Confirmation = "approved"
		if err := s.persistSession(ctx, current); err != nil {
			current.actions[index] = previous
			return s.conversation(current), err
		}
		s.auditAction(ctx, current.actions[index], "approved")
	}
	if s.actions == nil {
		return s.resolveAction(ctx, current, index, "Action execution is unavailable.", ErrUnavailable)
	}
	result, err := s.actions.ExecuteAction(ctx, current.actions[index])
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}
	auditAction := current.actions[index]
	if err == nil && result.ResourceID != "" {
		auditAction.IssueID = result.ResourceID
	}
	s.auditAction(ctx, auditAction, outcome)
	return s.resolveExecution(ctx, current, index, result, err)
}

func (s *Service) Reject(ctx context.Context, sessionID string, actionID string) (Conversation, error) {
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
	if current.connection != nil {
		return s.conversation(current), operatortool.ErrAccessDenied
	}
	return s.rejectAction(ctx, current, index)
}

func (s *Service) rejectAction(ctx context.Context, current *session, index int) (Conversation, error) {
	previous := current.actions[index]
	now := s.now().UTC()
	current.actions[index].Status = ActionRejected
	current.actions[index].Result = "Cancelled by the operator."
	current.actions[index].ResolvedAt = &now
	if err := s.persistSession(ctx, current); err != nil {
		current.actions[index] = previous
		return s.conversation(current), err
	}
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

func (s *Service) resolveExecution(ctx context.Context, current *session, index int, result ActionExecution, actionErr error) (Conversation, error) {
	if actionErr == nil {
		if current.actions[index].Kind == ActionKind(operatortool.SessionLogout) && result.SignOut != nil {
			copy := *result.SignOut
			current.actions[index].SignOut = &copy
		}
		current.actions[index].resultData = append(json.RawMessage(nil), result.Data...)
	}
	if actionErr == nil && result.ResourceID != "" {
		current.actions[index].IssueID = result.ResourceID
		current.actions[index].Identifier = result.Identifier
		current.actions[index].ResourceURL = result.URL
		current.actions[index].Revision = result.Revision
		current.actions[index].CommentID = result.CommentID
	}
	return s.resolveAction(ctx, current, index, result.Message, actionErr)
}

func (s *Service) resolveAction(ctx context.Context, current *session, index int, result string, actionErr error) (Conversation, error) {
	now := s.now().UTC()
	current.actions[index].ResolvedAt = &now
	current.actions[index].Result = strings.TrimSpace(result)
	if actionErr != nil {
		current.actions[index].Status = ActionFailed
		message := "The action failed. The board may not have changed."
		if current.actions[index].Result != "" {
			message = current.actions[index].Result
		}
		if err := s.persistSession(ctx, current); err != nil {
			return s.conversation(current), errors.Join(actionErr, err)
		}
		s.appendAssistant(current, message, true)
		return s.conversation(current), actionErr
	}
	current.actions[index].Status = ActionSucceeded
	message := current.actions[index].Result
	if message == "" {
		message = "Action completed."
	}
	if err := s.persistSession(ctx, current); err != nil {
		return s.conversation(current), err
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
	} else if s.store == nil {
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
	conversation := Conversation{Mode: current.mode,
		Messages:    append([]Message(nil), current.messages...),
		Actions:     cloneActions(current.actions),
		Unavailable: s.provider == nil && current.connection == nil,
	}
	if current.connection != nil {
		conversation.RequireConfirmation = current.connection.RequireConfirmation
		conversation.PrincipalID = current.connection.Identity.PrincipalID
		conversation.ConnectionID = current.connection.ID
		conversation.ApprovalBaseURL = current.connection.DashboardURL
		conversation.OrganizationID = current.connection.Identity.OrganizationID
		conversation.Client = current.connection.Client
	}
	return conversation
}

func cloneActions(actions []Action) []Action {
	out := make([]Action, len(actions))
	copy(out, actions)
	for index := range out {
		out[index].resultData = nil
		if out[index].SignOut != nil {
			copy := *out[index].SignOut
			out[index].SignOut = &copy
		}
		out[index].Labels = append([]string(nil), out[index].Labels...)
		out[index].Arguments = append(json.RawMessage(nil), out[index].Arguments...)
		if out[index].Work != nil {
			work := *out[index].Work
			if work.Title != nil {
				title := *work.Title
				work.Title = &title
			}
			if work.Body != nil {
				body := *work.Body
				work.Body = &body
			}
			if work.Priority != nil {
				priority := *work.Priority
				work.Priority = &priority
			}
			if work.Labels != nil {
				labels := append([]string{}, (*work.Labels)...)
				work.Labels = &labels
			}
			out[index].Work = &work
		}
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

func (s *Service) auditAction(ctx context.Context, action Action, outcome string) {
	if auditor, ok := s.actions.(interface {
		AuditAction(context.Context, Action, string)
	}); ok {
		auditor.AuditAction(ctx, action, outcome)
	}
}
