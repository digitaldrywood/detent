package hubauth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Authenticator) AuthenticateRequest(ctx context.Context, c echo.Context) (Credential, int, error) {
	if credential, ok := s.deps.BoardCredential(ctx); ok {
		return credential, http.StatusOK, nil
	}
	if s.deps.Hosted == nil && (c.Request().Header.Get(echo.HeaderAuthorization) == "" || strings.HasPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Basic ")) {
		credential, status, err := s.LocalCredential(ctx, c)
		if status == http.StatusUnauthorized {
			c.Response().Header().Set("WWW-Authenticate", `Basic realm="Detent Hub", charset="UTF-8"`)
		}
		return credential, status, err
	}
	if claims, ok := s.deps.SharedClaims(c); ok && claims.Kind == cloudassert.KindKey {
		return s.EntryKeyCredential(c.Request().Context(), claims)
	}
	if s.deps.Hosted != nil && c.Request().Header.Get(echo.HeaderAuthorization) == "" {
		return s.HostedCredential(c.Request().Context(), c)
	}
	token, err := BearerToken(c)
	if err != nil {
		return Credential{}, http.StatusUnauthorized, err
	}
	renewalRunner, renewalOrganization := "", ""
	if c.Request().Method == http.MethodPost && c.Path() == runnerBase+"/:runner/renew" {
		renewalRunner, renewalOrganization = c.Param("runner"), c.Param("organization")
	}
	organization := c.Param("organization")
	if organization == "" && c.Path() == "/api/v2/capabilities" {
		organization = strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization"))
		if organization == "" && s.deps.Hosted != nil {
			organization = s.deps.Hosted.OrganizationID
		}
	}
	if s.deps.Repository != nil && organization != "" {
		if hinted := strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization")); hinted != "" && hinted != organization {
			return Credential{}, http.StatusUnauthorized, fmt.Errorf("%w: organization scope does not match request", operatortool.ErrAccessDenied)
		}
		if s.deps.RunnerEventRoute != nil && s.deps.RunnerEventRoute(c) {
			ctx = s.deps.RunnerEventContext(ctx)
		}
		return s.AuthenticateRepositoryAPIHash(ctx, s.deps.Repository, tracker.OrganizationID(organization), apikey.HashToken(token), renewalRunner, renewalOrganization)
	}
	return s.AuthenticateAccessHash(ctx, apikey.HashToken(token), renewalRunner, renewalOrganization)
}

func BearerToken(c echo.Context) (string, error) {
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
