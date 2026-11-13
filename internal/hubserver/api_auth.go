package hubserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
	runnerRenewal   bool
	requestCapacity *int
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
	return s.requireHubauthAPI(allowed...)
}

func (s *Service) authenticateAPIRequest(c echo.Context) (apiCredential, int, error) {
	return s.authenticateAPIRequestContext(c.Request().Context(), c)
}

func (s *Service) authenticateAPIRequestContext(ctx context.Context, c echo.Context) (apiCredential, int, error) {
	return nativeCredentialResult(s.requestAuth().AuthenticateRequest(ctx, c))
}

// authenticateAPIToken is shared by HTTP authentication and operator execution.
func (s *Service) authenticateAPIToken(ctx context.Context, token, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	return s.authenticateAPIHash(ctx, apikey.HashToken(token), renewalRunner, renewalOrganization)
}

func (s *Service) authenticateAPIHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	return s.authenticateAccessHash(ctx, hash, renewalRunner, renewalOrganization)
}

func apiBearerToken(c echo.Context) (string, error) {
	return hubauth.BearerToken(c)
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
