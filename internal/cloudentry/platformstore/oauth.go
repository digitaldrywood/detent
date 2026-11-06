package platformstore

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) CreateOAuthClient(ctx context.Context, c OAuthClient) error {
	if c.ID == "" || c.Name == "" || len(c.RedirectURIs) == 0 || len(c.RedirectURIs) > 200 || c.AuthMethod != "none" && c.AuthMethod != "client_secret_post" && c.AuthMethod != "client_secret_basic" {
		return ErrInvalid
	}
	if c.AuthMethod != "none" && c.SecretHash == "" {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, false, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO platform.oauth_clients(id,name,auth_method,secret_hash,created_at) VALUES($1,$2,$3,$4,$5)`, c.ID, c.Name, c.AuthMethod, c.SecretHash, r.now()); err != nil {
			return err
		}
		for i, uri := range c.RedirectURIs {
			if _, err := tx.Exec(ctx, `INSERT INTO platform.oauth_client_redirect_uris(client_id,position,uri) VALUES($1,$2,$3)`, c.ID, i, uri); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repository) CreateOAuthGrant(ctx context.Context, g OAuthGrant, request OAuthRequest) error {
	if g.OrganizationID == "" || g.ID == "" || g.ClientID == "" || g.OwnerSubject == "" || g.Resource == "" || !apikey.ValidScope(apikey.Scope(g.Permission)) || request.ID == "" || request.CodeHash == "" || request.Challenge == "" || !request.CodeExpiresAt.After(r.now()) {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, false, func(tx pgx.Tx) error {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform.oauth_client_redirect_uris WHERE client_id=$1 AND uri=$2)`, g.ClientID, request.RedirectURI).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrInvalid
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.oauth_grants(organization_id,id,client_id,owner_subject,owner_email,permission,resource,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, g.OrganizationID, g.ID, g.ClientID, g.OwnerSubject, g.OwnerEmail, g.Permission, g.Resource, r.now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO platform.oauth_requests(organization_id,id,client_id,redirect_uri,challenge,resource,code_hash,code_expires_at,grant_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, g.OrganizationID, request.ID, g.ClientID, request.RedirectURI, request.Challenge, g.Resource, request.CodeHash, request.CodeExpiresAt, g.ID)
		return err
	})
}

func scanOAuthGrant(row scanner) (OAuthGrant, error) {
	var g OAuthGrant
	err := row.Scan(&g.OrganizationID, &g.ID, &g.ClientID, &g.OwnerSubject, &g.OwnerEmail, &g.Permission, &g.Resource, &g.CreatedAt, &g.AccessHash, &g.AccessExpiresAt, &g.RevokedAt)
	return g, err
}

const grantColumns = `organization_id,id,client_id,owner_subject,owner_email,permission,resource,created_at,access_hash,access_expires_at,revoked_at`

func (r *Repository) lockGrant(ctx context.Context, tx pgx.Tx, organization tracker.OrganizationID, id, client, resource string) (OAuthGrant, error) {
	g, err := scanOAuthGrant(tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM platform.oauth_grants WHERE organization_id=$1 AND id=$2 AND client_id=$3 AND resource=$4 AND revoked_at IS NULL FOR UPDATE`, organization, id, client, resource))
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthGrant{}, ErrNoSession
	}
	return g, err
}

func (r *Repository) ExchangeOAuthCode(ctx context.Context, organization tracker.OrganizationID, hash, client, resource, redirect, verifier string, tokens OAuthTokens) error {
	if tokens.AccessHash == "" || tokens.RefreshHash == "" || !tokens.AccessExpiresAt.After(r.now()) || !tokens.RefreshExpiresAt.After(r.now()) {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, false, func(tx pgx.Tx) error {
		var id, challenge string
		err := tx.QueryRow(ctx, `SELECT grant_id,challenge FROM platform.oauth_requests WHERE organization_id=$1 AND code_hash=$2 AND client_id=$3 AND resource=$4 AND redirect_uri=$5 AND consumed_at IS NULL AND code_expires_at>$6`, organization, hash, client, resource, redirect, r.now()).Scan(&id, &challenge)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(challenge), []byte(oauth2.S256ChallengeFromVerifier(verifier))) != 1 {
			return ErrNoSession
		}
		if _, err := r.lockGrant(ctx, tx, organization, id, client, resource); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE platform.oauth_requests SET consumed_at=$1 WHERE organization_id=$2 AND code_hash=$3 AND consumed_at IS NULL AND code_expires_at>$1`, r.now(), organization, hash)
		if err != nil {
			return err
		}
		n := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrNoSession
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.oauth_refresh_tokens(organization_id,token_hash,grant_id,expires_at) VALUES($1,$2,$3,$4)`, organization, tokens.RefreshHash, id, tokens.RefreshExpiresAt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE platform.oauth_grants SET access_hash=$1,access_expires_at=$2 WHERE organization_id=$3 AND id=$4`, tokens.AccessHash, tokens.AccessExpiresAt, organization, id)
		return err
	})
}

