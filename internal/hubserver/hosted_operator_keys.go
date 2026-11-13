package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func resolveHostedProjectAccess(access hubapi.HostedProjectAccess, projects []string) (hubapi.HostedProjectAccess, string) {
	if access == "" {
		access = hubapi.HostedProjectsAll
		if len(projects) > 0 {
			access = hubapi.HostedProjectsSelected
		}
	}
	if access != hubapi.HostedProjectsAll && access != hubapi.HostedProjectsSelected {
		return "", "Choose all or selected project access"
	}
	if access == hubapi.HostedProjectsSelected && len(projects) == 0 {
		return "", "Selected project access requires at least one project"
	}
	if access == hubapi.HostedProjectsAll && len(projects) > 0 {
		return "", "All project access cannot include selected project IDs"
	}
	return access, ""
}

func (s *Service) hostedKeyBrowser(c echo.Context) (hubapi.APICredential, error) {
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return hubapi.APICredential{}, auth.ErrHostedIdentity
	}
	credential, status, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		if status >= http.StatusInternalServerError {
			return hubapi.APICredential{}, errors.Join(tenantauth.ErrUnavailable, err)
		}
		return hubapi.APICredential{}, err
	}
	if credential.Hosted == nil || credential.Hosted.SupportActor != "" {
		return hubapi.APICredential{}, auth.ErrHostedIdentity
	}
	return s.hostedKeyCredentialFor(c.Request().Context(), credential)
}

func (s *Service) hostedKeyCredentialFor(ctx context.Context, credential hubapi.APICredential) (hubapi.APICredential, error) {
	if s.config.Hosted == nil || credential.Hosted == nil || credential.SessionHash == "" || credential.HostedKeyScope != "" || credential.Hosted.SupportActor != "" {
		return hubapi.APICredential{}, operatortool.ErrAccessDenied
	}
	session, err := s.WebSession(ctx, credential.SessionHash, s.config.now())
	if err != nil {
		return hubapi.APICredential{}, hubscope.OperatorAuthorizationError(err)
	}
	if session.Identity.SupportActor != "" {
		return hubapi.APICredential{}, operatortool.ErrAccessDenied
	}
	current, _, err := s.hostedSessionCredential(ctx, session, credential.SessionHash)
	if err != nil {
		return hubapi.APICredential{}, hubscope.OperatorAuthorizationError(err)
	}
	if current.ID != credential.ID || current.Hash != credential.Hash || current.HostedMembership != credential.HostedMembership {
		return hubapi.APICredential{}, operatortool.ErrAccessDenied
	}
	current.HostedRole = hubapi.LesserHostedRole(current.HostedRole, credential.HostedRole)
	return current, nil
}

type hostedAPIKey struct {
	Kind          string                     `json:"kind"`
	ProjectAccess hubapi.HostedProjectAccess `json:"project_access"`
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Scope         apikey.Scope               `json:"scope"`
	Expiry        *string                    `json:"expires_at"`
	CreatedAt     string                     `json:"created_at"`
	RevokedAt     *string                    `json:"revoked_at,omitempty"`
	Fingerprint   string                     `json:"fingerprint"`
	Revoked       bool                       `json:"revoked"`
	Projects      []string                   `json:"project_ids"`
}

func (s *Service) hostedAPIKeys(c echo.Context) error {
	credential, err := s.hostedKeyBrowser(c)
	if err != nil {
		if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.nativeAPIError(c, hubscope.OperatorAuthorizationError(err))
		}
		return s.hostedJSONError(c, http.StatusForbidden, "Sign in as an organization member to manage your API keys")
	}
	keys, err := s.hostedAPIKeysFor(c.Request().Context(), credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, struct {
		Keys []hostedAPIKey `json:"keys"`
	}{keys})
}

func (s *Service) hostedAPIKeysFor(ctx context.Context, credential hubapi.APICredential) ([]hostedAPIKey, error) {

	return s.accessHostedKeys(ctx, credential)
}

type hostedKeyRequest struct {
	Kind          string                     `json:"kind,omitempty"`
	ProjectAccess hubapi.HostedProjectAccess `json:"project_access"`
	Name          string                     `json:"name"`
	Scope         apikey.Scope               `json:"scope"`
	Days          int                        `json:"expires_days"`
	NeverExpires  bool                       `json:"never_expires,omitempty"`
	Projects      []string                   `json:"project_ids"`
}

func (request hostedKeyRequest) validExpiry() bool {
	if request.NeverExpires {
		return request.Days == 0
	}
	return request.Days >= 1 && request.Days <= 90
}

