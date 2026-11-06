package platformstore

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const sessionColumns = `token_hash,subject,email,csrf_secret,identity_subject,provider_organization_id,provider_session_id,identity_created_at,identity_expires_at,support_actor,support_reason,created_at,expires_at,revoked_at`
const authorizationColumns = `a.binding,a.session_hash,a.organization_id,a.identity_subject,a.provider_organization_id,a.provider_session_id,a.identity_created_at,a.identity_expires_at,a.support_actor,a.support_reason,a.support,a.effective_email,a.created_at,a.expires_at,a.revoked_at`

func scanSession(row scanner) (Session, error) {
	var s Session
	err := row.Scan(&s.Hash, &s.Subject, &s.Email, &s.CSRFSecret, &s.Identity.Subject, &s.Identity.OrganizationID, &s.Identity.SessionID, &s.Identity.CreatedAt, &s.Identity.ExpiresAt, &s.Identity.SupportActor, &s.Identity.SupportReason, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt)
	return s, err
}

func scanAuthorization(row scanner) (Authorization, error) {
	var a Authorization
	err := row.Scan(&a.Binding, &a.SessionHash, &a.OrganizationID, &a.Identity.Subject, &a.Identity.OrganizationID, &a.Identity.SessionID, &a.Identity.CreatedAt, &a.Identity.ExpiresAt, &a.Identity.SupportActor, &a.Identity.SupportReason, &a.Support, &a.EffectiveEmail, &a.CreatedAt, &a.ExpiresAt, &a.RevokedAt)
	return a, err
}

func (r *Repository) CreateSession(ctx context.Context, s Session) error {
	if s.Hash == "" || s.Subject == "" || s.Subject != s.Identity.Subject || s.Identity.SessionID == "" || s.CSRFSecret == "" || !s.Identity.ExpiresAt.After(r.now()) {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO platform.sessions(`+sessionColumns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$9,NULL)`, s.Hash, s.Subject, s.Email, s.CSRFSecret, s.Identity.Subject, s.Identity.OrganizationID, s.Identity.SessionID, s.Identity.CreatedAt, s.Identity.ExpiresAt, s.Identity.SupportActor, s.Identity.SupportReason, r.now())
		return err
	})
}

func (r *Repository) Session(ctx context.Context, hash string) (Session, error) {
	result, err := r.StoredSession(ctx, hash)
	if err == nil && !result.ExpiresAt.After(r.now()) {
		return Session{}, ErrNoSession
	}
	return result, err
}

func (r *Repository) StoredSession(ctx context.Context, hash string) (Session, error) {
	var result Session
	err := r.transaction(ctx, corestore.Serving, true, func(tx *sql.Tx) error {
		var err error
		result, err = scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM platform.sessions WHERE token_hash=$1 AND revoked_at IS NULL`, hash))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return Session{}, ErrNoSession
	}
	return result, err
}

func (r *Repository) lockSession(ctx context.Context, tx *sql.Tx, hash string) (Session, error) {
	s, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM platform.sessions WHERE token_hash=$1 FOR UPDATE`, hash))
	if errors.Is(err, sql.ErrNoRows) || err == nil && (s.RevokedAt != nil || !s.ExpiresAt.After(r.now())) {
		return Session{}, ErrNoSession
	}
	return s, err
}

