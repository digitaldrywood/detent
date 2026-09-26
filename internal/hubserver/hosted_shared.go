package hubserver

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const hostedSharedClaimsKey = "hosted_shared_claims"

type HostedSharedEntry struct {
	Issuer     string
	PublicKeys []ed25519.PublicKey
	Generation int64

	verifier *cloudassert.Verifier
}

func (e *HostedSharedEntry) validate(organization string) error {
	if e == nil {
		return nil
	}
	if !hostedSafeID(e.Issuer) || e.Generation < 1 || len(e.PublicKeys) == 0 || len(e.PublicKeys) > 4 {
		return errors.New("shared entry requires an issuer, allocation generation and one to four public keys")
	}
	for _, key := range e.PublicKeys {
		if len(key) != ed25519.PublicKeySize {
			return errors.New("shared entry public key is invalid")
		}
	}
	e.verifier = &cloudassert.Verifier{PublicKeys: e.PublicKeys, Issuer: e.Issuer, Audience: organization, Generation: e.Generation}
	return nil
}

func (s *Service) hostedShared() bool {
	return s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil
}

func (s *Service) hostedBase() string {
	if s.hostedShared() {
		return "/organizations/" + s.config.Hosted.OrganizationID
	}
	return ""
}

func (s *Service) hostedPath(path string) string {
	return s.hostedBase() + path
}

func (s *Service) hostedSignInPath() string {
	if s.hostedShared() {
		return "/organizations"
	}
	return "/login"
}

var hostedSharedEntryRoutes = map[string]bool{
	"/login": true, "/auth/oidc/start": true, "/auth/oidc/callback": true, "/invite": true, "/logout": true,
	"/support": true, "/support/start": true, "/webhooks/stripe": true,
	"/organization/switch": true, "/organization/create": true, "/organization/join": true,
}

func (s *Service) hostedSharedEntry(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		request := c.Request()
		entry := s.config.Hosted.SharedEntry
		if !cloudassert.CanonicalPath(request.URL) {
			return s.nativeAPIError(c, nativeNotFound())
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, cloudassert.MaxBodyBytes+1))
		if err != nil {
			return c.JSON(http.StatusBadRequest, apiErrorResponse{Code: "invalid_request", Message: "Request body could not be read"})
		}
		if len(body) > cloudassert.MaxBodyBytes {
			return c.JSON(http.StatusRequestEntityTooLarge, apiErrorResponse{Code: "payload_too_large", Message: "Request body is too large"})
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		claims, err := entry.verifier.Verify(request.Header.Get(cloudassert.Header), request.Method, request.URL.RequestURI(), body)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "entry_assertion_required", Message: "Requests must arrive through the authenticated shared entry"})
		}
		request.Header.Del(cloudassert.Header)
		bearer := request.Header.Get(echo.HeaderAuthorization) != ""
		if claims.Kind == cloudassert.KindBrowser && bearer || claims.Kind == cloudassert.KindMachine && !bearer {
			return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "entry_assertion_required", Message: "Requests must arrive through the authenticated shared entry"})
		}
		request.Header.Del("Cookie")
		organization := s.config.Hosted.OrganizationID
		prefix := "/organizations/" + organization
		path := request.URL.Path
		internal := strings.HasPrefix(path, "/internal/")
		switch {
		case internal:
		case path == prefix || strings.HasPrefix(path, prefix+"/"):
			path = strings.TrimPrefix(path, prefix)
			if path == "" {
				path = "/"
			}
			if strings.HasPrefix(path, "/organizations/") || strings.HasPrefix(path, "/internal/") || hostedSharedEntryRoutes[path] {
				return s.nativeAPIError(c, nativeNotFound())
			}
			request.URL.Path, request.URL.RawPath = path, ""
		case strings.HasPrefix(path, "/api/v2/organizations/"+organization+"/"):
		default:
			return s.nativeAPIError(c, nativeNotFound())
		}
		if internal != (claims.Kind == cloudassert.KindService) {
			return s.nativeAPIError(c, nativeNotFound())
		}
		c.Set(hostedSharedClaimsKey, claims)
		return next(c)
	}
}

