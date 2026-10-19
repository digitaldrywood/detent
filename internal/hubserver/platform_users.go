package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/platformusers"
)

func (s *Service) hostedPlatformUsers(c echo.Context) error {
	if claims, ok := hostedSharedClaims(c); !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input platformusers.Query
	if err := decodeAPIJSON(c, &input); err != nil {
		return invalidAPIRequest(c, err)
	}
	query := platformusers.Parse(input.Text)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	result := platformusers.TenantResult{Members: []platformusers.TenantMember{}, Invitations: []platformusers.TenantInvitation{}}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT email,user_id,role,created_at FROM hosted_members
WHERE active=1 AND CASE WHEN ? THEN lower(email)=? ELSE instr(lower(email),?)>0 END ORDER BY lower(email),user_id`, query.Exact, query.Text, query.Text)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var member platformusers.TenantMember
		if err := rows.Scan(&member.Email, &member.Subject, &member.Role, &member.JoinedAt); err != nil {
			return err
		}
		result.Members = append(result.Members, member)
	}
	err = rows.Err()
	if err != nil {
		return err
	}
	rows, err = s.database.reader.QueryContext(ctx, `SELECT id,email,expires_at FROM hosted_invitations
WHERE organization_id=? AND accepted_user_id='' AND CASE WHEN ? THEN lower(email)=? ELSE instr(lower(email),?)>0 END ORDER BY lower(email),id`, s.config.Hosted.OrganizationID, query.Exact, query.Text, query.Text)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var invitation platformusers.TenantInvitation
		if err := rows.Scan(&id, &invitation.Email, &invitation.ExpiresAt); err != nil {
			return err
		}
		if invitation.ExpiresAt == "" {
			pending, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, id)
			if err != nil {
				return err
			}
			invitation.ExpiresAt = formatHubTime(pending.ExpiresAt)
		}
		expires, err := time.Parse(time.RFC3339Nano, invitation.ExpiresAt)
		if err != nil {
			return err
		}
		if expires.After(s.config.now()) {
			result.Invitations = append(result.Invitations, invitation)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, result)
}
