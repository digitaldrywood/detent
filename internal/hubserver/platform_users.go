package hubserver

import (
	"context"
	"encoding/json"
	"errors"
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
		return s.invalidAPIRequest(c, err)
	}
	query := platformusers.Parse(input.Text)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	result := platformusers.TenantResult{Members: []platformusers.TenantMember{}, Invitations: []platformusers.TenantInvitation{}}
	rows, err := s.database.reader.QueryContext(ctx, `WITH users AS (
SELECT 'm:' || user_id AS cursor,email,user_id AS subject,role,created_at AS at FROM hosted_members WHERE active=1
UNION ALL
SELECT 'i:' || id,email,'','',expires_at FROM hosted_invitations WHERE organization_id=? AND accepted_user_id=''
)
SELECT cursor,email,subject,role,at FROM users
WHERE cursor>? AND CASE WHEN ? THEN lower(email)=? ELSE instr(lower(email),?)>0 END
ORDER BY cursor LIMIT ?`, s.config.Hosted.OrganizationID, input.After, query.Exact, query.Text, query.Text, platformusers.Limit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	processed := 0
	more := false
	for rows.Next() {
		if processed == platformusers.Limit {
			more = true
			break
		}
		var cursor, email, subject, role, at string
		if err := rows.Scan(&cursor, &email, &subject, &role, &at); err != nil {
			return err
		}
		previous := result
		if cursor[:2] == "m:" {
			result.Members = append(result.Members, platformusers.TenantMember{Email: email, Subject: subject, Role: role, JoinedAt: at})
		} else {
			if at == "" {
				pending, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, cursor[2:])
				if err != nil {
					return err
				}
				at = formatHubTime(pending.ExpiresAt)
			}
			expires, err := time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return err
			}
			if expires.After(s.config.now()) {
				result.Invitations = append(result.Invitations, platformusers.TenantInvitation{Email: email, ExpiresAt: at})
			}
		}
		result.NextCursor = cursor
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if len(body) > 60<<10 {
			if processed == 0 {
				return errors.New("platform user exceeds tenant page limit")
			}
			result = previous
			more = true
			break
		}
		processed++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !more {
		result.NextCursor = ""
	}
	return c.JSON(http.StatusOK, result)
}
