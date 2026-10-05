package chat

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type SessionState struct {
	ID                  string
	Identity            operatortool.Identity
	Client              string
	RequireConfirmation bool
	Actions             []Action
	LastUsedAt          time.Time
}

type SessionStore interface {
	Load(context.Context, string, time.Time, time.Duration) (SessionState, bool, error)
	Save(context.Context, SessionState, time.Time, time.Duration, int) error
	LoadConnectionMode(context.Context, string, operatortool.Identity) (ConnectionMode, error)
	SaveConnectionMode(context.Context, string, operatortool.Identity, ConnectionMode) error
}

type connectionPreference struct {
	identity operatortool.Identity
	mode     ConnectionMode
}

func (s *Service) connectionMode(ctx context.Context, id string, identity operatortool.Identity) (ConnectionMode, error) {
	if s.store != nil {
		return s.store.LoadConnectionMode(ctx, id, identity)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	preference, found := s.connectionModes[id]
	if !found {
		return ConfirmationMode, nil
	}
	if preference.identity != identity {
		return ConfirmationMode, operatortool.ErrAccessDenied
	}
	return preference.mode, nil
}

func (s *Service) saveConnectionMode(ctx context.Context, connection operatortool.Connection, mode ConnectionMode) error {
	if s.store != nil {
		return s.store.SaveConnectionMode(ctx, connection.ID, connection.Identity, mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connectionModes[connection.ID] = connectionPreference{identity: connection.Identity, mode: mode}
	return nil
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
	connection := operatortool.Connection{ID: id, Identity: state.Identity, Client: state.Client, RequireConfirmation: state.RequireConfirmation}
	connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
		if s.resolve == nil {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		return s.resolve(ctx, state.Identity)
	}
	mode, err := s.connectionMode(ctx, id, state.Identity)
	if err != nil {
		return err
	}
	if connection.RequireConfirmation {
		mode = ConfirmationMode
	}
	current.connection, current.mode = &connection, mode
	current.actions = cloneActions(state.Actions)
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
	if err := s.store.Save(ctx, SessionState{ID: current.connection.ID, Identity: current.connection.Identity, Client: current.connection.Client, RequireConfirmation: current.connection.RequireConfirmation, Actions: cloneActions(current.actions), LastUsedAt: now}, now, s.sessionTTL, s.sessionLimit); err != nil {
		return err
	}
	s.mu.Lock()
	current.lastUsedAt = now
	s.mu.Unlock()
	return nil
}
