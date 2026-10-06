package hubserver

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/platformaccounts"
)

func (s *Service) hostedPlatformAccounts(c echo.Context) error {
	if claims, ok := hostedSharedClaims(c); !ok || claims.Kind != cloudassert.KindService {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var input platformaccounts.Query
	if err := decodeAPIJSON(c, &input); err != nil {
		return invalidAPIRequest(c, err)
	}
	query, err := platformaccounts.Parse(input.Text)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apiErrorResponse{Code: "invalid_query", Message: "Enter at least three characters"})
	}
	ctx := c.Request().Context()
	result := platformaccounts.TenantResult{Members: []platformaccounts.TenantMember{}, Invitations: []platformaccounts.TenantInvitation{}}
	rows, err := s.database.db.QueryContext(ctx, `SELECT email,user_id,role,created_at FROM hosted_members
WHERE active=1 AND CASE WHEN ? THEN lower(email)=? ELSE instr(lower(email),?)>0 END ORDER BY lower(email),user_id`, query.Exact, query.Text, query.Text)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var member platformaccounts.TenantMember
		if err := rows.Scan(&member.Email, &member.Subject, &member.Role, &member.JoinedAt); err != nil {
			return err
		}
		result.Members = append(result.Members, member)
	}
	err = rows.Err()
	if err != nil {
		return err
	}
	rows, err = s.database.db.QueryContext(ctx, `SELECT id,email,expires_at FROM hosted_invitations
WHERE organization_id=? AND accepted_user_id='' AND CASE WHEN ? THEN lower(email)=? ELSE instr(lower(email),?)>0 END ORDER BY lower(email),id`, s.config.Hosted.OrganizationID, query.Exact, query.Text, query.Text)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var invitation platformaccounts.TenantInvitation
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
	emails := make(map[string]bool)
	for _, member := range result.Members {
		emails[strings.ToLower(member.Email)] = true
	}
	for _, invitation := range result.Invitations {
		emails[strings.ToLower(invitation.Email)] = true
	}
	ordered := make([]string, 0, len(emails))
	for email := range emails {
		ordered = append(ordered, email)
	}
	sort.Strings(ordered)
	for _, email := range ordered[min(len(ordered), platformaccounts.Limit):] {
		delete(emails, email)
	}
	members := result.Members[:0]
	for _, member := range result.Members {
		if emails[strings.ToLower(member.Email)] {
			members = append(members, member)
		}
	}
	result.Members = members
	invitations := result.Invitations[:0]
	for _, invitation := range result.Invitations {
		if emails[strings.ToLower(invitation.Email)] {
			invitations = append(invitations, invitation)
		}
	}
	result.Invitations = invitations
	return c.JSON(http.StatusOK, result)
}
