package hubserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

const hostedCookie = "detent_hosted_session"

func (s *Service) CreateWebSession(ctx context.Context, record auth.SessionRecord) error {
	return fromFeatureError(s.requestAuth().CreateWebSession(ctx, record))
}

func (s *Service) storedWebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	return s.accessWebSession(ctx, hash, now)
}

func (s *Service) WebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	return s.requestAuth().WebSession(ctx, hash, now)
}

func (s *Service) hostedSession(ctx context.Context, c echo.Context) (auth.Session, string, error) {
	return s.requestAuth().HostedSession(ctx, c)
}

func (s *Service) hostedMembership(ctx context.Context, identity *auth.HostedIdentity) (auth.Membership, error) {
	return s.requestAuth().Membership(ctx, identity)
}

func (s *Service) hostedCredential(ctx context.Context, c echo.Context) (apiCredential, int, error) {
	return nativeCredentialResult(s.requestAuth().HostedCredential(ctx, c))
}

// hostedSharedCredential builds a browser credential from the shared entry's
// assertion. The entry verified the WorkOS access token and carries its role,
// so the tenant does not call the provider on the request path; membership
// removal reaches the tenant through the entry's revocation of the binding.
func (s *Service) hostedSharedCredential(ctx context.Context, session auth.Session, hash, role string) (apiCredential, int, error) {
	return nativeCredentialResult(s.requestAuth().SharedCredential(ctx, session, hash, role))
}

func (s *Service) completePendingHostedInvitation(ctx context.Context, session auth.Session) (bool, error) {
	return s.accessInvitesService().RecoverPending(ctx, session)
}

// lesserHostedRole returns the less privileged of two organization roles, so
// a downgrade recorded by the tenant applies before the entry's access token
// carrying the older role expires.
func lesserHostedRole(a, b string) string {
	return hubapi.LesserHostedRole(a, b)
}

func (s *Service) hostedSessionCredential(ctx context.Context, session auth.Session, hash string) (apiCredential, int, error) {
	return nativeCredentialResult(s.requestAuth().SessionCredential(ctx, session, hash))
}

func hostedCSRF(token string) string {
	sum := sha256.Sum256([]byte("detent-hosted-csrf:" + token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) hostedCSRFValid(c echo.Context) bool {
	value := c.Request().Header.Get("X-CSRF-Token")
	if value == "" && strings.HasPrefix(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationForm) {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxAPIRequestBodyBytes)
		value = c.FormValue("csrf")
	}
	if s.hostedShared() {
		return s.hostedSharedCSRFValid(c, value)
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(hostedCSRF(cookie.Value))) == 1
}

func hostedReadRequest(c echo.Context) bool {
	return hubauth.ReadRequest(c)
}

type hostedMutationContext struct{}

func (s *Service) hostedBoundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		defer s.recordHostedRequest(c, time.Now())
		c.Response().Header().Set("Cache-Control", "no-store")
		c.Response().Header().Set("Referrer-Policy", "no-referrer")
		c.Response().Header().Set("X-Content-Type-Options", "nosniff")
		if organization := c.Param("organization"); organization != "" && organization != s.config.Hosted.OrganizationID {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/api/v1/webhooks/github" && c.Request().Method == http.MethodPost {
			return next(c)
		}
		if strings.HasPrefix(c.Path(), "/api/v1/") {
			if s.database.postgres != nil {
				return s.postgresFeatureUnavailable(c)
			}
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/health" && s.database.postgres == nil {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/webhooks/stripe" && c.Request().Method == http.MethodPost {
			return next(c)
		}
		bearerAPI := (strings.HasPrefix(c.Request().URL.Path, "/api/v2/") || c.Path() == "/mcp") && c.Request().Header.Get(echo.HeaderAuthorization) != ""
		if claims, ok := hostedSharedClaims(c); ok && (claims.Kind == cloudassert.KindService || claims.Kind == cloudassert.KindKey) {
			return next(c)
		}
		if !hostedReadRequest(c) && !bearerAPI && !strings.HasSuffix(c.Path(), "/redeem") && !s.hostedCSRFValid(c) {
			return s.refusedAPIError(c, http.StatusForbidden, "invalid_csrf", "Reload the form and try again", apiErrorResponse{Code: "invalid_csrf", Message: "Reload the form and try again"})
		}
		if !hostedReadRequest(c) && !bearerAPI {
			s.hostedMutationMu.Lock()
			defer s.hostedMutationMu.Unlock()
			c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), hostedMutationContext{}, true)))
		}
		return next(c)
	}
}

func (s *Service) requireHostedProject(ctx context.Context, query nativeQueryer, scope nativeScope, write bool) error {
	return s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, write) })
}

func (s *Service) hostedMutationIdentity(ctx context.Context, credential apiCredential) (auth.HostedIdentity, auth.Membership, error) {
	identity, membership, err := s.requestAuth().MutationIdentity(ctx, authCredential(credential))
	return identity, membership, fromFeatureError(err)
}

type hostedExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Service) hostedAudit(ctx context.Context, identity *auth.HostedIdentity, event, route, project string, status int) error {
	return s.recordAccessAudit(ctx, identity, event, route, project, status)
}
