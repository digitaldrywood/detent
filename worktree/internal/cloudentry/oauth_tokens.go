package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
)

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

type oauthRefreshToken struct {
	Grant     string    `json:"g"`
	ExpiresAt time.Time `json:"e"`
}

func (s *Service) exchangeOAuthToken(ctx context.Context, client string, form url.Values) (oauthTokenResponse, error) {
	if form.Get("grant_type") != "authorization_code" && form.Get("grant_type") != "refresh_token" {
		return oauthTokenResponse{}, errNoSession
	}
	now := s.config.now()
	var grant, challenge, redirect, resource, codeExpires string
	var consumed sql.NullString
	hash := apikey.HashToken(form.Get("code"))
	if form.Get("grant_type") == "authorization_code" {
		err := s.auth.store.reader.QueryRowContext(ctx, `SELECT grant_id,challenge,redirect_uri,resource,code_expires_at,consumed_at FROM oauth_requests WHERE code_hash=? AND client_id=?`, hash, client).Scan(&grant, &challenge, &redirect, &resource, &codeExpires, &consumed)
		if err != nil {
			return oauthTokenResponse{}, err
		}
		expiry, err := parseTime(codeExpires)
		verifier := form.Get("code_verifier")
		digest := oauthPKCE(verifier)
		if err != nil || consumed.Valid || !expiry.After(now) || redirect != form.Get("redirect_uri") || resource != form.Get("resource") || !validOAuthVerifier(verifier) || digest != challenge {
			return oauthTokenResponse{}, errNoSession
		}
	} else {
		var token oauthRefreshToken
		if err := s.openOAuth("oauth-refresh", form.Get("refresh_token"), &token); err != nil || !token.ExpiresAt.After(now) {
			return oauthTokenResponse{}, errNoSession
		}
		grant = token.Grant
		hash = apikey.HashToken(form.Get("refresh_token"))
		err := s.auth.store.reader.QueryRowContext(ctx, `SELECT t.expires_at FROM oauth_refresh_tokens t JOIN oauth_grants g ON g.id=t.grant_id WHERE t.grant_id=? AND g.client_id=? AND g.resource=?`, grant, client, form.Get("resource")).Scan(&codeExpires)
		if err != nil {
			return oauthTokenResponse{}, err
		}
		expiry, err := parseTime(codeExpires)
		if err != nil || !expiry.After(now) {
			return oauthTokenResponse{}, errNoSession
		}
	}
	key, err := s.oauthKey(ctx, "id", grant, false)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	organization, err := s.readyOrganization(ctx, key.Organizations[0].OrganizationID)
	if err != nil {
		return oauthTokenResponse{}, errNoSession
	}
	if _, err := s.keyClaims(ctx, key, organization, "POST", "/internal/v1/keys/catalog", nil); err != nil {
		return oauthTokenResponse{}, errNoSession
	}
	access, err := s.config.generateToken()
	if err != nil {
		return oauthTokenResponse{}, err
	}
	access = oauthAccessPrefix + access
	refreshExpires := now.Add(oauthRefreshLifetime)
	refresh, err := s.sealOAuth("oauth-refresh", oauthRefreshToken{Grant: grant, ExpiresAt: refreshExpires})
	if err != nil {
		return oauthTokenResponse{}, err
	}
	tx, err := s.auth.store.db.BeginTx(ctx, nil)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	defer tx.Rollback()
	var result sql.Result
	if form.Get("grant_type") == "authorization_code" {
		result, err = tx.ExecContext(ctx, "UPDATE oauth_requests SET consumed_at=? WHERE code_hash=? AND client_id=? AND consumed_at IS NULL AND julianday(code_expires_at)>julianday(?)", formatTime(now), hash, client, formatTime(now))
	} else {
		result, err = tx.ExecContext(ctx, "UPDATE oauth_refresh_tokens SET token_hash=?,expires_at=? WHERE grant_id=? AND token_hash=? AND julianday(expires_at)>julianday(?)", apikey.HashToken(refresh), formatTime(refreshExpires), grant, hash, formatTime(now))
	}
	if err != nil {
		return oauthTokenResponse{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return oauthTokenResponse{}, err
	}
	if count != 1 {
		if form.Get("grant_type") == "refresh_token" {
			if _, err = tx.ExecContext(ctx, "UPDATE oauth_grants SET revoked_at=? WHERE id=? AND revoked_at IS NULL", formatTime(now), grant); err != nil {
				return oauthTokenResponse{}, err
			}
			if err = tx.Commit(); err != nil {
				return oauthTokenResponse{}, err
			}
		}
		return oauthTokenResponse{}, errNoSession
	}
	result, err = tx.ExecContext(ctx, `UPDATE oauth_grants SET access_hash=?,access_expires_at=? WHERE id=? AND revoked_at IS NULL AND client_id=? AND resource=?`, apikey.HashToken(access), formatTime(now.Add(oauthAccessLifetime)), grant, client, form.Get("resource"))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	count, err = result.RowsAffected()
	if err != nil || count != 1 {
		return oauthTokenResponse{}, errNoSession
	}
	if form.Get("grant_type") == "authorization_code" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO oauth_refresh_tokens(token_hash,grant_id,expires_at) VALUES(?,?,?)", apikey.HashToken(refresh), grant, formatTime(refreshExpires)); err != nil {
			return oauthTokenResponse{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return oauthTokenResponse{}, err
	}
	return oauthTokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(oauthAccessLifetime / time.Second), RefreshToken: refresh, Scope: oauthScope}, nil
}