func hostedSharedClaims(c echo.Context) (cloudassert.Claims, bool) {
	claims, ok := c.Get(hostedSharedClaimsKey).(cloudassert.Claims)
	return claims, ok
}

func (s *Service) sharedHostedSession(c echo.Context) (auth.Session, string, error) {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindBrowser {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	identity := auth.HostedIdentity{
		Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, SessionID: claims.ProviderSession,
		CreatedAt: claims.SessionCreatedAt, ExpiresAt: claims.SessionExpiresAt, SupportActor: claims.SupportActor, SupportReason: claims.SupportReason,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	ctx := c.Request().Context()
	if _, err := s.database.db.ExecContext(ctx, "INSERT INTO hosted_sessions (token_hash,email,identity_json,expires_at,created_at) VALUES (?,?,?,?,?) ON CONFLICT(token_hash) DO NOTHING", claims.Binding, claims.Email, string(encoded), formatHubTime(claims.SessionExpiresAt), formatHubTime(s.config.now())); err != nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	session, err := s.WebSession(ctx, claims.Binding, s.config.now())
	if err != nil || session.Identity == nil || session.Identity.Subject != identity.Subject || session.Identity.SessionID != identity.SessionID || session.Identity.OrganizationID != identity.OrganizationID || !strings.EqualFold(session.Email, claims.Email) {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	c.Set("hosted_session", session)
	return session, claims.Binding, nil
}

func (s *Service) hostedSharedCSRF(c echo.Context) string {
	if claims, ok := hostedSharedClaims(c); ok && claims.Kind == cloudassert.KindBrowser {
		return claims.CSRF
	}
	return ""
}

func (s *Service) hostedSharedCSRFValid(c echo.Context, value string) bool {
	expected := s.hostedSharedCSRF(c)
	return expected != "" && subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

func (s *Service) registerHostedSharedRoutes(e *echo.Echo) {
	e.POST("/internal/v1/sessions/revoke", s.revokeHostedSharedSessions)
	e.POST("/internal/v1/invitations/accept", s.acceptHostedSharedInvitation)
}

type hostedSharedRevocation struct {
	Bindings []string `json:"bindings"`
}

func (s *Service) revokeHostedSharedSessions(c echo.Context) error {
	var request hostedSharedRevocation
	if err := decodeAPIJSON(c, &request); err != nil || len(request.Bindings) == 0 || len(request.Bindings) > 100 {
		return invalidAPIRequest(c, errors.New("invalid revocation"))
	}
	ctx := c.Request().Context()
	now := formatHubTime(s.config.now())
	for _, binding := range request.Bindings {
		if len(binding) != 64 {
			return invalidAPIRequest(c, errors.New("invalid binding"))
		}
		if _, err := s.database.db.ExecContext(ctx, "INSERT INTO hosted_sessions (token_hash,email,identity_json,expires_at,created_at,revoked_at) VALUES (?,'','{}',?,?,?) ON CONFLICT(token_hash) DO UPDATE SET revoked_at = COALESCE(revoked_at, excluded.revoked_at)", binding, now, now, now); err != nil {
			return s.internalAPIError(c, "revocation_unavailable", "Sessions could not be revoked", err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

type hostedSharedInvitation struct {
	Token string `json:"token"`
}

func (s *Service) acceptHostedSharedInvitation(c echo.Context) error {
	claims, _ := hostedSharedClaims(c)
	var request hostedSharedInvitation
	if err := decodeAPIJSON(c, &request); err != nil || claims.Subject == "" || claims.Email == "" {
		return invalidAPIRequest(c, errors.New("invalid invitation"))
	}
	if hostedEmailListed(s.config.Hosted.StaffEmails, claims.Email) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "invitation_unavailable", Message: "This invitation is unavailable"})
	}
	identity := auth.Identity{Subject: claims.Subject, Email: claims.Email, EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: claims.Subject, SessionID: claims.ProviderSession}}
	s.hostedMutationMu.Lock()
	err := s.acceptHostedInvitationFor(c.Request().Context(), identity, request.Token)
	s.hostedMutationMu.Unlock()
	if err != nil {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "invitation_unavailable", Message: "This invitation is expired, already used, or intended for another account or organization"})
	}
	return c.NoContent(http.StatusNoContent)
}
