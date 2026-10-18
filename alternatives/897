package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type oauthGrant struct {
	ID             string  `json:"id"`
	ClientID       string  `json:"client_id"`
	ClientName     string  `json:"client_name"`
	OrganizationID string  `json:"organization_id"`
	ConnectedAt    string  `json:"connected_at"`
	LastUsedAt     *string `json:"last_used_at"`
	RevokedAt      *string `json:"revoked_at"`
}

type oauthGrantPage struct {
	Grants     []oauthGrant `json:"grants"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

func (s *Service) oauthGrants(ctx context.Context, owner, cursor string) (oauthGrantPage, error) {
	rows, err := s.auth.store.reader.QueryContext(ctx, `SELECT g.id,g.client_id,c.name,g.organization_id,g.created_at,g.last_used_at,g.revoked_at FROM oauth_grants g JOIN oauth_clients c ON c.id=g.client_id WHERE g.owner_subject=? AND g.id>? ORDER BY g.id LIMIT 201`, owner, cursor)
	if err != nil {
		return oauthGrantPage{}, err
	}
	defer rows.Close()
	grants := []oauthGrant{}
	for rows.Next() {
		var g oauthGrant
		if err := rows.Scan(&g.ID, &g.ClientID, &g.ClientName, &g.OrganizationID, &g.ConnectedAt, &g.LastUsedAt, &g.RevokedAt); err != nil {
			return oauthGrantPage{}, err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return oauthGrantPage{}, err
	}
	page := oauthGrantPage{Grants: grants}
	if len(grants) > 200 {
		page.Grants = grants[:200]
		page.NextCursor = page.Grants[199].ID
	}
	return page, nil
}

func (s *Service) revokeOAuthGrant(ctx context.Context, owner, id string) error {
	result, err := s.auth.store.db.ExecContext(ctx, "UPDATE oauth_grants SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND owner_subject=?", formatTime(s.config.now()), id, owner)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Service) oauthGrantsJSON(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return c.NoContent(http.StatusUnauthorized)
	}
	cursor := c.QueryParam("cursor")
	if cursor != "" && !safeID(cursor) {
		return oauthError(c, 400, "invalid_request")
	}
	grants, err := s.oauthGrants(c.Request().Context(), session.Subject, cursor)
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	return c.JSON(http.StatusOK, grants)
}

func (s *Service) oauthRevokeJSON(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return c.NoContent(http.StatusForbidden)
	}
	if err = s.revokeOAuthGrant(c.Request().Context(), session.Subject, c.Param("grant")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.NoContent(http.StatusNotFound)
		}
		return oauthError(c, 503, "server_error")
	}
	return c.NoContent(http.StatusNoContent)
}

func (e accessKeyExecutor) executeOAuthGrant(ctx context.Context, key accessKey, call operatortool.Call) (operatortool.Result, error) {
	if key.Kind != "personal" {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if call.Name == operatortool.OAuthGrantList {
		var input operatortool.OAuthGrantListArguments
		if err := operatortool.DecodeArguments(call.Arguments, &input); err != nil {
			return operatortool.Result{}, err
		}
		if input.Cursor != "" && !safeID(input.Cursor) {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		grants, err := e.service.oauthGrants(ctx, key.Owner, input.Cursor)
		if err != nil {
			return operatortool.Result{}, err
		}
		return operatortool.EncodeResult(grants)
	}
	if !apikey.HasScope([]string{string(key.Permission)}, apikey.ScopeWrite) {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	var input operatortool.OAuthGrantArguments
	if err := operatortool.DecodeArguments(call.Arguments, &input); err != nil {
		return operatortool.Result{}, err
	}
	if input.GrantID == "" {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if err := e.service.revokeOAuthGrant(ctx, key.Owner, input.GrantID); err != nil {
		return operatortool.Result{}, err
	}
	return operatortool.EncodeResult(map[string]bool{"revoked": true})
}
