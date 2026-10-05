package chat

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type SessionState struct {
	ID         string
	Identity   operatortool.Identity
	Client     string
	LastUsedAt time.Time
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
	current := s.session(id)
	current.mu.Lock()
	defer current.mu.Unlock()
	return s.restoreSession(ctx, id, current)
}

func (s *Service) restoreSession(ctx context.Context, id string, current *session) error {
	if current.connection != nil || s.store == nil {
		return nil
	}
	state, found, err := s.store.Load(ctx, id, s.now().UTC(), s.sessionTTL)
	if err != nil || !found {
		return err
	}
	connection := operatortool.Connection{ID: id, Identity: state.Identity, Client: state.Client}
	connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
		if s.resolve == nil {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		return s.resolve(ctx, state.Identity)
	}
	current.connection = &connection

	s.mu.Lock()
	current.lastUsedAt = state.LastUsedAt
	s.mu.Unlock()
	return nil
}

func (s *Service) persistSession(ctx context.Context, current *session) error {
	if s.store == nil || current.connection == nil {
		return nil
	}
	now := s.now().UTC()
	if err := s.store.Save(ctx, SessionState{ID: current.connection.ID, Identity: current.connection.Identity, Client: current.connection.Client, LastUsedAt: now}, now, s.sessionTTL, s.sessionLimit); err != nil {
		return err
	}
	s.mu.Lock()
	current.lastUsedAt = now
	s.mu.Unlock()
	return nil
}
