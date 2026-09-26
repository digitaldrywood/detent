package cloudentry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

var errNoSession = errors.New("shared session is missing, expired or revoked")

type accountSession struct {
	Hash       string
	CSRFSecret string
	Subject    string
	Email      string
	Identity   auth.HostedIdentity
}

type authorization struct {
	Binding      string
	Organization string
	Identity     auth.HostedIdentity
}

type authStore struct {
	store *store
	now   func() time.Time
}

func (a *authStore) createSession(ctx context.Context, hash, csrfSecret string, identity auth.Identity) error {
	encoded, err := json.Marshal(identity.Hosted)
	if err != nil {
		return err
	}
	_, err = a.store.db.ExecContext(ctx, "INSERT INTO sessions(token_hash,subject,email,csrf_secret,identity_json,created_at,expires_at) VALUES (?,?,?,?,?,?,?)",
		hash, identity.Subject, identity.Email, csrfSecret, string(encoded), formatTime(a.now()), formatTime(identity.Hosted.ExpiresAt))
	return err
}

func (a *authStore) session(ctx context.Context, hash string) (accountSession, error) {
	var session accountSession
	var encoded, expires string
	var revoked sql.NullString
	err := a.store.db.QueryRowContext(ctx, "SELECT token_hash,subject,email,csrf_secret,identity_json,expires_at,revoked_at FROM sessions WHERE token_hash = ?", hash).
		Scan(&session.Hash, &session.Subject, &session.Email, &session.CSRFSecret, &encoded, &expires, &revoked)
	if err != nil || revoked.Valid {
		return accountSession{}, errNoSession
	}
	expiry, err := parseTime(expires)
	if err != nil || !expiry.After(a.now()) || json.Unmarshal([]byte(encoded), &session.Identity) != nil || session.Identity.Subject != session.Subject {
		return accountSession{}, errNoSession
	}
	return session, nil
}

func (a *authStore) authorize(ctx context.Context, session accountSession, organization string, identity auth.HostedIdentity) (authorization, []authorization, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return authorization{}, nil, err
	}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return authorization{}, nil, err
	}
	defer tx.Rollback()
	replaced, err := activeAuthorizations(ctx, tx, "SELECT binding,organization_id,identity_json,expires_at FROM authorizations WHERE session_hash = ? AND organization_id = ? AND revoked_at IS NULL", session.Hash, organization)
	if err != nil {
		return authorization{}, nil, err
	}
	now := formatTime(a.now())
	if _, err := tx.ExecContext(ctx, "UPDATE authorizations SET revoked_at = ? WHERE session_hash = ? AND organization_id = ? AND revoked_at IS NULL", now, session.Hash, organization); err != nil {
		return authorization{}, nil, err
	}
	result := authorization{Binding: cloudassert.AuthorizationBinding(session.Hash, organization, identity.SessionID), Organization: organization, Identity: identity}
	if _, err := tx.ExecContext(ctx, "INSERT INTO authorizations(binding,session_hash,organization_id,identity_json,created_at,expires_at) VALUES (?,?,?,?,?,?) ON CONFLICT(binding) DO UPDATE SET identity_json = excluded.identity_json, expires_at = excluded.expires_at, revoked_at = NULL",
		result.Binding, session.Hash, organization, string(encoded), now, formatTime(identity.ExpiresAt)); err != nil {
		return authorization{}, nil, err
	}
	var stale []authorization
	for _, previous := range replaced {
		if previous.Binding != result.Binding {
			stale = append(stale, previous)
		}
	}
	return result, stale, tx.Commit()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func activeAuthorizations(ctx context.Context, query queryer, statement string, args ...any) ([]authorization, error) {
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []authorization
	for rows.Next() {
		var item authorization
		var encoded, expires string
		if err := rows.Scan(&item.Binding, &item.Organization, &encoded, &expires); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &item.Identity); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (a *authStore) authorization(ctx context.Context, session accountSession, organization string) (authorization, error) {
	items, err := activeAuthorizations(ctx, a.store.db, "SELECT binding,organization_id,identity_json,expires_at FROM authorizations WHERE session_hash = ? AND organization_id = ? AND revoked_at IS NULL", session.Hash, organization)
	if err != nil || len(items) != 1 || !items[0].Identity.ExpiresAt.After(a.now()) || items[0].Identity.Subject != session.Subject {
		return authorization{}, errNoSession
	}
	return items[0], nil
}

func (a *authStore) revokeAuthorization(ctx context.Context, binding string) error {
	_, err := a.store.db.ExecContext(ctx, "UPDATE authorizations SET revoked_at = ? WHERE binding = ? AND revoked_at IS NULL", formatTime(a.now()), binding)
	return err
}

func (a *authStore) revokeSession(ctx context.Context, hash string) ([]authorization, error) {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	items, err := activeAuthorizations(ctx, tx, "SELECT binding,organization_id,identity_json,expires_at FROM authorizations WHERE session_hash = ? AND revoked_at IS NULL", hash)
	if err != nil {
		return nil, err
	}
	now := formatTime(a.now())
	if _, err := tx.ExecContext(ctx, "UPDATE authorizations SET revoked_at = ? WHERE session_hash = ? AND revoked_at IS NULL", now, hash); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL", now, hash); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}

type loginTransaction struct {
	ID                     string
	State                  string
	Verifier               string
	Organization           string
	ReturnPath             string
	InvitationToken        string
	InvitationOrganization string
}

func (a *authStore) createTransaction(ctx context.Context, hash string, transaction loginTransaction) error {
	_, err := a.store.db.ExecContext(ctx, "INSERT INTO transactions(token_hash,transaction_id,state,verifier,organization_id,return_path,invitation_token,invitation_organization,expires_at) VALUES (?,?,?,?,?,?,?,?,?)",
		hash, transaction.ID, transaction.State, transaction.Verifier, transaction.Organization, transaction.ReturnPath, transaction.InvitationToken, transaction.InvitationOrganization, formatTime(a.now().Add(10*time.Minute)))
	return err
}

func (a *authStore) consumeTransaction(ctx context.Context, hash, id string) (loginTransaction, error) {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return loginTransaction{}, err
	}
	defer tx.Rollback()
	var result loginTransaction
	var expires string
	var consumed sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT transaction_id,state,verifier,organization_id,return_path,invitation_token,invitation_organization,expires_at,consumed_at FROM transactions WHERE token_hash = ? AND transaction_id = ?", hash, id).
		Scan(&result.ID, &result.State, &result.Verifier, &result.Organization, &result.ReturnPath, &result.InvitationToken, &result.InvitationOrganization, &expires, &consumed)
	if err != nil || consumed.Valid {
		return loginTransaction{}, errNoSession
	}
	expiry, err := parseTime(expires)
	if err != nil || !expiry.After(a.now()) {
		return loginTransaction{}, errNoSession
	}
	if _, err := tx.ExecContext(ctx, "UPDATE transactions SET consumed_at = ?, invitation_token = '' WHERE token_hash = ?", formatTime(a.now()), hash); err != nil {
		return loginTransaction{}, err
	}
	return result, tx.Commit()
}

func (a *authStore) audit(ctx context.Context, subject, organization, event string) error {
	_, err := a.store.db.ExecContext(ctx, "INSERT INTO audit(subject,organization_id,event,recorded_at) VALUES (?,?,?,?)", subject, organization, event, formatTime(a.now()))
	return err
}