func (r *Repository) Authorize(ctx context.Context, hash string, organization tracker.OrganizationID, identity auth.HostedIdentity, tokens auth.HostedTokens, effectiveEmail string) (Authorization, *Authorization, error) {
	result := Authorization{Binding: cloudassert.AuthorizationBinding(hash, string(organization), identity.SessionID), SessionHash: hash, OrganizationID: organization, Identity: identity, Support: identity.SupportActor != "", EffectiveEmail: effectiveEmail, CreatedAt: r.now(), ExpiresAt: identity.ExpiresAt}
	if organization == "" || identity.Subject == "" || identity.SessionID == "" || !identity.ExpiresAt.After(r.now()) {
		return Authorization{}, nil, ErrInvalid
	}
	access, err := r.secrets.seal("provider-access", result.Binding, tokens.AccessToken)
	if err != nil {
		return Authorization{}, nil, err
	}
	refresh, err := r.secrets.seal("provider-refresh", result.Binding, tokens.RefreshToken)
	if err != nil {
		return Authorization{}, nil, err
	}
	var replaced *Authorization
	err = r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		s, err := r.lockSession(ctx, tx, hash)
		if err != nil {
			return err
		}
		if !identity.ExpiresAt.After(r.now()) {
			return ErrNoSession
		}
		if result.Support && !strings.EqualFold(identity.SupportActor, s.Email) || !result.Support && identity.Subject != s.Subject {
			return ErrNoSession
		}
		var provider string
		if err := tx.QueryRowContext(ctx, `SELECT provider_id FROM platform.organizations WHERE id=$1 AND state='ready'`, organization).Scan(&provider); errors.Is(err, sql.ErrNoRows) {
			return ErrNoSession
		} else if err != nil {
			return err
		}
		if identity.OrganizationID != provider {
			return ErrNoSession
		}
		previous, err := scanAuthorization(tx.QueryRowContext(ctx, `SELECT `+authorizationColumns+` FROM platform.authorizations a WHERE a.organization_id=$1 AND a.session_hash=$2 AND a.revoked_at IS NULL`, organization, hash))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && previous.Binding != result.Binding {
			if _, err := tx.ExecContext(ctx, `UPDATE platform.authorizations SET revoked_at=$1 WHERE organization_id=$2 AND binding=$3`, r.now(), organization, previous.Binding); err != nil {
				return err
			}
			replaced = &previous
		}
		stored, err := tx.ExecContext(ctx, `INSERT INTO platform.authorizations(binding,session_hash,organization_id,identity_subject,provider_organization_id,provider_session_id,identity_created_at,identity_expires_at,support_actor,support_reason,created_at,expires_at,support,effective_email,access_token,refresh_token)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$8,$12,$13,$14,$15)
 ON CONFLICT(organization_id,binding) DO UPDATE SET identity_subject=excluded.identity_subject,provider_organization_id=excluded.provider_organization_id,provider_session_id=excluded.provider_session_id,identity_created_at=excluded.identity_created_at,identity_expires_at=excluded.identity_expires_at,support_actor=excluded.support_actor,support_reason=excluded.support_reason,expires_at=excluded.expires_at,support=excluded.support,effective_email=excluded.effective_email,access_token=excluded.access_token,refresh_token=excluded.refresh_token
 WHERE platform.authorizations.revoked_at IS NULL AND platform.authorizations.session_hash=excluded.session_hash`, result.Binding, hash, organization, identity.Subject, identity.OrganizationID, identity.SessionID, identity.CreatedAt, identity.ExpiresAt, identity.SupportActor, identity.SupportReason, result.CreatedAt, result.Support, effectiveEmail, access, refresh)
		if err != nil {
			return err
		}
		count, err := stored.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrNoSession
		}
		return nil
	})
	if err != nil {
		return Authorization{}, nil, err
	}
	return result, replaced, nil
}

func (r *Repository) Authorization(ctx context.Context, hash string, organization tracker.OrganizationID) (Authorization, error) {
	var result Authorization
	err := r.transaction(ctx, corestore.Serving, true, func(tx *sql.Tx) error {
		var err error
		result, err = scanAuthorization(tx.QueryRowContext(ctx, `SELECT `+authorizationColumns+` FROM platform.authorizations a JOIN platform.sessions s ON s.token_hash=a.session_hash JOIN platform.organizations tenant ON tenant.id=a.organization_id AND tenant.provider_id=a.provider_organization_id AND tenant.state='ready' WHERE a.organization_id=$1 AND a.session_hash=$2 AND a.revoked_at IS NULL AND a.expires_at>$3 AND s.revoked_at IS NULL AND s.expires_at>$3 AND ((a.support AND lower(a.support_actor)=lower(s.email)) OR (NOT a.support AND a.identity_subject=s.subject))`, organization, hash, r.now()))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return Authorization{}, ErrNoSession
	}
	return result, err
}

func (r *Repository) RevokeSession(ctx context.Context, hash string) error {
	return r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE platform.sessions SET revoked_at=$1 WHERE token_hash=$2 AND revoked_at IS NULL`, r.now(), hash)
		return err
	})
}

func (r *Repository) RevokeAuthorization(ctx context.Context, hash string, organization tracker.OrganizationID, binding string) error {
	return r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		if _, err := r.lockSession(ctx, tx, hash); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE platform.authorizations SET revoked_at=$1 WHERE organization_id=$2 AND session_hash=$3 AND binding=$4 AND revoked_at IS NULL`, r.now(), organization, hash, binding)
		return err
	})
}

