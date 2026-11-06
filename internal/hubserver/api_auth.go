package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type apiScope string

const (
	apiScopeWorker      apiScope = "worker"
	apiScopeOperator    apiScope = "operator"
	apiScopeAdmin       apiScope = "admin"
	bootstrapTokenID             = "bootstrap-admin"
	maxBearerTokenBytes          = 4096
)

type apiCredential struct {
	EntryKey   *apikey.KeyAuthority
	Hosted     *auth.HostedIdentity
	HostedRole string
	// HostedMembership is the provider membership the credential was
	// resolved from; mutation rechecks compare it against the member row.
	HostedMembership    string
	HostedPrincipal     string
	HostedKeyScope      apikey.Scope
	HostedKeyKind       string
	HostedProjectAccess hostedProjectAccess
	ManageRunners       bool
	SessionHash         string
	Hash                string
	ID                  string
	Name                string
	Scope               apiScope
	NativeOnly          bool
	Runner              runnerauth.Identity
	// Agent is the assistant acting for this credential's principal.
	Agent string
	// runnerRenewal is set only for POST renewal of this bound identity.
	// Expiry limits ordinary API use, but must not strand a stopped host.
	runnerRenewal bool
}

type apiErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type tokenRequest struct {
	Kind          string              `json:"-"`
	Name          string              `json:"name"`
	Scope         apiScope            `json:"scope"`
	Issuer        *apiCredential      `json:"-"`
	KeyScope      apikey.Scope        `json:"-"`
	ExpiresAt     *time.Time          `json:"-"`
	ProjectIDs    []string            `json:"-"`
	ProjectAccess hostedProjectAccess `json:"-"`
}

type tokenGrantResponse struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
}
type tokenResponse struct {
	Kind          string               `json:"kind,omitempty"`
	ProjectAccess hostedProjectAccess  `json:"project_access,omitempty"`
	Projects      []string             `json:"project_ids"`
	ExpiresAt     *time.Time           `json:"expires_at,omitempty"`
	KeyScope      apikey.Scope         `json:"key_scope,omitempty"`
	NativeOnly    bool                 `json:"native_only"`
	RevokedAt     *time.Time           `json:"revoked_at,omitempty"`
	Grants        []tokenGrantResponse `json:"grants"`
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Scope         apiScope             `json:"scope"`
	Token         string               `json:"token,omitempty"`
	Fingerprint   string               `json:"fingerprint"`
	CreatedAt     time.Time            `json:"created_at"`
	RotatedAt     time.Time            `json:"rotated_at,omitempty"`
}

func (s *Service) requireAPIScope(allowed ...apiScope) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.authenticateAPIRequest(c)
			if err != nil {
				if errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid || errors.Is(operatorAuthorizationError(err), operatortool.ErrProviderUnavailable) {
					return s.nativeAPIError(c, err)
				}
				return s.refusedAPIError(c, status, "unauthorized", "Valid scoped API token is required", apiErrorResponse{Code: "unauthorized", Message: "Valid scoped API token is required"})
			}
			if s.config.Hosted != nil {
				if credential.Hosted != nil && c.Path() == runnerBase+"/:runner/routing" && c.Request().Method == http.MethodGet {
					return s.requireHostedAdministration(next)(c)
				}
				if credential.Hosted == nil && credential.Runner.RunnerID == "" && c.Path() != "/health" && !s.hostedArtifactPublisher(c, credential) && !s.hostedChangeCheckPrincipal(c, credential) {
					return s.nativeAPIError(c, nativeNotFound())
				}
				hostedRoute := strings.HasPrefix(c.Path(), nativeBase) || c.Path() == nativeOrganizationIssuePath && hostedReadRequest(c)
				if credential.Hosted != nil && (!hostedRoute || strings.HasSuffix(c.Path(), "/checks") || strings.Contains(c.Path(), "/imports")) {
					return s.nativeAPIError(c, nativeNotFound())
				}
			}
			if credential.HostedKeyScope != "" && !hostedKeyAllows(credential.HostedKeyScope, apikey.ScopeWrite) && !hostedReadRequest(c) {
				return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "API key permission does not permit writes", nil)
			}
			if credential.Runner.RunnerID != "" && !runnerOperationAllowed(c, credential.Runner.Operations) {
				return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "Runner does not permit this operation", apiErrorResponse{Code: "insufficient_scope", Message: "Runner does not permit this operation"})
			}
			if strings.HasPrefix(c.Path(), "/api/v1/") {
				if credential.NativeOnly {
					return s.refusedAPIError(c, http.StatusForbidden, "native_protocol_required", "Scoped tokens require the native protocol", apiErrorResponse{Code: "native_protocol_required", Message: "Scoped tokens require the native protocol"})
				}
				if err := s.requireCompatibilityResource(c); err != nil {
					return err
				}
			}
			for _, scope := range allowed {
				if credential.Scope == scope || credential.Scope == apiScopeAdmin {
					c.Set("hub_api_credential", credential)
					if err := s.admitRunnerRequest(c, credential); err != nil || c.Response().Committed {
						return err
					}
					return next(c)
				}
			}
			return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "API token scope does not permit this operation", apiErrorResponse{Code: "insufficient_scope", Message: "API token scope does not permit this operation"})
		}
	}
}