func (s *Service) createHostedAPIKey(c echo.Context) error {
	credential, err := s.hostedKeyBrowser(c)
	if err != nil {
		if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.nativeAPIError(c, hubscope.OperatorAuthorizationError(err))
		}
		return s.hostedJSONError(c, http.StatusForbidden, "Sign in as an organization member to create an API key")
	}
	var request hostedKeyRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, message := resolveHostedProjectAccess(request.ProjectAccess, request.Projects); message != "" {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, message)
	}
	key, err := s.createHostedAPIKeyFor(c.Request().Context(), credential, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, key)
}

func (s *Service) authorizeHostedKeyRequest(ctx context.Context, credential hubapi.APICredential, request hostedKeyRequest) error {
	if request.Kind != "" && request.Kind != "personal" && request.Kind != "service" {
		return operatortool.ErrInvalidArguments
	}
	if !request.validExpiry() || len(request.Projects) > 200 || !apikey.ValidScope(request.Scope) {
		return operatortool.ErrInvalidArguments
	}
	if _, message := resolveHostedProjectAccess(request.ProjectAccess, request.Projects); message != "" {
		return operatortool.ErrInvalidArguments
	}
	if !hubscope.HostedRoleAllows(credential.HostedRole, request.Scope, "credentials") {
		return operatortool.ErrAccessDenied
	}
	for _, project := range request.Projects {
		scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), Project: tracker.ProjectID(project), Credential: credential}
		if err := s.requireHostedProject(ctx, nil, scope, false); err != nil {
			return err
		}
		if err := s.database.authorizeNativeProject(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) createHostedAPIKeyFor(ctx context.Context, credential hubapi.APICredential, request hostedKeyRequest) (hubapi.TokenResponse, error) {
	credential, err := s.hostedKeyCredentialFor(ctx, credential)
	if err != nil {
		return hubapi.TokenResponse{}, err
	}
	if !request.validExpiry() || len(request.Projects) > 200 {
		return hubapi.TokenResponse{}, hubapi.NativePublicInvalid("Choose Never or 1–90 days and at most 200 projects")
	}
	var message string
	request.ProjectAccess, message = resolveHostedProjectAccess(request.ProjectAccess, request.Projects)
	if message != "" {
		return hubapi.TokenResponse{}, hubapi.NativePublicInvalid(message)
	}
	var expiry *time.Time
	if !request.NeverExpires {
		value := s.config.now().Add(time.Duration(request.Days) * 24 * time.Hour)
		expiry = &value
	}
	scope := hubapi.APIScopeOperator
	if request.Scope == apikey.ScopeAdmin {
		scope = hubapi.APIScopeAdmin
	}
	key, err := s.createAPITokenFor(ctx, hubapi.TokenRequest{Kind: request.Kind, Name: request.Name, Scope: scope, Issuer: &credential, KeyScope: request.Scope, ExpiresAt: expiry, ProjectIDs: request.Projects, ProjectAccess: request.ProjectAccess})
	if err != nil {
		return hubapi.TokenResponse{}, err
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "credential_created", "POST /api-keys", "", http.StatusCreated); err != nil {
		return hubapi.TokenResponse{}, err
	}
	return key, nil
}

func (s *Service) revokeHostedAPIKey(c echo.Context) error {
	credential, err := s.hostedKeyBrowser(c)
	if err != nil {
		if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.nativeAPIError(c, hubscope.OperatorAuthorizationError(err))
		}
		return s.hostedJSONError(c, http.StatusForbidden, "Sign in to revoke your API key")
	}
	if err := s.revokeHostedAPIKeyFor(c.Request().Context(), credential, c.Param("key")); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) hostedAPIKeyFor(ctx context.Context, credential hubapi.APICredential, id string) (hostedAPIKey, error) {
	keys, err := s.hostedAPIKeysFor(ctx, credential)
	if err != nil {
		return hostedAPIKey{}, err
	}
	for _, key := range keys {
		if key.ID == id {
			return key, nil
		}
	}
	return hostedAPIKey{}, hubapi.NativeNotFound()
}

func (s *Service) revokeHostedAPIKeyFor(ctx context.Context, credential hubapi.APICredential, id string) error {
	if _, err := s.hostedAPIKeyFor(ctx, credential, id); err != nil {
		return err
	}
	if err := s.revokeAPITokenFor(ctx, id); err != nil {
		return err
	}
	return s.hostedAudit(ctx, credential.Hosted, "credential_revoked", "DELETE /api-keys/:key", "", http.StatusNoContent)
}