func (r *Repository) Tokens(ctx context.Context, hash string, organization tracker.OrganizationID, binding string) (auth.HostedTokens, error) {
	var tokens auth.HostedTokens
	err := r.transaction(ctx, corestore.Serving, true, func(tx *sql.Tx) error {
		var access, refresh string
		err := tx.QueryRowContext(ctx, `SELECT a.access_token,a.refresh_token FROM platform.authorizations a JOIN platform.sessions s ON s.token_hash=a.session_hash JOIN platform.organizations tenant ON tenant.id=a.organization_id AND tenant.provider_id=a.provider_organization_id AND tenant.state='ready' WHERE a.organization_id=$1 AND a.session_hash=$2 AND a.binding=$3 AND a.revoked_at IS NULL AND a.expires_at>$4 AND s.revoked_at IS NULL AND s.expires_at>$4 AND ((a.support AND lower(a.support_actor)=lower(s.email)) OR (NOT a.support AND a.identity_subject=s.subject))`, organization, hash, binding, r.now()).Scan(&access, &refresh)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		tokens.AccessToken, err = r.secrets.open("provider-access", binding, access)
		if err != nil {
			return err
		}
		tokens.RefreshToken, err = r.secrets.open("provider-refresh", binding, refresh)
		return err
	})
	if err != nil {
		return auth.HostedTokens{}, err
	}
	return tokens, nil
}

func (r *Repository) RotateTokens(ctx context.Context, hash string, organization tracker.OrganizationID, binding string, previous, next auth.HostedTokens) error {
	access, err := r.secrets.seal("provider-access", binding, next.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := r.secrets.seal("provider-refresh", binding, next.RefreshToken)
	if err != nil {
		return err
	}
	return r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		if _, err := r.lockSession(ctx, tx, hash); err != nil {
			return err
		}
		var storedAccess, storedRefresh string
		err := tx.QueryRowContext(ctx, `SELECT a.access_token,a.refresh_token FROM platform.authorizations a JOIN platform.sessions s ON s.token_hash=a.session_hash JOIN platform.organizations tenant ON tenant.id=a.organization_id AND tenant.provider_id=a.provider_organization_id AND tenant.state='ready' WHERE a.organization_id=$1 AND a.session_hash=$2 AND a.binding=$3 AND a.revoked_at IS NULL AND a.expires_at>$4 AND ((a.support AND lower(a.support_actor)=lower(s.email)) OR (NOT a.support AND a.identity_subject=s.subject)) FOR UPDATE OF a`, organization, hash, binding, r.now()).Scan(&storedAccess, &storedRefresh)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		oldAccess, err := r.secrets.open("provider-access", binding, storedAccess)
		if err != nil {
			return err
		}
		oldRefresh, err := r.secrets.open("provider-refresh", binding, storedRefresh)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(previous.AccessToken), []byte(oldAccess)) != 1 || subtle.ConstantTimeCompare([]byte(previous.RefreshToken), []byte(oldRefresh)) != 1 {
			return corestore.ErrRevisionConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE platform.authorizations SET access_token=$1,refresh_token=$2 WHERE organization_id=$3 AND session_hash=$4 AND binding=$5`, access, refresh, organization, hash, binding)
		return err
	})
}
