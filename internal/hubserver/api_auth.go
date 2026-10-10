package hubserver

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
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
	HostedProjectAccess hostedProjectAccess
	ManageRunners       bool
	SessionHash         string
	Hash                string
	ID                  string
	Name                string
	Scope               apiScope
	NativeOnly          bool
	Runner              runnerauth.Identity
	// runnerRenewal is set only for POST renewal of this bound identity.
	// Expiry limits ordinary API use, but must not strand a stopped host.
	runnerRenewal bool
}

type apiErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type tokenRequest struct {
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

func (d *database) ensureInitialAdminToken(ctx context.Context, token []byte) error {
	value := strings.TrimSpace(string(token))
	if value == "" {
		return nil
	}
	now, err := d.currentTime()
	if err != nil {
		return err
	}
	formatted := formatHubTime(now)
	_, err = d.db.ExecContext(ctx, `
INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`,
		bootstrapTokenID, "Bootstrap administrator", apikey.HashToken(value), tokenFingerprint(apikey.HashToken(value)), apiScopeAdmin, formatted, formatted,
	)
	if err != nil {
		return fmt.Errorf("store initial hub administrator token: %w", err)
	}
	return nil
}

func (s *Service) requireAPIScope(allowed ...apiScope) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.authenticateAPIRequest(c)
			if err != nil {
				return c.JSON(status, apiErrorResponse{Code: "unauthorized", Message: "Valid scoped API token is required"})
			}
			if s.config.Hosted != nil {
				if credential.Hosted == nil && credential.Runner.RunnerID == "" && !s.hostedArtifactPublisher(c, credential) && !s.hostedChangeCheckPrincipal(c, credential) {
					return s.nativeAPIError(c, nativeNotFound())
				}
				hostedRoute := strings.HasPrefix(c.Path(), nativeBase) || c.Path() == nativeOrganizationIssuePath && hostedReadRequest(c)
				if credential.Hosted != nil && (!hostedRoute || strings.HasSuffix(c.Path(), "/checks") || strings.Contains(c.Path(), "/imports")) {
					return s.nativeAPIError(c, nativeNotFound())
				}
			}
			if credential.HostedKeyScope != "" && !hostedKeyAllows(credential.HostedKeyScope, apikey.ScopeWrite) && !hostedReadRequest(c) {
				return c.NoContent(http.StatusForbidden)
			}
			if credential.Runner.RunnerID != "" && !runnerOperationAllowed(c, credential.Runner.Operations) {
				return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "insufficient_scope", Message: "Runner does not permit this operation"})
			}
			if strings.HasPrefix(c.Path(), "/api/v1/") {
				if credential.NativeOnly {
					return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "native_protocol_required", Message: "Scoped tokens require the native protocol"})
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
			return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "insufficient_scope", Message: "API token scope does not permit this operation"})
		}
	}
}

func (s *Service) authenticateAPIRequest(c echo.Context) (apiCredential, int, error) {
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
	return s.authenticateAPIToken(c.Request().Context(), token, renewalRunner, renewalOrganization)
}

// authenticateAPIToken is shared by HTTP authentication and operator execution.
func (s *Service) authenticateAPIToken(ctx context.Context, token, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	return s.authenticateAPIHash(ctx, apikey.HashToken(token), renewalRunner, renewalOrganization)
}

func (s *Service) authenticateAPIHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (apiCredential, int, error) {
	var credential apiCredential
	var storedHash, createdAt, operations string
	var revokedAt, expiresAt, lastUsedAt sql.NullString
	err := s.database.auth().QueryRowContext(ctx, `
SELECT t.id, t.name, t.scope, t.token_hash, t.revoked_at, t.native_only, t.expires_at, t.created_at, t.last_used_at,
coalesce(r.id, ''), coalesce(r.machine_id, ''), coalesce(r.organization_id, ''), coalesce(r.operations_json, '[]')
FROM api_tokens t LEFT JOIN runner_identities r ON r.token_id = t.id
WHERE t.token_hash = ?`, hash).Scan(&credential.ID, &credential.Name, &credential.Scope, &storedHash, &revokedAt, &credential.NativeOnly, &expiresAt, &createdAt, &lastUsedAt,
		&credential.Runner.RunnerID, &credential.Runner.MachineID, &credential.Runner.OrganizationID, &operations)
	if errors.Is(err, sql.ErrNoRows) {
		return apiCredential{}, http.StatusUnauthorized, errors.New("token was not found")
	}
	if err != nil {
		return apiCredential{}, http.StatusServiceUnavailable, fmt.Errorf("read hub API token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hash)) != 1 || revokedAt.Valid {
		return apiCredential{}, http.StatusUnauthorized, errors.New("token is inactive")
	}
	now, err := s.database.currentTime()
	if err != nil {
		return apiCredential{}, http.StatusServiceUnavailable, err
	}
	credential.runnerRenewal = credential.Runner.Valid() && renewalRunner != "" && credential.Runner.RunnerID == renewalRunner && string(credential.Runner.OrganizationID) == renewalOrganization
	if !credential.timeValid(now, createdAt, expiresAt) {
		return apiCredential{}, http.StatusUnauthorized, errors.New("token is outside its validity interval")
	}
	if expiresAt.Valid {
		credential.Runner.ExpiresAt, err = parseTimeValue(expiresAt.String)
		if err != nil {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
	}
	if err := json.Unmarshal([]byte(operations), &credential.Runner.Operations); err != nil {
		return apiCredential{}, http.StatusServiceUnavailable, err
	}
	credential.Hash = hash
	if s.config.Hosted != nil {
		credential, err = s.hostedAPITokenCredential(ctx, credential, createdAt, expiresAt)
		if err != nil {
			return apiCredential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
		}
	}
	if tokenUseStale(lastUsedAt, now) {
		s.recordTokenUse(ctx, credential.ID, now)
	}
	return credential, http.StatusOK, nil
}

// recordTokenUse writes last_used_at off the request path, at most one write
// in flight per token, so authentication never waits for the writer.
func (s *Service) recordTokenUse(ctx context.Context, id string, now time.Time) {
	if _, pending := s.tokenUse.LoadOrStore(id, struct{}{}); pending {
		return
	}
	s.tokenUseWork.Add(1)
	go func() {
		defer s.tokenUseWork.Done()
		defer s.tokenUse.Delete(id)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := s.database.db.ExecContext(ctx, "UPDATE api_tokens SET last_used_at = ? WHERE id = ?", formatHubTime(now), id); err != nil {
			s.config.Logger.Warn("record hub API token use failed", "token_id", id, "error", err)
		}
	}()
}

// tokenUseInterval bounds how often an authenticated request writes
// last_used_at, so authentication reads never queue behind the writer.
const tokenUseInterval = time.Minute

func tokenUseStale(lastUsed sql.NullString, now time.Time) bool {
	if !lastUsed.Valid {
		return true
	}
	at, err := parseTimeValue(lastUsed.String)
	return err != nil || now.Sub(at) >= tokenUseInterval
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
		return "", errors.New("request is required")
	}
	authorizations := c.Request().Header.Values(echo.HeaderAuthorization)
	if len(authorizations) != 1 {
		return "", errors.New("one authorization header is required")
	}
	value := strings.TrimSpace(authorizations[0])
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("bearer authorization is required")
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxBearerTokenBytes {
		return "", errors.New("bearer token is invalid")
	}
	return token, nil
}

func (s *Service) createAPIToken(c echo.Context) error {
	var request tokenRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
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
