package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Service) exchangeOAuthToken(ctx context.Context, client string, form url.Values) (_ oauthTokenResponse, resultErr error) {
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
		hash = apikey.HashToken(form.Get("refresh_token"))
		err := s.auth.store.reader.QueryRowContext(ctx, `SELECT t.grant_id,t.expires_at FROM (
			SELECT grant_id,expires_at FROM oauth_refresh_tokens WHERE token_hash=?
			UNION ALL
			SELECT grant_id,expires_at FROM oauth_spent_refresh_tokens WHERE token_hash=?
		) t JOIN oauth_grants g ON g.id=t.grant_id WHERE g.client_id=? AND g.resource=? AND g.revoked_at IS NULL`, hash, hash, client, form.Get("resource")).Scan(&grant, &codeExpires)
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
	refresh, err := s.config.generateToken()
	if err != nil {
		return oauthTokenResponse{}, err
	}
	refresh = "oauth_refresh_" + refresh

	tokens := platformstore.OAuthTokens{AccessHash: apikey.HashToken(access), RefreshHash: apikey.HashToken(refresh), AccessExpiresAt: now.Add(oauthAccessLifetime), RefreshExpiresAt: refreshExpires}
	repository := s.auth.store.db.postgres
	if form.Get("grant_type") == "authorization_code" {
		err = repository.ExchangeOAuthCode(ctx, tracker.OrganizationID(organization.ID), hash, client, form.Get("resource"), form.Get("redirect_uri"), form.Get("code_verifier"), tokens)
	} else {
		err = repository.RotateOAuthRefresh(ctx, tracker.OrganizationID(organization.ID), hash, client, form.Get("resource"), tokens)
	}
	if err != nil {
		return oauthTokenResponse{}, entryAuthError(err)
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
	query := `SELECT g.id,c.name,g.owner_subject,g.owner_email,g.organization_id,g.permission,g.resource,g.created_at,g.access_hash,g.access_expires_at FROM oauth_grants g JOIN oauth_clients c ON c.id=g.client_id WHERE g.` + column + `=? AND g.revoked_at IS NULL`
	args := []any{value}
	query += ` AND (SELECT count(*) FROM oauth_grants duplicate WHERE duplicate.` + column + `=?)=1`
	args = append(args, value)
	err := s.auth.store.reader.QueryRowContext(ctx, query, args...).Scan(&k.ID, &k.Name, &k.Owner, &k.OwnerEmail, &organization, &k.Permission, &resource, &created, &hash, &expires)
	if err != nil {
		return k, err
	}
	if resource != s.config.PublicURL+"/mcp" {
		return k, errNoSession
	}
	k.CreatedAt, err = parseTime(created)
	if err != nil {
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