func (s *Service) oauthKey(ctx context.Context, column, value string, requireAccess bool) (accessKey, error) {
	if column != "id" && column != "access_hash" {
		return accessKey{}, errNoSession
	}
	var k accessKey
	var organization, resource, created string
	var expires, hash sql.NullString
	err := s.auth.store.reader.QueryRowContext(ctx, `SELECT g.id,c.name,g.owner_subject,g.owner_email,g.organization_id,g.permission,g.resource,g.created_at,g.access_hash,g.access_expires_at FROM oauth_grants g JOIN oauth_clients c ON c.id=g.client_id WHERE g.`+column+`=? AND g.revoked_at IS NULL`, value).Scan(&k.ID, &k.Name, &k.Owner, &k.OwnerEmail, &organization, &k.Permission, &resource, &created, &hash, &expires)
	if err != nil {
		return k, err
	}
	if resource != s.config.PublicURL+"/mcp" {
		return k, errNoSession
	}
	k.CreatedAt, err = parseTime(created)
	if err != nil || k.CreatedAt.After(s.config.now()) {
		return k, errNoSession
	}
	if requireAccess {
		if !expires.Valid || !hash.Valid {
			return k, errNoSession
		}
		at, err := parseTime(expires.String)
		if err != nil || !at.After(s.config.now()) {
			return k, errNoSession
		}
		k.ExpiresAt = &at
	}
	k.Hash = hash.String
	k.Kind = "personal"
	k.OAuth = true
	k.AccessContext = "selected"
	k.Organizations = []keyOrganization{{OrganizationID: organization, ProjectContext: apikey.ProjectContext{Access: "all"}}}
	org, err := s.readyOrganization(ctx, organization)
	if err != nil {
		return k, errNoSession
	}
	if _, err = s.keyMembership(ctx, k.Owner, org); err != nil {
		return k, errNoSession
	}
	return k, nil
}

func (s *Service) activeMCPKey(ctx context.Context, hash string) (accessKey, error) {
	k, err := s.activeAccessKey(ctx, hash)
	if !errors.Is(err, sql.ErrNoRows) {
		return k, err
	}
	return s.oauthKey(ctx, "access_hash", hash, true)
}

func (s *Service) recordCredentialUse(ctx context.Context, key accessKey, organization string) error {
	if !key.OAuth {
		return s.recordKeyUse(ctx, key.ID, organization)
	}
	_, err := s.auth.store.db.ExecContext(ctx, "UPDATE oauth_grants SET last_used_at=? WHERE id=? AND revoked_at IS NULL", formatTime(s.config.now()), key.ID)
	return err
}

func oauthGrantID(id string) bool { return strings.HasPrefix(id, "oauth_grant_") }
