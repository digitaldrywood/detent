package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type operatorChatStore struct {
	database *database
}

func (store operatorChatStore) Load(ctx context.Context, id string, now time.Time, ttl time.Duration) (chat.SessionState, bool, error) {
	state := chat.SessionState{ID: id}
	var identity, lastUsed string
	err := store.database.db.QueryRowContext(ctx, `SELECT identity_json, client, last_used_at FROM operator_chat_sessions WHERE connection_id = ? AND last_used_at >= ?`, id, now.Add(-ttl).UTC().Format("2006-01-02T15:04:05.000000000Z")).Scan(&identity, &state.Client, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	if err := json.Unmarshal([]byte(identity), &state.Identity); err != nil {
		return state, false, err
	}
	state.LastUsedAt, err = parseTimeValue(lastUsed)
	return state, err == nil, err
}

func (store operatorChatStore) Save(ctx context.Context, state chat.SessionState, now time.Time, ttl time.Duration, limit int) error {
	identity, err := json.Marshal(state.Identity)
	if err != nil {
		return err
	}
	tx, err := store.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO operator_chat_sessions(connection_id, organization_id, identity_json, client, last_used_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(connection_id) DO UPDATE SET last_used_at = excluded.last_used_at WHERE identity_json = excluded.identity_json`, state.ID, state.Identity.OrganizationID, string(identity), state.Client, state.LastUsedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"))
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

func (s *Service) resolveOperatorChatAuthority(ctx context.Context, identity operatortool.Identity) (operatortool.Authority, error) {
	if resolve, ok := ctx.Value(hubOperatorResolverKey{}).(func(context.Context) (apiCredential, error)); ok {
		current, err := resolve(ctx)
		if err == nil && current.EntryKey != nil && operatorIdentity(current, identity.OrganizationID) == identity {
			return s.operatorCurrentAuthority(ctx, current, identity.OrganizationID)
		}
	}
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
