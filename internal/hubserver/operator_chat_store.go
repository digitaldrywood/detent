package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type operatorChatStore struct {
	database *database
}

type operatorStoredAction struct {
	Action         chat.Action       `json:"action"`
	NativeWorkflow bool              `json:"native_workflow,omitempty"`
	ConversationID string            `json:"conversation_id,omitempty"`
	Mutation       mutation.Metadata `json:"mutation"`
}

func (store operatorChatStore) Load(ctx context.Context, id string, now time.Time, ttl time.Duration) (chat.SessionState, bool, error) {
	state := chat.SessionState{ID: id}
	var identity, actions, lastUsed string
	err := store.database.db.QueryRowContext(ctx, `SELECT identity_json, client, require_confirmation, actions_json, last_used_at FROM operator_chat_sessions WHERE connection_id = ? AND last_used_at >= ?`, id, now.Add(-ttl).UTC().Format("2006-01-02T15:04:05.000000000Z")).Scan(&identity, &state.Client, &state.RequireConfirmation, &actions, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	if err := json.Unmarshal([]byte(identity), &state.Identity); err != nil {
		return state, false, err
	}
	var stored []operatorStoredAction
	if err := json.Unmarshal([]byte(actions), &stored); err != nil {
		return state, false, err
	}
	for _, action := range stored {
		action.Action.NativeWorkflow = action.NativeWorkflow
		action.Action.ConversationID = action.ConversationID
		action.Action.Mutation = action.Mutation
		state.Actions = append(state.Actions, action.Action)
	}
	state.LastUsedAt, err = parseTimeValue(lastUsed)
	return state, err == nil, err
}

func (store operatorChatStore) Save(ctx context.Context, state chat.SessionState, now time.Time, ttl time.Duration, limit int) error {
	identity, err := json.Marshal(state.Identity)
	if err != nil {
		return err
	}
	stored := make([]operatorStoredAction, 0, len(state.Actions))
	for _, action := range state.Actions {
		stored = append(stored, operatorStoredAction{Action: action, NativeWorkflow: action.NativeWorkflow, ConversationID: action.ConversationID, Mutation: action.Mutation})
	}
	actions, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	tx, err := store.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO operator_chat_sessions(connection_id, organization_id, identity_json, client, require_confirmation, actions_json, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(connection_id) DO UPDATE SET actions_json = excluded.actions_json, last_used_at = excluded.last_used_at WHERE identity_json = excluded.identity_json`, state.ID, state.Identity.OrganizationID, string(identity), state.Client, state.RequireConfirmation, string(actions), state.LastUsedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return operatortool.ErrAccessDenied
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM operator_chat_sessions WHERE last_used_at < ? OR connection_id NOT IN (SELECT connection_id FROM operator_chat_sessions ORDER BY last_used_at DESC, connection_id DESC LIMIT ?)`, now.Add(-ttl).UTC().Format("2006-01-02T15:04:05.000000000Z"), limit); err != nil {
		return err
	}
	return tx.Commit()
}

func (store operatorChatStore) LoadConnectionMode(ctx context.Context, id string, identity operatortool.Identity) (chat.ConnectionMode, error) {
	var storedIdentity string
	var mode chat.ConnectionMode
	err := store.database.db.QueryRowContext(ctx, `SELECT identity_json, mode FROM operator_connections WHERE connection_id = ?`, id).Scan(&storedIdentity, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		return chat.ConfirmationMode, nil
	}
	if err != nil {
		return chat.ConfirmationMode, err
	}
	var original operatortool.Identity
	if err := json.Unmarshal([]byte(storedIdentity), &original); err != nil {
		return chat.ConfirmationMode, err
	}
	if original != identity {
		return chat.ConfirmationMode, operatortool.ErrAccessDenied
	}
	return mode, nil
}

func (store operatorChatStore) SaveConnectionMode(ctx context.Context, id string, identity operatortool.Identity, mode chat.ConnectionMode) error {
	raw, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	result, err := store.database.db.ExecContext(ctx, `INSERT INTO operator_connections(connection_id, organization_id, identity_json, mode) VALUES (?, ?, ?, ?) ON CONFLICT(connection_id) DO UPDATE SET mode = excluded.mode WHERE identity_json = excluded.identity_json`, id, identity.OrganizationID, string(raw), mode)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return operatortool.ErrAccessDenied
	}
	return nil
}

func (s *Service) resolveOperatorChatAuthority(ctx context.Context, identity operatortool.Identity) (operatortool.Authority, error) {
	var credential apiCredential
	var err error
	if identity.SessionID == "" {
		credential, _, err = s.authenticateAPIHash(ctx, identity.CredentialID, "", "")
	} else if s.config.Hosted != nil && strings.HasPrefix(identity.PrincipalID, "account:") {
		credential, err = s.hostedAccountCredential(ctx, identity.SessionID)
	} else if s.config.Hosted != nil {
		session, sessionErr := s.WebSession(ctx, identity.SessionID, s.config.now())
		if sessionErr != nil {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		credential, _, err = s.hostedSessionCredential(ctx, session, identity.SessionID)
	} else {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	if err != nil || operatorIdentity(credential, identity.OrganizationID) != identity {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	return s.operatorCurrentAuthority(ctx, credential, identity.OrganizationID)
}
