package cloudentry

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/platformaccounts"
)

func (s *Service) platformAccountsJSON(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "Sign in to continue"})
	}
	ctx := c.Request().Context()
	if !s.platformStaff(ctx, session) {
		return c.JSON(http.StatusForbidden, map[string]string{"code": "forbidden", "message": "The platform console is limited to Detent staff"})
	}
	query, err := platformaccounts.Parse(c.QueryParam("q"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_query", "message": "Enter at least three characters"})
	}
	if _, err := s.auth.store.db.ExecContext(ctx, "INSERT INTO audit(subject,organization_id,event,recorded_at,query_length) VALUES(?,'','platform_accounts_searched',?,?)", session.Subject, formatTime(s.auth.now()), utf8.RuneCountInString(query.Text)); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "Account search is temporarily unavailable"})
	}
	result, err := s.searchPlatformAccounts(ctx, query)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "Account search is temporarily unavailable"})
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) searchPlatformAccounts(ctx context.Context, query platformaccounts.Query) (platformaccounts.Result, error) {
	result := platformaccounts.Result{Accounts: []platformaccounts.Account{}, Unsearched: []platformaccounts.Organization{}}
	accounts := make(map[string]platformaccounts.Account)
	account := func(email string) platformaccounts.Account {
		email = platformEmail(email)
		if existing, ok := accounts[email]; ok {
			return existing
		}
		return platformaccounts.Account{Email: email, Memberships: []platformaccounts.Membership{}, Invitations: []platformaccounts.Invitation{}}
	}
	rows, err := s.auth.store.db.QueryContext(ctx, `WITH latest AS (
SELECT lower(email) AS email,subject,created_at,
row_number() OVER (PARTITION BY lower(email) ORDER BY julianday(created_at) DESC,token_hash) AS rank
FROM sessions WHERE coalesce(json_extract(identity_json,'$.support_actor'),'') = ''
AND CASE WHEN ? THEN lower(email)=? ELSE instr(lower(email),?)>0 END)
SELECT email,subject,created_at FROM latest WHERE rank=1 ORDER BY email LIMIT ?`, query.Exact, query.Text, query.Text, platformaccounts.Limit)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var email, subject, at string
		if err := rows.Scan(&email, &subject, &at); err != nil {
			return result, err
		}
		item := account(email)
		item.Subject, item.LastSignInAt = subject, at
		accounts[item.Email] = item
	}
	err = rows.Err()
	if err != nil {
		return result, err
	}
	rows, err = s.registry.store.db.QueryContext(ctx, `SELECT email,role FROM platform_members
WHERE CASE WHEN ? THEN lower(email)=? ELSE instr(lower(email),?)>0 END ORDER BY email LIMIT ?`, query.Exact, query.Text, query.Text, platformaccounts.Limit)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var email, role string
		if err := rows.Scan(&email, &role); err != nil {
			return result, err
		}
		item := account(email)
		item.PlatformRole = role
		accounts[item.Email] = item
	}
	err = rows.Err()
	if err != nil {
		return result, err
	}
	organizations, err := s.registry.List(ctx)
	if err != nil {
		return result, err
	}
	var ready []Organization
	var ids []string
	for _, organization := range organizations {
		if organization.State == "ready" {
			ready = append(ready, organization)
			ids = append(ids, organization.ID)
		}
	}
	tenantResults := make([]platformaccounts.TenantResult, len(ids))
	searched := make([]bool, len(ids))
	s.eachReadyTenant(ctx, ids, func(call context.Context, index int, organization Organization) {
		status, body, err := s.serviceCall(call, organization, "/internal/v1/platform/accounts", query, nil)
		if err == nil && status == http.StatusOK && json.Unmarshal(body, &tenantResults[index]) == nil {
			searched[index] = true
		}
	})
	for index, organization := range ready {
		org := platformaccounts.Organization{ID: organization.ID, Name: organization.Name}
		if !searched[index] {
			result.Unsearched = append(result.Unsearched, org)
			continue
		}
		for _, member := range tenantResults[index].Members {
			item := account(member.Email)
			if item.Subject == "" {
				item.Subject = member.Subject
			}
			item.Memberships = append(item.Memberships, platformaccounts.Membership{Organization: org, Role: member.Role, JoinedAt: member.JoinedAt})
			accounts[item.Email] = item
		}
		for _, invitation := range tenantResults[index].Invitations {
			item := account(invitation.Email)
			item.Invitations = append(item.Invitations, platformaccounts.Invitation{Organization: org, ExpiresAt: invitation.ExpiresAt})
			accounts[item.Email] = item
		}
	}
	emails := make([]string, 0, len(accounts))
	for email := range accounts {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	for _, email := range emails[:min(len(emails), platformaccounts.Limit)] {
		item := accounts[email]
		sort.Slice(item.Memberships, func(i, j int) bool { return strings.Compare(item.Memberships[i].Name, item.Memberships[j].Name) < 0 })
		sort.Slice(item.Invitations, func(i, j int) bool { return strings.Compare(item.Invitations[i].Name, item.Invitations[j].Name) < 0 })
		result.Accounts = append(result.Accounts, item)
	}
	return result, nil
}