func (s *Service) authenticateAPIRequest(c echo.Context) (apiCredential, int, error) {
	return s.authenticateAPIRequestContext(c.Request().Context(), c)
}

func (s *Service) authenticateAPIRequestContext(ctx context.Context, c echo.Context) (apiCredential, int, error) {
	if credential, ok := entryBoardCredential(ctx); ok {
		return credential, http.StatusOK, nil
	}
	if s.config.Hosted == nil && (c.Request().Header.Get(echo.HeaderAuthorization) == "" || strings.HasPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Basic ")) {
		credential, status, err := s.localAppCredentialContext(ctx, c)
		if status == http.StatusUnauthorized {
			c.Response().Header().Set("WWW-Authenticate", `Basic realm="Detent Hub", charset="UTF-8"`)
		}
		return credential, status, err
	}
	if claims, ok := hostedSharedClaims(c); ok && claims.Kind == cloudassert.KindKey {
		return s.entryKeyCredential(c.Request().Context(), claims)
	}
	if s.config.Hosted != nil && c.Request().Header.Get(echo.HeaderAuthorization) == "" {
		return s.hostedCredential(c.Request().Context(), c)
	}
	token, err := apiBearerToken(c)
	if err != nil {
		return apiCredential{}, http.StatusUnauthorized, err
	}
	renewalRunner, renewalOrganization := "", ""
	if c.Request().Method == http.MethodPost && c.Path() == runnerBase+"/:runner/renew" {
		renewalRunner, renewalOrganization = c.Param("runner"), c.Param("organization")
	}
	organization := c.Param("organization")
	if organization == "" && c.Path() == "/api/v2/capabilities" {
		organization = strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization"))
		if organization == "" && s.config.Hosted != nil {
			organization = s.config.Hosted.OrganizationID
		}
	}
	if s.config.TenantRepository != nil && organization != "" {
		if hinted := strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization")); hinted != "" && hinted != organization {
			return apiCredential{}, http.StatusUnauthorized, fmt.Errorf("%w: organization scope does not match request", operatortool.ErrAccessDenied)
		}
		return s.authenticateTenantAPIHash(ctx, tracker.OrganizationID(organization), apikey.HashToken(token), renewalRunner, renewalOrganization)
	}
	return s.authenticateAPIToken(ctx, token, renewalRunner, renewalOrganization)
}

// authenticateAPIToken is shared by HTTP authentication and operator execution.
func (s *Service) authenticateAPIToken(ctx context.Context, token, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	return s.authenticateAPIHash(ctx, apikey.HashToken(token), renewalRunner, renewalOrganization)
}

func (s *Service) authenticateAPIHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	return s.authenticateAccessHash(ctx, hash, renewalRunner, renewalOrganization)
}

func (credential apiCredential) timeValid(now time.Time, created string, expires sql.NullString) bool {
	if !expires.Valid {
		return !credential.runnerRenewal
	}
	if !credential.runnerRenewal {
		return runnerTimeValid(now, created, expires.String)
	}
	start, err := parseTimeValue(created)
	if err != nil {
		return false
	}
	end, err := parseTimeValue(expires.String)
	return err == nil && start.Before(end) && !now.Before(start)
}

func apiBearerToken(c echo.Context) (string, error) {
	if c == nil || c.Request() == nil {
		return "", fmt.Errorf("%w: request is required", operatortool.ErrAccessDenied)
	}
	authorizations := c.Request().Header.Values(echo.HeaderAuthorization)
	if len(authorizations) != 1 {
		return "", fmt.Errorf("%w: one authorization header is required", operatortool.ErrAccessDenied)
	}
	value := strings.TrimSpace(authorizations[0])
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("%w: bearer authorization is required", operatortool.ErrAccessDenied)
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxBearerTokenBytes {
		return "", fmt.Errorf("%w: bearer token is invalid", operatortool.ErrAccessDenied)
	}
	return token, nil
}

func (s *Service) createAPIToken(c echo.Context) error {
	var request tokenRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	response, err := s.createAPITokenFor(c.Request().Context(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, response)
}

func (s *Service) rotateAPIToken(c echo.Context) error {
	response, err := s.rotateAPITokenFor(c.Request().Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, response)
}

func (s *Service) revokeAPIToken(c echo.Context) error {
	if err := s.revokeAPITokenFor(c.Request().Context(), strings.TrimSpace(c.Param("id"))); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func validAPIScope(scope apiScope) bool {
	switch scope {
	case apiScopeWorker, apiScopeOperator, apiScopeAdmin:
		return true
	default:
		return false
	}
}

func tokenFingerprint(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