func (r *Repository) RotateOAuthRefresh(ctx context.Context, organization tracker.OrganizationID, hash, client, resource string, tokens OAuthTokens) error {
	if tokens.AccessHash == "" || tokens.RefreshHash == "" || tokens.RefreshHash == hash || !tokens.AccessExpiresAt.After(r.now()) || !tokens.RefreshExpiresAt.After(r.now()) {
		return ErrInvalid
	}
	replay := false
	err := r.transaction(ctx, corestore.Serving, false, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `SELECT grant_id FROM platform.oauth_refresh_tokens WHERE organization_id=$1 AND token_hash=$2 UNION ALL SELECT grant_id FROM platform.oauth_spent_refresh_tokens WHERE organization_id=$1 AND token_hash=$2 LIMIT 1`, organization, hash).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		if _, err := r.lockGrant(ctx, tx, organization, id, client, resource); err != nil {
			return err
		}
		var reused bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform.oauth_spent_refresh_tokens WHERE token_hash=$1)`, tokens.RefreshHash).Scan(&reused); err != nil {
			return err
		}
		if reused {
			return ErrInvalid
		}

		var current string
		var expires pgtype.Timestamptz
		err = tx.QueryRow(ctx, `SELECT token_hash,expires_at FROM platform.oauth_refresh_tokens WHERE organization_id=$1 AND grant_id=$2`, organization, id).Scan(&current, &expires)
		if err != nil {
			return err
		}
		if current != hash || !expires.Time.After(r.now()) {
			replay = true
			_, err := tx.Exec(ctx, `UPDATE platform.oauth_grants SET revoked_at=$1 WHERE organization_id=$2 AND id=$3`, r.now(), organization, id)
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.oauth_spent_refresh_tokens(organization_id,token_hash,grant_id,expires_at) VALUES($1,$2,$3,$4)`, organization, hash, id, expires.Time); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.oauth_refresh_tokens SET token_hash=$1,expires_at=$2 WHERE organization_id=$3 AND grant_id=$4`, tokens.RefreshHash, tokens.RefreshExpiresAt, organization, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE platform.oauth_grants SET access_hash=$1,access_expires_at=$2 WHERE organization_id=$3 AND id=$4`, tokens.AccessHash, tokens.AccessExpiresAt, organization, id)
		return err
	})
	if err == nil && replay {
		return ErrNoSession
	}
	return err
}

func (r *Repository) OAuthGrant(ctx context.Context, organization tracker.OrganizationID, accessHash string) (OAuthGrant, error) {
	var g OAuthGrant
	err := r.transaction(ctx, corestore.Serving, true, func(tx pgx.Tx) error {
		var err error
		g, err = scanOAuthGrant(tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM platform.oauth_grants WHERE organization_id=$1 AND access_hash=$2 AND revoked_at IS NULL AND access_expires_at>$3`, organization, accessHash, r.now()))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		return err
	})
	return g, err
}

func (r *Repository) RevokeOAuthGrant(ctx context.Context, organization tracker.OrganizationID, owner, id string) error {
	return r.transaction(ctx, corestore.Serving, false, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE platform.oauth_grants SET revoked_at=$1 WHERE organization_id=$2 AND owner_subject=$3 AND id=$4 AND revoked_at IS NULL`, r.now(), organization, owner, id)
		return err
	})
}
