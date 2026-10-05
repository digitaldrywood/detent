package chat

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type SessionState struct {
	ID                  string
	Identity            operatortool.Identity
	Client              string
	RequireConfirmation bool
	Mode                ConnectionMode
	Actions             []Action
	LastUsedAt          time.Time
}

type SessionStore interface {
	Load(context.Context, string, time.Time, time.Duration) (SessionState, bool, error)
	Save(context.Context, SessionState, time.Time, time.Duration, int) error
}

func WithSessionStore(store SessionStore, resolve func(context.Context, operatortool.Identity) (operatortool.Authority, error)) Option {
	return func(s *Service) {
		s.store, s.resolve = store, resolve
	}
}

func (s *Service) RestoreConnection(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	current := s.session(id)
	if current != nil {
		current.mu.Lock()
		defer current.mu.Unlock()
		return s.restoreSession(ctx, id, current)
	}
	if s.store == nil {
		return nil
	}
	state, found, err := s.store.Load(ctx, id, s.now().UTC(), s.sessionTTL)
	if err != nil || !found {
		return err
	}
	current = s.ensureSession(id)
	current.mu.Lock()
	defer current.mu.Unlock()
	s.restoreSessionState(id, current, state)
	return nil
}

func (s *Service) restoreSession(ctx context.Context, id string, current *session) error {
	if current.connection != nil || s.store == nil {
		return nil
	}
	state, found, err := s.store.Load(ctx, id, s.now().UTC(), s.sessionTTL)
	if err != nil || !found {
		return err
	}
	s.restoreSessionState(id, current, state)
	return nil
}

func (s *Service) restoreSessionState(id string, current *session, state SessionState) {
	if current.connection != nil {
		return
	}
	connection := operatortool.Connection{ID: id, Identity: state.Identity, Client: state.Client, RequireConfirmation: state.RequireConfirmation}
	connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
		if s.resolve == nil {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		return s.resolve(ctx, state.Identity)
	}
	current.connection, current.mode = &connection, state.Mode
	current.actions = cloneActions(state.Actions)
	s.mu.Lock()
	current.lastUsedAt = state.LastUsedAt
	current.lastAccessedAt = s.now().UTC()
	s.mu.Unlock()
}

func (s *Service) persistSession(ctx context.Context, current *session) error {
	if s.store == nil || current.connection == nil {
		return nil
	}
	now := s.now().UTC()
	if err := s.store.Save(ctx, SessionState{ID: current.connection.ID, Identity: current.connection.Identity, Client: current.connection.Client, RequireConfirmation: current.connection.RequireConfirmation, Mode: current.mode, Actions: cloneActions(current.actions), LastUsedAt: now}, now, s.sessionTTL, s.sessionLimit); err != nil {
		return err
	}
	s.mu.Lock()
	current.lastUsedAt = now
	s.mu.Unlock()
	return nil
}
