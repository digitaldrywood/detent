package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
)

func hostedKeyAllows(scope, required apikey.Scope) bool {
	return apikey.ValidScope(scope) && apikey.HasScope([]string{string(scope)}, required)
}

func hostedRoleAllows(role string, scope apikey.Scope) bool {
	if !auth.ValidOrganizationRole(role) {
		return false
	}
	if scope == apikey.ScopeRead {
		return true
	}
	if scope == apikey.ScopeWrite {
		return role != "viewer"
	}
	return scope == apikey.ScopeAdmin && (role == "owner" || role == "admin")
}

func (s *Service) hostedAPITokenCredential(ctx context.Context, credential apiCredential, created string, expires sql.NullString) (apiCredential, error) {
	var user, organization, membership, scope sql.NullString
	err := s.database.db.QueryRowContext(ctx, "SELECT hosted_user_id,hosted_organization_id,hosted_membership_id,operator_key_scope FROM api_tokens WHERE id=?", credential.ID).Scan(&user, &organization, &membership, &scope)
	if err != nil {
		return apiCredential{}, err
	}
	if !user.Valid && !organization.Valid && !membership.Valid && !scope.Valid {
		return credential, nil
	}
	if !user.Valid || organization.String != s.config.Hosted.OrganizationID || membership.String == "" || !expires.Valid || !credential.NativeOnly || credential.Runner.RunnerID != "" || credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin || !hostedKeyAllows(apikey.Scope(scope.String), apikey.ScopeRead) {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	provider, err := s.hostedProviderOrganization(ctx)
	if err != nil {
		return apiCredential{}, err
	}
	identity := &auth.HostedIdentity{Subject: user.String, OrganizationID: provider, SessionID: "api-key:" + credential.ID}
	identity.CreatedAt, err = parseTimeValue(created)
	if err != nil {
		return apiCredential{}, err
	}
	identity.ExpiresAt, err = parseTimeValue(expires.String)
	if err != nil {
		return apiCredential{}, err
	}
	current, err := s.hostedMembership(ctx, identity)
	if err != nil || current.ID != membership.String {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	var local string
	err = s.database.db.QueryRowContext(ctx, `SELECT m.principal_id,m.role FROM hosted_members m JOIN api_tokens t ON t.id=m.principal_id WHERE m.user_id=? AND m.membership_id=? AND m.active=1 AND t.revoked_at IS NULL`, user.String, membership.String).Scan(&credential.HostedPrincipal, &local)
	if err != nil || !auth.ValidOrganizationRole(local) {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	credential.Hosted, credential.HostedMembership = identity, membership.String
	credential.HostedRole = lesserHostedRole(current.Role.Slug, local)
	credential.HostedKeyScope = apikey.Scope(scope.String)
	return credential, nil
}

func (s *Service) hostedKeyBrowser(c echo.Context) (apiCredential, error) {
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	credential, _, err := s.hostedCredential(c)
	if err != nil || credential.Hosted == nil || credential.Hosted.SupportActor != "" {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	return credential, nil
}

type hostedAPIKey struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Scope       apikey.Scope `json:"scope"`
	Expiry      string       `json:"expires_at"`
	Fingerprint string       `json:"fingerprint"`
	Revoked     bool         `json:"revoked"`
	Projects    []string     `json:"project_ids"`
}

func (s *Service) hostedAPIKeys(c echo.Context) error {
	credential, err := s.hostedKeyBrowser(c)
	if err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "Sign in as an organization member to manage your API keys")
	}
	keys := []hostedAPIKey{}
	rows, err := s.database.db.QueryContext(c.Request().Context(), `SELECT id,name,operator_key_scope,expires_at,token_fingerprint,revoked_at FROM api_tokens WHERE hosted_user_id=? AND hosted_organization_id=? ORDER BY created_at DESC`, credential.Hosted.Subject, s.config.Hosted.OrganizationID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	for rows.Next() {
		var key hostedAPIKey
		var revoked sql.NullString
		if err := rows.Scan(&key.ID, &key.Name, &key.Scope, &key.Expiry, &key.Fingerprint, &revoked); err != nil {
			rows.Close()
			return s.nativeAPIError(c, err)
		}
		key.Revoked = revoked.Valid
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	for i := range keys {
		grants, err := s.database.db.QueryContext(c.Request().Context(), "SELECT project_id FROM token_grants WHERE token_id=? AND organization_id=? ORDER BY project_id", keys[i].ID, s.config.Hosted.OrganizationID)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		keys[i].Projects = []string{}
		for grants.Next() {
			var project string
			if err := grants.Scan(&project); err != nil {
				grants.Close()
				return s.nativeAPIError(c, err)
			}
			keys[i].Projects = append(keys[i].Projects, project)
		}
		err = grants.Err()
		grants.Close()
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, struct {
		Keys []hostedAPIKey `json:"keys"`
	}{keys})
}

func (s *Service) createHostedAPIKey(c echo.Context) error {
	credential, err := s.hostedKeyBrowser(c)
	if err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "Sign in as an organization member to create an API key")
	}
	var request struct {
		Name     string       `json:"name"`
		Scope    apikey.Scope `json:"scope"`
		Days     int          `json:"expires_days"`
		Projects []string     `json:"project_ids"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Days < 1 || request.Days > 90 || len(request.Projects) > 200 {
		return s.nativeAPIError(c, nativeInvalid("Choose 1–90 days and at most 200 projects"))
	}
	expiry := s.config.now().Add(time.Duration(request.Days) * 24 * time.Hour)
	scope := apiScopeOperator
	if request.Scope == apikey.ScopeAdmin {
		scope = apiScopeAdmin
	}
	key, err := s.createAPITokenFor(c.Request().Context(), tokenRequest{Name: request.Name, Scope: scope, Issuer: &credential, KeyScope: request.Scope, ExpiresAt: &expiry, ProjectIDs: request.Projects})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "credential_created", "POST /api-keys", "", http.StatusCreated); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, key)
}

func (s *Service) revokeHostedAPIKey(c echo.Context) error {
	credential, err := s.hostedKeyBrowser(c)
	if err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "Sign in to revoke your API key")
	}
	var count int
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM api_tokens WHERE id=? AND hosted_user_id=? AND hosted_organization_id=?", c.Param("key"), credential.Hosted.Subject, s.config.Hosted.OrganizationID).Scan(&count); err != nil {
		return s.nativeAPIError(c, err)
	}
	if count != 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := s.revokeAPITokenFor(c.Request().Context(), c.Param("key")); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "credential_revoked", "DELETE /api-keys/:key", "", http.StatusNoContent); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
